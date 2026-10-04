package plugin

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr"
	"github.com/abdallah/session-hub/internal/move"
	"github.com/abdallah/session-hub/internal/resume"
)

// Mover timing.
const (
	moveExitWait     = 30 * time.Second // how long Claude may take to leave the pane after /exit
	movePollEvery    = 500 * time.Millisecond
	moveResultTries  = 5
	moveResultPause  = 2 * time.Second // grows by this much after each failed try
	moveArchiveTries = 3
	// moveDoneFor is how long the target keeps posting a done the sessionhub did not
	// record while Claude still runs here: store.MoveStepTTL, after which the
	// move times out in unpacking.
	moveDoneFor   = 10 * time.Minute
	moveDonePause = 30 * time.Second // the longest pause between those posts
	// moveCloudWait is how long claude --cloud may take to print its link.
	moveCloudWait = 2 * time.Minute
	exitCommand   = "/exit"
)

// detailNoMover is why a watcher without a move key fails a move step.
const detailNoMover = "this watcher has no move key; see watcher.log"

// detailResumes ends the detail of a source step that failed after /exit.
// The source restarts the session once the sessionhub has recorded the failure.
const detailResumes = "; the session resumes here"

// mover runs this machine's part of a move: packing and handing off on the
// source (move-out while packing), unpacking on the target (move-in), and
// the source's finish (move-out once the move ended). See docs/plugin.md,
// "Moves".
type mover struct {
	socket    string // the herdr socket
	claudeDir string // ~/.claude, or $CLAUDE_CONFIG_DIR
	stateDir  string // sessionhub's state dir; moved/ holds archives
	roots     []string
	claudeBin string
	key       *ecdh.PrivateKey
	log       *log.Logger
	now       func() time.Time
	// Waits. Tests shorten them.
	exitWait, pollEvery time.Duration
	start               resume.ControlOptions // PollEvery and PollFor of StartResumed
	resultTries         int
	resultPause         time.Duration // also the pause between archive tries
	archiveTries        int
	doneFor             time.Duration // see moveDoneFor
	cloudWait           time.Duration // how long claude --cloud may take to print its link
}

func newMover(socket, claudeDir, stateDir string, roots []string, key *ecdh.PrivateKey, logger *log.Logger) *mover {
	return &mover{socket: socket, claudeDir: claudeDir, stateDir: stateDir, roots: roots, claudeBin: "claude", key: key,
		log: logger, now: time.Now, exitWait: moveExitWait, pollEvery: movePollEvery,
		start: resume.ControlOptions{PollFor: controlPollFor}, resultTries: moveResultTries, resultPause: moveResultPause,
		archiveTries: moveArchiveTries, doneFor: moveDoneFor, cloudWait: moveCloudWait}
}

// stepError is a failed step: the step's name and why.
type stepError struct {
	step string
	err  error
}

func (e *stepError) Error() string { return e.step + ": " + e.err.Error() }

func stepf(step, format string, args ...any) *stepError {
	return &stepError{step: step, err: fmt.Errorf(format, args...)}
}

// failed is the result for a failed step.
func failed(err error) api.MoveResultIn {
	return api.MoveResultIn{State: api.MoveFailed, Detail: cleanErr(err)}
}

// cleanErr is err's text made safe for a move detail or watcher.log: server
// and git messages may carry terminal escapes, and a URL's credentials are
// removed.
func cleanErr(err error) string {
	return termtext.Clean(move.RedactURLs(fmt.Sprint(err)), api.MaxMoveDetailRunes)
}

// handle runs one move claim. Each part posts its own results.
func (m *mover) handle(ctx context.Context, cl *client.Client, claim api.ControlClaim) {
	mv := claim.Move
	if mv == nil {
		m.log.Printf("move: claim %s has no move", claim.Request.ID)
		return
	}
	switch {
	case claim.Request.Action == api.ActionMoveOut && mv.State == api.MovePacking:
		m.moveOut(ctx, cl, claim)
	case claim.Request.Action == api.ActionMoveOut:
		m.finish(ctx, cl, claim)
	case claim.Request.Action == api.ActionMoveIn:
		m.moveIn(ctx, cl, claim)
	}
}

// moveOut is the source's part: steps 1 to 6 of the spec. Every check that
// can fail while the session runs comes before /exit; a step that fails
// after it goes through abort. A pack that uploaded its bundle posts
// nothing: the upload moved the move on.
func (m *mover) moveOut(ctx context.Context, cl *client.Client, claim api.ControlClaim) {
	s, mv := claim.Session, claim.Move
	m.log.Printf("move: %s: moving session %s to %s", mv.ID, s.ID, termtext.Clean(mv.Target, 64))
	fail := func(err error) { m.postResult(ctx, cl, mv.ID, failed(err)) }
	if _, err := m.idlePane(s); err != nil {
		fail(&stepError{"check pane", err})
		return
	}
	repo, err := move.ReadRepo(ctx, s.CWD, m.roots)
	if err != nil {
		fail(&stepError{"read repo", err})
		return
	}
	if mv.Target == api.MoveCloud {
		if !move.IsGitHub(repo.Remote) {
			fail(stepf("cloud", "the remote %s is not on GitHub", move.NormalizeRemote(repo.Remote)))
			return
		}
		m.toCloud(ctx, cl, claim, repo)
		return
	}
	peer, err := move.ParsePublicKey(mv.PeerKey)
	if err != nil {
		fail(stepf("seal", "the target's key: %v", err))
		return
	}
	version, err := move.ClaudeVersion(ctx, m.claudeBin)
	if err != nil {
		fail(&stepError{"build bundle", err})
		return
	}
	in := move.BuildInput{MoveID: mv.ID, SessionID: s.ID, Machine: mv.Source, CWD: s.CWD,
		ClaudeDir: m.claudeDir, ClaudeVersion: version, Repo: repo, Now: m.now()}
	// A trial build finds a missing transcript or a size limit while the
	// session still runs. The real build follows /exit.
	if _, _, err := move.Build(ctx, in); err != nil {
		fail(&stepError{"build bundle", err})
		return
	}
	if err := move.PushBranch(ctx, repo); err != nil {
		fail(&stepError{"push", err})
		return
	}
	// A push can take minutes: a move that timed out meanwhile must not end
	// the session.
	if cur, err := cl.GetMove(context.WithoutCancel(ctx), mv.ID); err != nil {
		fail(stepf("end session", "the move can't be read: %s", cleanErr(err)))
		return
	} else if cur.State != api.MovePacking {
		m.log.Printf("move: %s: the move is %s, so session %s keeps running", mv.ID, termtext.Clean(cur.State, 40), s.ID)
		return
	}
	if !m.endIdle(ctx, cl, s, mv.ID) {
		return
	}
	in.Now = m.now()
	data, man, err := move.Build(ctx, in)
	if err != nil {
		m.abort(ctx, cl, s, mv.ID, &stepError{"build bundle", err})
		return
	}
	sealed, err := move.Seal(m.key, peer, mv.ID, data)
	if err != nil {
		m.abort(ctx, cl, s, mv.ID, &stepError{"seal", err})
		return
	}
	if err := cl.PutMoveBundle(ctx, mv.ID, bytes.NewReader(sealed), int64(len(sealed))); err != nil {
		// The bundle may have landed even though the answer did not. Only a
		// move the sessionhub still shows packing certainly lacks it.
		cur, gerr := cl.GetMove(context.WithoutCancel(ctx), mv.ID)
		switch {
		case gerr != nil:
			m.log.Printf("move: %s: upload: %s; the move can't be read (%s), so session %s stays ended until the move ends",
				mv.ID, cleanErr(err), cleanErr(gerr), s.ID)
		case cur.State != api.MovePacking:
			m.log.Printf("move: %s: upload answered %s, but the move is %s", mv.ID, cleanErr(err), termtext.Clean(cur.State, 40))
		default:
			m.abort(ctx, cl, s, mv.ID, &stepError{"upload", err})
		}
		return
	}
	m.log.Printf("move: %s: uploaded %d bytes; skipped %d files", mv.ID, len(sealed), len(man.Skipped))
}

// toCloud is the cloud part of move-out. Everything that can fail without
// the cloud runs while the session still runs: the push, the Claude Code
// version, and the sessionhub-owned branch with the uncommitted work. Then it ends
// the session, starts a cloud session with the hand-off prompt, and reports
// done with its link and the files it left out.
func (m *mover) toCloud(ctx context.Context, cl *client.Client, claim api.ControlClaim, repo move.Repo) {
	s, mv := claim.Session, claim.Move
	until := m.now().Add(m.doneFor)
	fail := func(err error) { m.postResult(ctx, cl, mv.ID, failed(err)) }
	if err := move.PushBranch(ctx, repo); err != nil {
		fail(&stepError{"push", err})
		return
	}
	if _, err := move.ClaudeVersion(ctx, m.claudeBin); err != nil {
		fail(&stepError{"start cloud", err})
		return
	}
	branch, skipped, err := move.PushCloudBranch(ctx, repo, s.ID)
	if err != nil {
		fail(&stepError{"cloud branch", err})
		return
	}
	// The pushes can take minutes: a move that timed out meanwhile must not
	// end the session.
	if cur, err := cl.GetMove(context.WithoutCancel(ctx), mv.ID); err != nil {
		fail(stepf("end session", "the move can't be read: %s", cleanErr(err)))
		return
	} else if cur.State != api.MovePacking {
		m.log.Printf("move: %s: the move is %s, so session %s keeps running", mv.ID, termtext.Clean(cur.State, 40), s.ID)
		return
	}
	if !m.endIdle(ctx, cl, s, mv.ID) {
		return
	}
	link, err := move.StartCloud(ctx, m.claudeBin, s.CWD, move.HandOffPrompt(s, mv.Source, branch), m.cloudWait)
	var started *move.CloudStartedError
	if err != nil && !errors.As(err, &started) {
		m.abort(ctx, cl, s, mv.ID, &stepError{"start cloud", err})
		return
	}
	// A cloud session exists, or may: mark it before posting, so the finish
	// of a move that ends without this done never restarts the session here.
	if err := m.markCloud(mv.ID, link); err != nil {
		m.log.Printf("move: %s: mark the cloud session: %s", mv.ID, cleanErr(err))
	}
	if started != nil {
		m.log.Printf("move: %s: start cloud: %s; session %s is not restarted here", mv.ID, cleanErr(err), s.ID)
		fail(stepf("start cloud", "%v; not restarted here", err))
		return
	}
	in := api.MoveResultIn{State: api.MoveDone, CloudURL: link}
	if len(skipped) > 0 {
		in.Detail = "not carried: " + strings.Join(skipped, ", ")
	}
	m.log.Printf("move: %s: session %s handed to the cloud on branch %s: %s", mv.ID, s.ID, branch, link)
	m.keepCloudDone(ctx, cl, mv.ID, in, until, s.ID)
}

// keepCloudDone posts a cloud move's done, with growing pauses, until the
// sessionhub records it, the move ends otherwise, or until passes. The cloud
// session runs either way; the marker (markCloud) keeps the finish from
// restarting the session here.
func (m *mover) keepCloudDone(ctx context.Context, cl *client.Client, id string, in api.MoveResultIn, until time.Time, sid string) {
	for try := 1; ; try++ {
		if m.postResult(ctx, cl, id, in) {
			return
		}
		cur, err := cl.GetMove(context.WithoutCancel(ctx), id)
		switch {
		case err == nil && cur.State == api.MoveDone:
			return
		case err == nil && cur.State != api.MovePacking:
			m.log.Printf("move: %s: the move is %s, but the cloud session %s runs; session %s is not restarted here",
				id, termtext.Clean(cur.State, 40), in.CloudURL, sid)
			return
		case !m.now().Before(until):
			m.log.Printf("move: %s: done still not recorded; the cloud session %s runs, and session %s is not restarted here",
				id, in.CloudURL, sid)
			return
		}
		if !sleepCtx(ctx, min(m.resultPause*time.Duration(try), moveDonePause)) {
			return
		}
	}
}

// cloudMarkPath is the marker of a move that started, or may have started,
// a cloud session: <state dir>/moved/<move id>/cloud. ok is false for an ID
// that is not one path element.
func (m *mover) cloudMarkPath(id string) (string, bool) {
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id {
		return "", false
	}
	return filepath.Join(m.stateDir, "moved", id, "cloud"), true
}

// markCloud records that move id started a cloud session at link ("" when
// the link is unknown).
func (m *mover) markCloud(id, link string) error {
	p, ok := m.cloudMarkPath(id)
	if !ok {
		return fmt.Errorf("invalid move ID %q", termtext.Clean(id, 40))
	}
	if link == "" {
		link = "unknown"
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(link+"\n"), 0o600)
}

// cloudMark reports whether move id marked a cloud session, and its link or
// "link unknown".
func (m *mover) cloudMark(id string) (string, bool) {
	p, ok := m.cloudMarkPath(id)
	if !ok {
		return "", false
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false
	}
	if link := strings.TrimSpace(string(b)); err == nil && move.ValidCloudLink(link) {
		return link, true
	}
	return "link unknown", true
}

// endIdle checks again that the session's pane is idle, since a push can
// take minutes, and ends Claude with /exit. It reports whether the session
// ended; when it did not, it posted the failure.
func (m *mover) endIdle(ctx context.Context, cl *client.Client, s api.Session, id string) bool {
	pane, err := m.idlePane(s)
	if err != nil {
		m.postResult(ctx, cl, id, failed(&stepError{"check pane", err}))
		return false
	}
	if err := m.endSession(ctx, pane, s.ID, true); err != nil {
		m.abort(ctx, cl, s, id, &stepError{"end session", err})
		return false
	}
	return true
}

// abort handles a source step that failed after /exit. It posts the failure
// and restarts the session here only once the sessionhub recorded it. When the
// answer is lost, the move is read back: a move that failed with this
// detail was recorded after all (the answer was lost after the commit, and
// the retry got 409), so the session restarts. A failure the sessionhub did not
// take (the move ended meanwhile, the upload landed after all, or the sessionhub is
// out of reach) leaves the session ended, and the move's finish restarts it,
// so it never runs on two machines.
func (m *mover) abort(ctx context.Context, cl *client.Client, s api.Session, id string, err error) {
	in := failed(errors.New(err.Error() + detailResumes))
	in.Detail = termtext.Clean(in.Detail, api.MaxMoveDetailRunes)
	if !m.postResult(ctx, cl, id, in) {
		cur, gerr := cl.GetMove(context.WithoutCancel(ctx), id)
		if gerr != nil || cur.State != api.MoveFailed || !strings.HasPrefix(cur.Detail, in.Detail) {
			m.log.Printf("move: %s: session %s stays ended until the move ends", id, s.ID)
			return
		}
		m.log.Printf("move: %s: the sessionhub recorded the failure after all", id)
	}
	m.restart(ctx, cl, s, id, api.MoveFailed)
}

// restart starts session s here again after move id ended in state, unless
// a pane already runs it. A restart that fails is added to the move's
// detail as a note.
func (m *mover) restart(ctx context.Context, cl *client.Client, s api.Session, id, state string) {
	r := resume.StartResumed(context.WithoutCancel(ctx), m.socket, s, m.start)
	var why string
	switch r.Outcome {
	case resume.OutcomeResumed, resume.OutcomeRunning:
		m.log.Printf("move: %s: session %s %s here", id, s.ID, r.Outcome)
		return
	case resume.OutcomeNoDir:
		why = "the directory is gone"
	default:
		why = fmt.Sprint(r.Err)
	}
	m.log.Printf("move: %s: restart session %s: %s", id, s.ID, termtext.Clean(why, api.MaxMoveDetailRunes))
	m.postResult(ctx, cl, id, api.MoveResultIn{State: noteState(state), Detail: "restart failed: " + why})
}

// noteState is the state a source note on a move that ended in state
// carries: the result route takes only done or failed, and every reader
// treats cancelled like failed.
func noteState(state string) string {
	if state == api.MoveCancelled {
		return api.MoveFailed
	}
	return state
}

// finish is the source's last step, once the move ended: archive the
// transcript after a machine move's done (with retries), or restart the
// session after a failure. A problem here is added to the move's detail.
func (m *mover) finish(ctx context.Context, cl *client.Client, claim api.ControlClaim) {
	s, mv := claim.Session, claim.Move
	var mark string
	var marked bool
	if mv.Target == api.MoveCloud {
		mark, marked = m.cloudMark(mv.ID)
	}
	switch {
	case mv.State == api.MoveDone && mv.Target == api.MoveCloud:
		m.log.Printf("move: %s: done in the cloud; the transcript stays here", mv.ID)
	case mv.State == api.MoveDone:
		var err error
		for try := 1; ; try++ {
			if err = move.Archive(m.claudeDir, m.stateDir, s.ID, mv.ID, m.now()); err == nil || try >= m.archiveTries {
				break
			}
			sleepCtx(context.WithoutCancel(ctx), m.resultPause*time.Duration(try))
		}
		if err != nil {
			m.log.Printf("move: %s: archive session %s: %s; remove its transcript by hand so it is not resumed twice", mv.ID, s.ID, cleanErr(err))
			m.postResult(ctx, cl, mv.ID, api.MoveResultIn{State: api.MoveDone, Detail: archiveNote(mv.Source, err)})
			return
		}
		m.log.Printf("move: %s: done; session %s archived under moved/%s", mv.ID, s.ID, mv.ID)
	case marked:
		note := "cloud session started (" + mark + "); not restarted here"
		m.log.Printf("move: %s: %s (%s); %s", mv.ID, mv.State, termtext.Clean(mv.Detail, 200), note)
		m.postResult(ctx, cl, mv.ID, api.MoveResultIn{State: noteState(mv.State), Detail: note})
	default:
		m.log.Printf("move: %s: %s (%s); restarting session %s", mv.ID, mv.State, termtext.Clean(mv.Detail, 200), s.ID)
		m.restart(ctx, cl, s, mv.ID, mv.State)
	}
}

// archiveNoteHint ends the note of an archive that failed.
const archiveNoteHint = "; remove the transcript there by hand"

// archiveNote is the note for an archive on machine that failed with err. The
// error is cut first, so the hint always fits in api.MaxMoveDetailRunes.
func archiveNote(machine string, err error) string {
	head := "archive on " + termtext.Clean(machine, 64) + " failed: "
	room := api.MaxMoveDetailRunes - utf8.RuneCountInString(head) - utf8.RuneCountInString(archiveNoteHint)
	return head + termtext.Clean(err.Error(), room) + archiveNoteHint
}

// idlePane returns the pane where Claude runs session s, idle or done.
func (m *mover) idlePane(s api.Session) (string, error) {
	if s.HerdrPane == "" {
		return "", errors.New(detailNotInHerdr)
	}
	if s.HerdrSession != "" && s.HerdrSession != herdr.SessionName(m.socket) {
		return "", errors.New("the session's pane is on another herdr server")
	}
	h, err := herdr.Dial(m.socket)
	if err != nil {
		return "", errors.New(detailHerdrDown)
	}
	defer h.Close()
	p, err := h.PaneGet(s.HerdrPane)
	var he *herdr.Error
	switch {
	case errors.As(err, &he) && he.Code == "pane_not_found":
		return "", errors.New(detailPaneGone)
	case err != nil:
		return "", errors.New(detailHerdrDown)
	case p.Agent == "":
		return "", errors.New(detailNoAgent)
	case p.SessionID() == "":
		return "", errors.New(detailNoSessionID)
	case p.SessionID() != s.ID:
		return "", errors.New(detailOtherSession)
	}
	switch p.AgentStatus {
	case "idle", "done":
		return p.PaneID, nil
	case "":
		return "", errors.New("the agent's status is unknown")
	}
	return "", errors.New("the agent is " + termtext.Clean(p.AgentStatus, 40))
}

// endSession types /exit into pane and waits up to exitWait for Claude to
// leave it: the pane is gone, runs no agent, or runs another session. With
// id empty (a pane this watcher started, before herdr reports the session),
// only the agent leaving counts. A pane with no agent counts only once
// Claude was seen there: seen says the caller saw it, and a poll here that
// shows an agent counts too. Otherwise "no agent" may only mean herdr has not
// detected Claude yet.
func (m *mover) endSession(ctx context.Context, pane, id string, seen bool) error {
	h, err := herdr.Dial(m.socket)
	if err != nil {
		return errors.New(detailHerdrDown)
	}
	defer h.Close()
	if err := h.AgentPrompt(pane, exitCommand); err != nil {
		var be *herdr.BlockedError
		if errors.As(err, &be) {
			return errors.New(detailBlocked)
		}
		return fmt.Errorf("send %s: %w", exitCommand, err)
	}
	deadline := time.Now().Add(m.exitWait)
	for {
		p, err := h.PaneGet(pane)
		var he *herdr.Error
		// Claude that herdr shows with no session ID has not left: herdr
		// reports that briefly, and uploading then would run the session in
		// two places.
		if err == nil && p.Agent != "" {
			seen = true
		}
		if errors.As(err, &he) && he.Code == "pane_not_found" ||
			err == nil && (p.Agent == "" && seen || id != "" && p.SessionID() != "" && p.SessionID() != id) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("Claude was still in the pane %s after %s", termtext.Clean(pane, 64), m.exitWait)
		}
		if !sleepCtx(ctx, m.pollEvery) {
			return ctx.Err()
		}
	}
}

// postResult posts a move result and reports whether the sessionhub recorded it. A
// network error or 5xx is retried up to resultTries times; a 4xx (the move
// ended meanwhile, or it is not this machine's step) is logged.
func (m *mover) postResult(ctx context.Context, cl *client.Client, id string, in api.MoveResultIn) bool {
	in.Detail = termtext.Clean(in.Detail, api.MaxMoveDetailRunes)
	pctx := context.WithoutCancel(ctx)
	for try := 1; ; try++ {
		err := cl.PostMoveResult(pctx, id, in)
		if err == nil {
			m.log.Printf("move: %s: %s %s", id, in.State, in.Detail)
			return true
		}
		var se *client.StatusError
		if errors.As(err, &se) && se.Status < 500 || try >= m.resultTries {
			m.log.Printf("move: %s: result %s not recorded: %s", id, in.State, cleanErr(err))
			return false
		}
		sleepCtx(pctx, m.resultPause*time.Duration(try))
	}
}

// moveIn is the target's part: download and open the bundle, check the
// Claude Code version and that the session is new here, find a clean clone
// with the same remote, check out the source's HEAD, apply the changes,
// write the transcript, and start Claude while the move is still open.
// Every check that needs no change runs before the clone is touched; a
// failure after the checkout takes back what the move did, and nothing
// else.
func (m *mover) moveIn(ctx context.Context, cl *client.Client, claim api.ControlClaim) {
	mv := claim.Move
	until := m.now().Add(m.doneFor)
	m.log.Printf("move: %s: taking session %s from %s", mv.ID, termtext.Clean(claim.Session.ID, 64), termtext.Clean(mv.Source, 64))
	fail := func(err error) { m.postResult(ctx, cl, mv.ID, failed(err)) }
	var buf bytes.Buffer
	if _, err := cl.GetMoveBundle(ctx, mv.ID, &buf); err != nil {
		fail(&stepError{"download", err})
		return
	}
	peer, err := move.ParsePublicKey(mv.PeerKey)
	if err != nil {
		fail(stepf("open", "the source's key: %v", err))
		return
	}
	plain, err := move.Open(m.key, peer, mv.ID, buf.Bytes())
	if err != nil {
		fail(&stepError{"open", err})
		return
	}
	b, err := move.Extract(plain)
	if err != nil {
		fail(&stepError{"open", err})
		return
	}
	man := b.Manifest
	if man.MoveID != mv.ID || man.SessionID != claim.Session.ID || man.SourceMachine != mv.Source {
		fail(stepf("open", "the bundle's manifest does not match the move"))
		return
	}
	version, err := move.ClaudeVersion(ctx, m.claudeBin)
	if err != nil {
		fail(&stepError{"claude version", err})
		return
	}
	if version != man.ClaudeVersion {
		fail(stepf("claude version", "Claude Code %s here, %s on %s; run the same version on both", version, man.ClaudeVersion, mv.Source))
		return
	}
	if ts, err := move.FindTranscripts(m.claudeDir, man.SessionID); err != nil {
		fail(&stepError{"transcript", err})
		return
	} else if len(ts) > 0 {
		fail(stepf("transcript", "session %s already has a transcript here: %s", man.SessionID, ts[0]))
		return
	}
	clone, err := move.FindClone(ctx, m.roots, man.Remote, man.RootRel)
	if errors.Is(err, move.ErrNoClone) {
		fail(stepf("find clone", "%v on %s", err, mv.Target))
		return
	}
	if err != nil {
		fail(&stepError{"find clone", err})
		return
	}
	carried := append(slices.Clone(man.Untracked), move.PatchNewFiles(b.Patch)...)
	if err := move.CheckClone(ctx, clone, carried); err != nil {
		fail(&stepError{"check clone", err})
		return
	}
	if err := m.cloneFree(ctx, clone, man.Branch); err != nil {
		fail(&stepError{"check clone", err})
		return
	}
	co, err := move.PrepareClone(ctx, clone, man.Branch, man.Head)
	if err != nil {
		fail(&stepError{"checkout", err})
		return
	}
	var written []string
	// undo removes the transcript it wrote and takes back what the move did
	// to the clone; the note says how that went, cleaned.
	undo := func() string {
		move.RemoveFiles(written)
		if err := co.Undo(context.WithoutCancel(ctx)); err != nil {
			return "; putting the clone back failed: " + cleanErr(err)
		}
		return "; the clone is back on " + termtext.Clean(co.Prev(), 64)
	}
	// abort undoes and posts the failure. The error is cut first, so the
	// note about the clone always fits in the detail.
	abort := func(err error) {
		note := undo()
		room := max(api.MaxMoveDetailRunes-utf8.RuneCountInString(note), 60)
		detail := termtext.Clean(err.Error(), room) + note
		m.log.Printf("move: %s: %s", mv.ID, detail)
		m.postResult(ctx, cl, mv.ID, api.MoveResultIn{State: api.MoveFailed, Detail: detail})
	}
	if err := co.Apply(ctx, b); err != nil {
		abort(&stepError{"apply", err})
		return
	}
	// The session's directory, with links resolved, must be in the clone: a
	// linked folder there must not start Claude or file its transcript
	// elsewhere.
	cwd, err := filepath.EvalSymlinks(filepath.Join(clone, filepath.FromSlash(man.RelPath)))
	if err != nil {
		abort(stepf("apply", "the session's directory %s is missing", filepath.Join(clone, filepath.FromSlash(man.RelPath))))
		return
	}
	if realClone, err := filepath.EvalSymlinks(clone); err != nil ||
		cwd != realClone && !strings.HasPrefix(cwd, realClone+string(filepath.Separator)) {
		abort(stepf("unpack", "%s leaves the clone", man.RelPath))
		return
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		abort(stepf("apply", "the session's directory %s is missing", cwd))
		return
	}
	if written, err = move.WriteSession(m.claudeDir, cwd, b); err != nil {
		abort(&stepError{"write transcript", err})
		return
	}
	// Start only while the move is open. Once it ended (a timeout), the
	// source restarts the session, so it must not start here too.
	if cur, err := cl.GetMove(ctx, mv.ID); err != nil || cur.State != api.MoveUnpacking {
		why := fmt.Sprint(err)
		if err == nil {
			why = "the move is " + cur.State
		}
		abort(stepf("start", "not starting: %s", why))
		return
	}
	s := api.Session{ID: man.SessionID, CWD: cwd, Title: claim.Session.Title}
	r := resume.StartResumed(ctx, m.socket, s, m.start)
	if r.Outcome != resume.OutcomeResumed {
		why := string(r.Outcome)
		if r.Err != nil {
			why = r.Err.Error()
		}
		// Claude may be in the pane. If /exit does not end it, the session
		// may run here, so it stays here: done, with the files kept, and the
		// source archives instead of restarting.
		if r.Pane != "" && (r.Outcome == resume.OutcomeError || r.Outcome == resume.OutcomeRunning) {
			if err := m.stopClaude(ctx, mv.ID, r); err != nil {
				in := api.MoveResultIn{State: api.MoveDone, Detail: fmt.Sprintf("start: %s; Claude may be running in pane %s on %s; check it there",
					termtext.Clean(why, 120), termtext.Clean(r.Pane, 64), termtext.Clean(mv.Target, 64))}
				m.keepDone(ctx, cl, mv.ID, in, until, r.Pane, mv.Source)
				return
			}
		}
		abort(stepf("start", "%s", why))
		return
	}
	in := api.MoveResultIn{State: api.MoveDone}
	if len(man.Skipped) > 0 {
		in.Detail = "not carried: " + strings.Join(man.Skipped, ", ")
	}
	if m.postResult(ctx, cl, mv.ID, in) {
		m.log.Printf("move: %s: session %s resumed in %s", mv.ID, man.SessionID, termtext.Clean(cwd, 200))
		return
	}
	// The sessionhub did not record done. Unless the move reads back done, the
	// source gets the session back, so Claude must not stay here.
	if cur, err := cl.GetMove(context.WithoutCancel(ctx), mv.ID); err == nil && cur.State == api.MoveDone {
		m.log.Printf("move: %s: session %s resumed in %s", mv.ID, man.SessionID, termtext.Clean(cwd, 200))
		return
	}
	// When /exit fails, Claude still runs here: taking its files away would
	// not stop it, so they stay, and done is posted again until the move
	// would time out.
	if err := m.stopClaude(ctx, mv.ID, r); err != nil {
		m.log.Printf("move: %s: done was not recorded, and session %s still runs here in pane %s; end it before the move times out or it also restarts on %s",
			mv.ID, man.SessionID, termtext.Clean(r.Pane, 64), termtext.Clean(mv.Source, 64))
		m.keepDone(ctx, cl, mv.ID, in, until, r.Pane, mv.Source)
		return
	}
	m.log.Printf("move: %s: done was not recorded, so session %s stopped here%s", mv.ID, man.SessionID, undo())
}

// cloneFree fails when the checkout would switch clone to branch while an
// agent in a herdr pane here works in the clone: its next commit would land
// on the moved branch. A pane whose directory no longer resolves is skipped.
func (m *mover) cloneFree(ctx context.Context, clone, branch string) error {
	cur, err := move.CurrentBranch(ctx, clone)
	if err != nil {
		return err
	}
	if cur == branch {
		return nil
	}
	realClone, err := filepath.EvalSymlinks(clone)
	if err != nil {
		return err
	}
	h, err := herdr.Dial(m.socket)
	if err != nil {
		return errors.New(detailHerdrDown)
	}
	defer h.Close()
	snap, err := h.Snapshot()
	if err != nil {
		return fmt.Errorf("read herdr snapshot: %w", err)
	}
	for _, p := range snap.Panes {
		if p.Agent == "" {
			continue
		}
		for _, dir := range []string{p.CWD, p.ForegroundCWD} {
			if dir == "" {
				continue
			}
			d, err := filepath.EvalSymlinks(dir)
			if err == nil && (d == realClone || strings.HasPrefix(d, realClone+string(filepath.Separator))) {
				return fmt.Errorf("%s is in use by a session in pane %s", clone, termtext.Clean(p.PaneID, 64))
			}
		}
	}
	return nil
}

// keepDone posts done for a session that may run in pane here, with growing
// pauses, until the sessionhub records it, the move reads back done, the move ends
// another way, or until passes. It never removes a file: Claude may use them.
func (m *mover) keepDone(ctx context.Context, cl *client.Client, id string, in api.MoveResultIn, until time.Time, pane, source string) {
	pane, source = termtext.Clean(pane, 64), termtext.Clean(source, 64)
	for try := 1; ; try++ {
		if m.postResult(ctx, cl, id, in) {
			return
		}
		cur, err := cl.GetMove(context.WithoutCancel(ctx), id)
		switch {
		case err == nil && cur.State == api.MoveDone:
			return
		case err == nil && cur.State != api.MoveUnpacking:
			m.log.Printf("move: %s: the move is %s, so the session restarts on %s; end Claude in pane %s here by hand",
				id, termtext.Clean(cur.State, 40), source, pane)
			return
		case !m.now().Before(until):
			m.log.Printf("move: %s: done still not recorded; end Claude in pane %s here by hand before the session restarts on %s",
				id, pane, source)
			return
		}
		if !sleepCtx(ctx, min(m.resultPause*time.Duration(try), moveDonePause)) {
			return
		}
	}
}

// stopClaude ends Claude in the pane of r, which StartResumed returned. An
// error means Claude may still run there. In a pane that StartResumed
// created (a new workspace), /exit is only a courtesy: the pane closes, and
// Claude counts as stopped only once herdr no longer has the pane, so a
// Claude that herdr never detected stops too. In another pane, /exit must
// end the Claude that herdr showed there.
func (m *mover) stopClaude(ctx context.Context, id string, r resume.ControlResult) error {
	ctx = context.WithoutCancel(ctx)
	// OutcomeResumed and OutcomeRunning mean herdr showed Claude in the pane.
	seen := r.Outcome == resume.OutcomeResumed || r.Outcome == resume.OutcomeRunning
	var err error
	if r.Workspace == "" {
		err = m.endSession(ctx, r.Pane, "", seen)
	} else {
		if seen {
			if xerr := m.endSession(ctx, r.Pane, "", true); xerr != nil {
				m.log.Printf("move: %s: /exit in pane %s: %s; closing the pane", id, termtext.Clean(r.Pane, 64), cleanErr(xerr))
			}
		}
		err = m.closePane(ctx, r.Pane)
	}
	if err != nil {
		m.log.Printf("move: %s: end Claude in pane %s: %s; end it by hand", id, termtext.Clean(r.Pane, 64), cleanErr(err))
	}
	return err
}

// closePane closes pane and waits up to exitWait until herdr no longer has
// it. Any other outcome is an error: the pane, and Claude in it, may remain.
func (m *mover) closePane(ctx context.Context, pane string) error {
	h, err := herdr.Dial(m.socket)
	if err != nil {
		return errors.New(detailHerdrDown)
	}
	defer h.Close()
	var he *herdr.Error
	if err := h.PaneClose(pane); err != nil && !(errors.As(err, &he) && he.Code == "pane_not_found") {
		return fmt.Errorf("close pane %s: %w", termtext.Clean(pane, 64), err)
	}
	deadline := time.Now().Add(m.exitWait)
	for {
		_, err := h.PaneGet(pane)
		if errors.As(err, &he) && he.Code == "pane_not_found" {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("pane %s was still open %s after closing it", termtext.Clean(pane, 64), m.exitWait)
		}
		if !sleepCtx(ctx, m.pollEvery) {
			return ctx.Err()
		}
	}
}
