package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/digest"
	"github.com/abdallah/session-hub/internal/herdr"
	"github.com/abdallah/session-hub/internal/move"
)

// Watcher timing.
const (
	passInterval      = 2 * time.Second
	passBudget        = 1 * time.Second // total send time per pass, under the queue lock
	heartbeatInterval = 60 * time.Second
	unresolvedTTL     = 10 * time.Minute // unresolvable pane items are dropped after this
	cacheTTL          = 10 * time.Minute // panes missing from snapshots are forgotten after this
	gitTTL            = 60 * time.Second
	repeatLogEvery    = 5 * time.Minute // an identical failure line is repeated at most this often
)

// snapshotter is the herdr call the watcher needs (*herdr.Client).
type snapshotter interface {
	Snapshot() (herdr.Snapshot, error)
}

// workspaceReporter is the herdr call the sidebar count needs
// (*herdr.Client).
type workspaceReporter interface {
	ReportWorkspaceMetadata(p herdr.WorkspaceMetadataParams) error
}

// inboxToken is the workspace token that carries the inbox count. Show it
// with $inbox in [ui.sidebar.spaces] rows.
const inboxToken = "inbox"

// inboxText is the sidebar text for the inbox counts: the total, plus
// " · n blocked" when any are blocked. "" means clear the token.
func inboxText(c api.InboxCounts) string {
	total := c.Blocked + c.Waiting + c.Finished
	switch {
	case total == 0:
		return ""
	case c.Blocked > 0:
		return fmt.Sprintf("%d · %d blocked", total, c.Blocked)
	}
	return strconv.Itoa(total)
}

// workspaceIDs lists the snapshot's workspaces, in snapshot order.
func workspaceIDs(snap herdr.Snapshot) []string {
	out := make([]string, 0, len(snap.Workspaces))
	for _, ws := range snap.Workspaces {
		if ws.WorkspaceID != "" {
			out = append(out, ws.WorkspaceID)
		}
	}
	return out
}

type cacheEntry struct {
	u    api.SessionUpsert
	seen time.Time
}

// transcriptMark is a transcript's size and modification time.
type transcriptMark struct {
	size int64
	mod  time.Time
}

// maxDigestsPerBeat bounds the digests one heartbeat runs; the rest wait
// for the next heartbeat.
const maxDigestsPerBeat = 10

// digestTimeout bounds one session's digest, including the transcript read,
// so a huge or stuck transcript cannot hold up the watcher's ticks.
const digestTimeout = 15 * time.Second

type gitEntry struct {
	repo, branch string
	at           time.Time
}

// watcher drains the queue and sends heartbeats. All times come from the
// caller (step), so tests drive it with a fake clock.
type watcher struct {
	q            *client.Queue
	herdr        snapshotter
	herdrSession string
	newClient    func() (*client.Client, error)
	git          gitFunc
	log          *log.Logger
	logPath      string // truncated when it grows past maxWatcherLog; "" to skip

	cache        map[string]cacheEntry // pane ID → session
	unidentified []string              // Claude panes without a session ID, from the last snapshot
	gitCache     map[string]gitEntry   // cwd → git info
	lastBeat     time.Time             // last heartbeat attempt
	lastSnap     time.Time             // last snapshot attempt (heartbeat or refresh)
	lastBeatOK   time.Time             // last heartbeat the server accepted
	lastLog      map[string]time.Time  // failure line → when it was last logged

	// digest builds and sends one session's digest; transcriptStat reports
	// its transcript's size and modification time. Tests replace both.
	digest         func(ctx context.Context, id string) error
	transcriptStat func(id string) (size int64, mod time.Time, ok bool)
	digested       map[string]transcriptMark // session ID → transcript at the last good digest
	failed         map[string]time.Time      // session ID → when its digest last failed; cleared on success

	// sidebar writes the inbox count on each workspace; nil turns the count
	// off. newWatcher leaves it nil, runWatcher sets it.
	sidebar     workspaceReporter
	workspaces  []string          // workspace IDs from the last snapshot
	sidebarText map[string]string // workspace ID → inbox text it last accepted
	// instructionsPath is the local rules copy the heartbeat refreshes; ""
	// turns the refresh off. newWatcher leaves it empty, runWatcher sets it.
	instructionsPath string
	// moveKey is this machine's move public key, sent after the first
	// accepted heartbeat and every moveKeyEvery after that; "" sends none.
	moveKey   string
	moveKeyAt time.Time // when the server last took it
}

// moveKeyEvery is how often the watcher registers its move key again, so a
// server restored from a backup learns it back.
const moveKeyEvery = time.Hour

func newWatcher(q *client.Queue, h snapshotter, herdrSession string, newClient func() (*client.Client, error), git gitFunc, logger *log.Logger) *watcher {
	return &watcher{
		q: q, herdr: h, herdrSession: herdrSession, newClient: newClient, git: git, log: logger,
		cache: map[string]cacheEntry{}, gitCache: map[string]gitEntry{}, lastLog: map[string]time.Time{},
		digested: map[string]transcriptMark{}, failed: map[string]time.Time{},
		sidebarText: map[string]string{},
	}
}

// logf writes one line, but repeats an identical line at most every
// repeatLogEvery, so a server that stays down does not fill watcher.log.
func (w *watcher) logf(now time.Time, format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if t, ok := w.lastLog[line]; ok && now.Sub(t) < repeatLogEvery {
		return
	}
	w.lastLog[line] = now
	w.log.Print(line)
}

// step runs one 2 s tick: the heartbeat when due, then a queue pass.
func (w *watcher) step(ctx context.Context, now time.Time) {
	if w.lastBeat.IsZero() || now.Sub(w.lastBeat) >= heartbeatInterval {
		w.heartbeat(ctx, now)
	}
	w.pass(ctx, now)
	w.capLog()
}

func (w *watcher) capLog() {
	if w.logPath == "" {
		return
	}
	if st, err := os.Stat(w.logPath); err == nil && st.Size() > maxWatcherLog {
		os.Truncate(w.logPath, 0) // stderr is O_APPEND, so writes continue at the new end
	}
}

// refresh takes a snapshot and updates the pane cache. It returns this
// machine's full set of Claude panes for a heartbeat.
func (w *watcher) refresh(ctx context.Context, now time.Time) ([]api.SessionUpsert, error) {
	w.lastSnap = now
	snap, err := w.herdr.Snapshot()
	if err != nil {
		return nil, err
	}
	ups := buildSessions(ctx, snap, w.herdrSession, nil)
	w.unidentified = unidentifiedPanes(snap)
	w.workspaces = workspaceIDs(snap)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range ups {
		cwd := ups[i].CWD
		if cwd == "" || w.git == nil {
			continue
		}
		if g, ok := w.gitCache[cwd]; ok && now.Sub(g.at) < gitTTL {
			ups[i].GitRepo, ups[i].GitBranch = g.repo, g.branch
			continue
		}
		wg.Add(1)
		go func(u *api.SessionUpsert) {
			defer wg.Done()
			u.GitRepo, u.GitBranch = w.git(ctx, u.CWD)
			mu.Lock()
			w.gitCache[u.CWD] = gitEntry{u.GitRepo, u.GitBranch, now}
			mu.Unlock()
		}(&ups[i])
	}
	wg.Wait()
	for _, u := range ups {
		w.cache[u.HerdrPane] = cacheEntry{u: u, seen: now}
	}
	// Keep panes that just vanished for a while: a pane_closed event for them
	// is still resolved from here.
	for id, e := range w.cache {
		if now.Sub(e.seen) >= cacheTTL {
			delete(w.cache, id)
		}
	}
	for cwd, g := range w.gitCache {
		if now.Sub(g.at) >= gitTTL {
			delete(w.gitCache, cwd)
		}
	}
	return ups, nil
}

// heartbeat sends the full set of panes (PUT herdr-sessions). A failure is
// logged and not queued: the next heartbeat carries a fresh set anyway.
func (w *watcher) heartbeat(ctx context.Context, now time.Time) {
	w.lastBeat = now
	ups, err := w.refresh(ctx, now)
	if err != nil {
		w.logf(now, "heartbeat: herdr snapshot: %v", err)
		return
	}
	// Digests run after the heartbeat is sent, whether or not the send
	// succeeds (a digest is queued when the server is down), so a slow first
	// read of a large transcript never delays the heartbeat.
	defer w.digests(ctx, now)
	c, err := w.newClient()
	if err != nil {
		w.logf(now, "heartbeat: %v", err)
		return
	}
	res, err := c.PutHerdrSessionsResult(ctx, api.HerdrSessionsPut{HerdrSession: w.herdrSession, Sessions: ups, UnidentifiedPanes: w.unidentified})
	if err != nil {
		w.logf(now, "heartbeat: send failed: %v", err)
		return
	}
	for _, in := range res.Invalid {
		w.logf(now, "heartbeat: server skipped session %q: %s", in.ID, in.Reason)
	}
	w.lastBeatOK = now
	w.reportInbox(ctx, c, now)
	w.refreshInstructions(ctx, c, now)
	w.registerMoveKey(ctx, c, now)
}

// registerMoveKey sends the move public key when the server has not taken it
// in the last moveKeyEvery. A failure is logged and retried at the next
// heartbeat.
func (w *watcher) registerMoveKey(ctx context.Context, c *client.Client, now time.Time) {
	if w.moveKey == "" || (!w.moveKeyAt.IsZero() && now.Sub(w.moveKeyAt) < moveKeyEvery) {
		return
	}
	if err := c.PutMoveKey(ctx, w.moveKey); err != nil {
		w.logf(now, "move: key not registered: %s", cleanErr(err))
		return
	}
	if w.moveKeyAt.IsZero() {
		if raw, err := api.ParseMoveKey(w.moveKey); err == nil {
			w.log.Printf("move: key %s registered", api.MoveKeyFingerprint(raw))
		}
	}
	w.moveKeyAt = now
}

// refreshInstructions rewrites the local rules copy when the server's
// version changed. A failure keeps the old copy; sessions keep seeing it.
// It runs after the heartbeat is sent and never fails it.
func (w *watcher) refreshInstructions(ctx context.Context, c *client.Client, now time.Time) {
	if w.instructionsPath == "" {
		return
	}
	changed, err := client.RefreshInstructions(ctx, c, w.instructionsPath)
	if err != nil {
		w.logf(now, "rules: refresh failed: %v", err)
		return
	}
	if changed {
		w.log.Printf("rules: local copy updated")
	}
}

// reportInbox puts the inbox count on every workspace of this herdr
// session. A failed inbox read keeps what the sidebar shows. A workspace
// whose text is unchanged since it last accepted one gets no call; a failed
// report is retried at the next heartbeat. The first heartbeat reports every
// workspace, a clear included, so a count an older watcher left goes away.
// The heartbeat is already sent when this runs, and the sessionhub client's 2 s
// timeout bounds the read, so a slow server delays only the work after it.
func (w *watcher) reportInbox(ctx context.Context, c *client.Client, now time.Time) {
	if w.sidebar == nil {
		return
	}
	in, err := c.Inbox(ctx)
	if err != nil {
		w.logf(now, "sidebar: read the inbox: %v", err)
		return
	}
	text := inboxText(in.Counts)
	listed := map[string]bool{}
	for _, ws := range w.workspaces {
		listed[ws] = true
		if prev, ok := w.sidebarText[ws]; ok && prev == text {
			continue
		}
		p := herdr.WorkspaceMetadataParams{WorkspaceID: ws, Source: "sessionhub", Tokens: map[string]*string{inboxToken: nil}}
		if text != "" {
			t := text
			p.Tokens[inboxToken] = &t
		}
		if err := w.sidebar.ReportWorkspaceMetadata(p); err != nil {
			w.logf(now, "sidebar: workspace %s: %v", ws, err)
			continue
		}
		w.sidebarText[ws] = text
	}
	for ws := range w.sidebarText {
		if !listed[ws] {
			delete(w.sidebarText, ws) // closed workspaces
		}
	}
}

// digests runs the digest for each known Claude session whose transcript
// changed since its last good digest. The pane cache still holds panes that
// closed a moment ago, so a session's last turn is digested too.
func (w *watcher) digests(ctx context.Context, now time.Time) {
	if w.digest == nil || w.transcriptStat == nil {
		return
	}
	// The cache is keyed by pane, and a resumed session can sit under two
	// panes for a while, so collect each session once.
	set := map[string]bool{}
	for _, e := range w.cache {
		if e.u.ID != "" {
			set[e.u.ID] = true
		}
	}
	// Forget sessions the cache no longer holds, so the map stays bounded.
	for id := range w.digested {
		if !set[id] {
			delete(w.digested, id)
		}
	}
	for id := range w.failed {
		if !set[id] {
			delete(w.failed, id)
		}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	// Sessions that never failed go first, so a few persistent failures
	// cannot starve the rest; failed ones follow, oldest failure first.
	sort.Slice(ids, func(i, j int) bool {
		a, aok := w.failed[ids[i]]
		b, bok := w.failed[ids[j]]
		if aok != bok {
			return !aok
		}
		if aok && !a.Equal(b) {
			return a.Before(b)
		}
		return ids[i] < ids[j]
	})
	ran := 0
	for _, id := range ids {
		if ran == maxDigestsPerBeat {
			return
		}
		size, mod, ok := w.transcriptStat(id)
		if !ok {
			continue
		}
		mark := transcriptMark{size, mod}
		if w.digested[id] == mark {
			continue
		}
		ran++
		dctx, cancel := context.WithTimeout(ctx, digestTimeout)
		err := w.digest(dctx, id)
		cancel()
		if err != nil {
			w.logf(now, "digest %s: %v", id, err)
			w.failed[id] = now
			continue
		}
		delete(w.failed, id)
		w.digested[id] = mark
	}
}

// itemLabel names an item in log lines: "upsert", "event state_changed".
func itemLabel(it client.Item) string {
	if k := metaOf(it).kind; k != "" {
		return it.Op + " " + k
	}
	return it.Op
}

// peek returns the queued items without changing the file.
func (w *watcher) peek() ([]client.Item, error) {
	var items []client.Item
	err := w.q.Drain(func(all []client.Item) []client.Item {
		items = append(items, all...)
		return all // unchanged: no rewrite
	})
	return items, err
}

// pass drains the queue once, in two phases:
//
//  1. Outside any send: peek at the queue and, when a pane item names a pane
//     the cache does not know, refresh the cache from one snapshot. herdr and
//     git calls never run while the queue lock is held.
//  2. One Drain: coalesce, resolve from the cache, and send each item through
//     client.Replay. Sending holds the queue lock, so the whole send phase has
//     a passBudget deadline; items it does not reach stay queued for the next
//     pass. That bounds how long a concurrent Append (`sessionhub plugin event`, a
//     SessionEnd hook) waits on the lock.
//
// Errors: retryable ones and auth errors (401/403: a wrong or revoked token)
// stop the pass and keep that item and every later one. Any other error drops
// the item with a log line. A kill mid-pass leaves the file as it was, so the
// items sent in that pass are sent again (at-least-once, see docs/client.md).
func (w *watcher) pass(ctx context.Context, now time.Time) {
	items, err := w.peek()
	if err != nil {
		w.logf(now, "queue: %v", err)
		return
	}
	if len(items) == 0 {
		return
	}
	if w.needsRefresh(items, now) {
		if _, err := w.refresh(ctx, now); err != nil {
			w.logf(now, "herdr snapshot: %v", err)
		}
	}

	c, cerr := w.newClient()
	sctx, cancel := context.WithTimeout(ctx, passBudget)
	defer cancel()
	err = w.q.Drain(func(all []client.Item) []client.Item {
		keep, _ := coalesce(all)
		var unsent []client.Item
		stopped := false
		for _, it := range keep {
			// A queued herdr_sessions body (from a failed startup) older than a
			// heartbeat the server accepted is stale: sending it would briefly
			// revive sessions the heartbeat already ended.
			if it.Op == client.OpHerdrSessions && !w.lastBeatOK.IsZero() && it.QueuedAt.Before(w.lastBeatOK) &&
				metaOf(it).herdrSession == w.herdrSession {
				continue
			}
			r, ok := w.resolve(it)
			if !ok {
				if now.Sub(it.QueuedAt) >= unresolvedTTL {
					w.logf(now, "dropping %s for pane %s: no session after %s", itemLabel(it), it.PaneID, unresolvedTTL)
					continue
				}
				unsent = append(unsent, it)
				continue
			}
			if stopped {
				unsent = append(unsent, it)
				continue
			}
			if cerr != nil {
				w.logf(now, "send skipped: %v", cerr)
				stopped = true
				unsent = append(unsent, it)
				continue
			}
			if sctx.Err() != nil {
				stopped = true
				unsent = append(unsent, it)
				continue
			}
			err := w.send(sctx, c, r)
			switch {
			case err == nil:
			case client.IsAuthError(err):
				w.logf(now, "send refused, check the token; keeping queue: %s: %v", itemLabel(it), err)
				stopped = true
				unsent = append(unsent, it)
			case client.IsRetryable(err):
				if sctx.Err() != nil && ctx.Err() == nil {
					w.logf(now, "pass budget of %s used up; keeping the rest for the next pass", passBudget)
				} else {
					w.logf(now, "send failed, keeping queue: %s: %v", itemLabel(it), err)
				}
				stopped = true
				unsent = append(unsent, it)
			default:
				w.logf(now, "dropping %s for session %s: %v", itemLabel(it), r.SessionID, err)
			}
		}
		return unsent
	})
	if err != nil {
		w.logf(now, "queue: %v", err)
	}
}

// needsRefresh reports whether a pane item names a pane of this herdr server
// that the cache does not know, or knows only from a snapshot older than the
// item. The second case catches /clear: the pane keeps its ID but gets a new
// agent_session, so an entry from before the item would send the item to the
// old session. A closed pane is gone from every new snapshot, so pane_closed
// never triggers a refresh; a snapshot already taken in this step (the
// heartbeat's) counts.
func (w *watcher) needsRefresh(items []client.Item, now time.Time) bool {
	if w.lastSnap.Equal(now) {
		return false
	}
	for _, it := range items {
		if it.Op == client.OpHerdrSessions || it.SessionID != "" || it.PaneID == "" {
			continue
		}
		m := metaOf(it)
		if m.kind == api.KindPaneClosed || (m.herdrSession != "" && m.herdrSession != w.herdrSession) {
			continue
		}
		if e, ok := w.cache[it.PaneID]; !ok || it.QueuedAt.After(e.seen) {
			return true
		}
	}
	return false
}

// resolve fills in the session for a pane item from the cache: SessionID, and
// for a pane upsert the full body from the snapshot. Items that already name a
// session (hooks, MCP) or need none (herdr_sessions) pass through. ok is false
// when the pane has no known session yet, or belongs to another herdr server.
// It makes no herdr call: pass refreshes the cache before it takes the lock.
func (w *watcher) resolve(it client.Item) (client.Item, bool) {
	if it.Op == client.OpHerdrSessions || it.SessionID != "" || it.PaneID == "" {
		return it, true
	}
	m := metaOf(it)
	if m.herdrSession != "" && m.herdrSession != w.herdrSession {
		return it, false
	}
	e, ok := w.cache[it.PaneID]
	if !ok {
		return it, false
	}
	it.SessionID = e.u.ID
	if it.Op == client.OpUpsert {
		b, err := json.Marshal(e.u)
		if err != nil {
			return it, false
		}
		it.Body = b
	}
	return it, true
}

// send replays one resolved item. When the server does not know the session
// of an event, report, or title (404), it upserts the session from what the
// pane gives and retries once. Without that, an MCP report or title queued
// before the session's first upsert reached the server was dropped.
func (w *watcher) send(ctx context.Context, c *client.Client, it client.Item) error {
	err := client.Replay(ctx, c, it)
	if !client.IsNotFound(err) || it.SessionID == "" {
		return err
	}
	switch it.Op {
	case client.OpEvent, client.OpReport, client.OpTitle:
	default:
		return err
	}
	if uerr := c.UpsertSession(ctx, w.upsertFor(it)); uerr != nil {
		return uerr
	}
	return client.Replay(ctx, c, it)
}

// upsertFor is the session as the watcher knows it: the snapshot entry for the
// item's pane, else any cached pane running that session (a hooks item has no
// pane), else the bare ID. Every field comes from herdr (cwd, herdr IDs, git
// info, title hint), with agent "claude" and source "plugin".
func (w *watcher) upsertFor(it client.Item) api.SessionUpsert {
	if e, ok := w.cache[it.PaneID]; ok && it.PaneID != "" && e.u.ID == it.SessionID {
		return e.u
	}
	for _, e := range w.cache {
		if e.u.ID == it.SessionID {
			return e.u
		}
	}
	return api.SessionUpsert{ID: it.SessionID, Agent: "claude", Source: api.SourcePlugin}
}

// socketID identifies the herdr socket file; a new herdr server makes a new
// one.
type socketID struct{ dev, ino uint64 }

func statSocket(path string) (socketID, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return socketID{}, err
	}
	return socketID{uint64(st.Dev), uint64(st.Ino)}, nil
}

// lockRetryFor is how long a new watcher keeps trying watcher.lock before it
// concludes another watcher holds it.
const lockRetryFor = 200 * time.Millisecond

// lockWithRetry is tryLock retried every 10 ms for up to wait. Every
// `sessionhub plugin event` probes watcher.lock (watcherRunning takes the flock and
// releases it at once), so a single try can fail against a probe instead of
// a watcher; the new watcher then exits and no watcher runs. A real watcher
// holds the lock for its lifetime, so it still wins every retry.
func lockWithRetry(path string, wait time.Duration) (*os.File, bool, error) {
	deadline := time.Now().Add(wait)
	for {
		f, held, err := tryLock(path)
		if err != nil || !held || !time.Now().Before(deadline) {
			return f, held, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// runWatcher holds <state>/watcher.lock for its lifetime and ticks every 2 s
// until ctx ends or the herdr socket changes or disappears.
func runWatcher(ctx context.Context, stateDir, socketPath string, newClient func() (*client.Client, error), git gitFunc, logger *log.Logger) error {
	lock, held, err := lockWithRetry(filepath.Join(stateDir, watcherLockFile), lockRetryFor)
	if err != nil {
		return err
	}
	if held {
		logger.Printf("another watcher holds %s; exiting", filepath.Join(stateDir, watcherLockFile))
		return nil
	}
	defer lock.Close()
	lock.Truncate(0)
	lock.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0)

	sock, err := statSocket(socketPath)
	if err != nil {
		logger.Printf("herdr socket %s: %v; exiting", socketPath, err)
		return nil
	}
	hc, err := herdr.Dial(socketPath)
	if err != nil {
		logger.Printf("herdr socket %s: %v; exiting", socketPath, err)
		return nil
	}
	w := newWatcher(client.NewQueue(stateDir), hc, herdr.SessionName(socketPath), newClient, git, logger)
	w.logPath = filepath.Join(stateDir, watcherLogFile)
	w.sidebar = hc
	w.instructionsPath = client.InstructionsPath(stateDir)
	w.transcriptStat = func(id string) (int64, time.Time, bool) {
		p, err := digest.TranscriptPath(digest.DefaultClaudeDir(), id)
		if err != nil {
			return 0, time.Time{}, false
		}
		fi, err := os.Stat(p)
		if err != nil {
			return 0, time.Time{}, false
		}
		return fi.Size(), fi.ModTime(), true
	}
	b := digest.Builder{Git: digest.Git}
	w.digest = func(ctx context.Context, id string) error {
		d, err := b.Build(ctx, id)
		if errors.Is(err, digest.ErrLocked) || errors.Is(err, digest.ErrEmpty) {
			return nil
		}
		if err != nil {
			return err
		}
		c, cerr := w.newClient()
		if cerr != nil {
			c = nil
		}
		_, err = digest.Send(ctx, c, w.q, id, d)
		return err
	}
	logger.Printf("watcher started: pid %d, herdr session %q, socket %s", os.Getpid(), w.herdrSession, socketPath)

	// The control loop runs beside the 2 s ticks and stops with them. Wait
	// for it on the way out, so a request it is running ends with the
	// watcher, not after it.
	ctl := newController(newClient, controlRunner(socketPath, 0, controlPollFor), logger)
	ctl.deliver = messageRunner(socketPath)
	ctl.start = startRunner(socketPath, 0, controlPollFor)
	if key, err := move.LoadOrCreateKey(move.KeyPath()); err != nil {
		logger.Printf("move: no move key, so this machine can't move sessions: %v", err)
	} else {
		cfg, err := client.LoadConfig()
		if err != nil {
			logger.Printf("move: client config not read, so moves search only the default roots: %s", cleanErr(err))
		}
		w.moveKey = move.PublicKeyString(key)
		ctl.move = newMover(socketPath, digest.DefaultClaudeDir(), stateDir, move.DefaultRoots(cfg.MoveRoots), key, logger).handle
	}
	ctlCtx, stopCtl := context.WithCancel(ctx)
	ctlDone := make(chan struct{})
	go func() {
		defer close(ctlDone)
		ctl.loop(ctlCtx)
	}()
	defer func() {
		stopCtl()
		<-ctlDone
	}()

	t := time.NewTicker(passInterval)
	defer t.Stop()
	w.step(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			logger.Printf("watcher stopping: %v", context.Cause(ctx))
			return nil
		case <-t.C:
		}
		cur, err := statSocket(socketPath)
		if err != nil {
			logger.Printf("herdr socket gone (%v); exiting", err)
			return nil
		}
		if cur != sock {
			logger.Printf("herdr socket replaced (new server); exiting")
			return nil
		}
		w.step(ctx, time.Now())
	}
}
