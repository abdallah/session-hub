package plugin

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr"
	"github.com/abdallah/session-hub/internal/resume"
)

// Control loop timing.
const (
	controlWait       = 30 * time.Second // the long-poll wait the watcher asks for
	controlBackoffMin = 5 * time.Second  // the first pause after a network error
	controlBackoffMax = 60 * time.Second
	controlAuthPause  = 5 * time.Minute  // after 401 or 403, or without a usable config
	controlFastPoll   = time.Second      // the least time between two polls that got nothing
	controlPollFor    = 15 * time.Second // how long to watch the pane for the link
	// controlPostFor bounds posting a result, also after the watcher stops.
	controlPostFor = 2 * time.Second
	// controlResumedFor is how long the watcher remembers the pane it resumed
	// a session in. herdr reports the session there only some seconds after
	// Claude starts; until then a second request must not resume it again.
	controlResumedFor = 2 * time.Minute
)

// Details the watcher reports. The dashboard and the CLI show them as they
// are.
const (
	detailAlreadyOn     = "Remote Control was already on"
	detailDialogStuck   = "Remote Control was already on; press Esc in the pane to close its dialog"
	detailNoLink        = "sent, link not seen"
	detailResumed       = "resumed in a new herdr workspace"
	detailResumedNoLink = "resumed in a new herdr workspace, link not seen"
	detailBlocked       = "waiting on a prompt in the pane"
	detailDialogOpen    = "a Remote Control dialog is open in the pane; press Esc there"
	detailLooksLive     = "looks live but pane not found"
	detailNoDir         = "the session's directory no longer exists"
	detailNotRunning    = "not running"
	detailHerdrFailed   = "herdr call failed"
	detailJustResumed   = "just resumed; try again in a moment"
	detailNotInHerdr    = "the session is not in herdr"
	detailPaneGone      = "the session's pane is gone"
	detailOtherSession  = "the pane runs another session"
	detailNoAgent       = "no agent runs in the pane"
	detailNoSessionID   = "herdr has no session ID for the pane yet"
	detailHerdrDown     = "herdr did not answer"
	detailNoDelivery    = "this watcher cannot deliver messages"
	detailStarted       = "started in a new herdr workspace"
	detailStartedNoLink = "started in a new herdr workspace, link not seen"
	detailStartOff      = "starting sessions is turned off on this machine (remote_start = false)"
	detailNoStarter     = "this watcher cannot start sessions"
	detailStartNoDir    = "the directory doesn't exist on this machine"
	detailOutsideHome   = "the directory is outside this machine's home directory"
)

// controlFunc turns on Remote Control for one session on this machine.
// knownPane is the pane the watcher resumed the session in moments ago, or "".
type controlFunc func(ctx context.Context, s api.Session, knownPane string) resume.ControlResult

// messageFunc submits one message prompt to session s's pane and says what
// happened: delivered, busy (try later), or refused.
type messageFunc func(ctx context.Context, s api.Session, text string) api.ControlResultIn

// paneAgent is the herdr calls message delivery needs (*herdr.Client).
type paneAgent interface {
	PaneGet(id string) (herdr.PaneInfo, error)
	AgentPrompt(target, text string) error
}

// messageRunner is the watcher's messageFunc on the herdr socket.
func messageRunner(socket string) messageFunc {
	return func(_ context.Context, s api.Session, text string) api.ControlResultIn {
		hc, err := herdr.Dial(socket)
		if err != nil {
			return api.ControlResultIn{State: api.MessageBusy, Detail: detailHerdrDown}
		}
		defer hc.Close()
		return deliverMessage(hc, s, text)
	}
}

// deliverMessage types text into s's pane with agent.prompt, but only when
// herdr reports the pane's agent idle or done and the pane runs s. A working
// or blocked agent, an agent whose status herdr does not report, a pane
// whose session ID herdr has not learned yet (it learns it a few seconds
// after Claude starts; until then agent_session is missing or not of kind
// "id"), a herdr that does not answer, or a herdr error other than
// pane_not_found is busy: the server offers the message again later. A
// missing pane (pane_not_found), another session, or no agent is refused.
func deliverMessage(h paneAgent, s api.Session, text string) api.ControlResultIn {
	busy := func(d string) api.ControlResultIn { return api.ControlResultIn{State: api.MessageBusy, Detail: d} }
	refused := func(d string) api.ControlResultIn { return api.ControlResultIn{State: api.MessageRefused, Detail: d} }
	if s.HerdrPane == "" {
		return refused(detailNotInHerdr)
	}
	p, err := h.PaneGet(s.HerdrPane)
	var he *herdr.Error
	switch {
	case errors.As(err, &he) && he.Code == "pane_not_found":
		return refused(detailPaneGone)
	case err != nil:
		return busy(detailHerdrDown)
	}
	if p.Agent == "" {
		return refused(detailNoAgent)
	}
	var id string
	if as := p.AgentSession; as != nil && as.Kind == "id" {
		id = as.Value
	}
	switch {
	case id == "":
		return busy(detailNoSessionID)
	case id != s.ID:
		return refused(detailOtherSession)
	}
	switch p.AgentStatus {
	case "idle", "done":
	case "":
		return busy("the agent's status is unknown")
	default:
		return busy("the agent is " + termtext.Clean(p.AgentStatus, 40))
	}
	err = h.AgentPrompt(s.HerdrPane, text)
	var be *herdr.BlockedError
	switch {
	case errors.As(err, &be):
		return busy(detailBlocked)
	case errors.As(err, &he):
		return refused(termtext.Clean(he.Error(), api.MaxControlDetailRunes))
	case err != nil:
		return busy(detailHerdrDown)
	}
	return api.ControlResultIn{State: api.MessageDelivered}
}

// startRunner is the watcher's start function: resume.StartNew on the herdr
// socket, in the home directory, unless this machine turned remote starts
// off. It reads the client config for each request, so the opt-out takes
// effect without a restart.
func startRunner(socket string, every, wait time.Duration) func(ctx context.Context, req api.StartRequest) api.ControlResultIn {
	return func(ctx context.Context, req api.StartRequest) api.ControlResultIn {
		// An unreadable config might hold remote_start = false: refuse.
		cfg, err := client.LoadConfig()
		if err != nil {
			return api.ControlResultIn{State: api.ControlFailed, Detail: "the client config can't be read, so remote starts are refused"}
		}
		if client.RemoteStartOff(cfg, os.Getenv) {
			return api.ControlResultIn{State: api.ControlFailed, Detail: detailStartOff}
		}
		home, _ := os.UserHomeDir()
		return startResultFor(resume.StartNew(ctx, socket, home, req,
			resume.ControlOptions{Socket: socket, PollEvery: every, PollFor: wait}))
	}
}

// startResultFor turns what resume.StartNew did into the result the server
// stores.
func startResultFor(r resume.NewSessionResult) api.ControlResultIn {
	switch r.Outcome {
	case resume.OutcomeStarted:
		detail := detailStarted
		if r.URL == "" {
			detail = detailStartedNoLink
		}
		if r.PromptNote != "" {
			detail += "; " + r.PromptNote
		}
		return api.ControlResultIn{State: api.ControlDone, URL: r.URL, Detail: termtext.Clean(detail, api.MaxControlDetailRunes)}
	case resume.OutcomeNoDir:
		return api.ControlResultIn{State: api.ControlFailed, Detail: detailStartNoDir}
	case resume.OutcomeOutsideHome:
		return api.ControlResultIn{State: api.ControlFailed, Detail: detailOutsideHome}
	}
	msg := detailHerdrFailed
	if r.Err != nil {
		msg = r.Err.Error()
	}
	return api.ControlResultIn{State: api.ControlFailed, Detail: termtext.Clean(msg, api.MaxControlDetailRunes)}
}

// controlRunner is the watcher's controlFunc: resume.Control on the herdr
// socket, resuming a session that no pane runs unless the watcher just
// resumed it in knownPane. Zero every or wait means resume's defaults.
func controlRunner(socket string, every, wait time.Duration) controlFunc {
	return func(ctx context.Context, s api.Session, knownPane string) resume.ControlResult {
		return resume.Control(ctx, s, resume.ControlOptions{Socket: socket, Resume: true, PollEvery: every, PollFor: wait, KnownPane: knownPane})
	}
}

// controller keeps one long poll open to the sessionhub and runs each Remote
// Control request it claims. It never touches the queue or its lock.
type controller struct {
	newClient func() (*client.Client, error)
	run       controlFunc
	// deliver submits a message; nil refuses every message. runWatcher sets
	// it.
	deliver messageFunc
	// move runs a move claim and posts its result; nil fails every move
	// step. runWatcher sets it when the machine has a move key.
	move func(ctx context.Context, cl *client.Client, claim api.ControlClaim)
	// start starts a new session for a start request; nil fails every
	// start. runWatcher sets it.
	start func(ctx context.Context, req api.StartRequest) api.ControlResultIn
	log   *log.Logger
	wait  time.Duration
	// sleep pauses for d, or returns false when ctx ends first. Tests
	// replace it.
	sleep   func(ctx context.Context, d time.Duration) bool
	backoff time.Duration // the last network-error pause; 0 after a success
	// resumed maps a session ID to the pane the watcher resumed it in, for
	// controlResumedFor. Only loop's goroutine uses it.
	resumed map[string]resumedPane
	now     func() time.Time // tests replace it
}

// resumedPane is where and when the watcher resumed a session.
type resumedPane struct {
	pane string
	at   time.Time
}

func newController(newClient func() (*client.Client, error), run controlFunc, logger *log.Logger) *controller {
	return &controller{newClient: newClient, run: run, log: logger, wait: controlWait, sleep: sleepCtx,
		resumed: map[string]resumedPane{}, now: time.Now}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// loop polls until ctx ends.
func (c *controller) loop(ctx context.Context) {
	c.log.Printf("control: waiting for Remote Control requests")
	for ctx.Err() == nil {
		if d := c.once(ctx); d > 0 && !c.sleep(ctx, d) {
			return
		}
	}
}

// once runs one poll and, when it claims a request, that request. It
// returns how long to pause before the next poll.
func (c *controller) once(ctx context.Context) time.Duration {
	cl, err := c.newClient()
	if err != nil {
		c.log.Printf("control: %v; retrying in %s", err, controlAuthPause)
		return controlAuthPause
	}
	start := time.Now()
	claim, err := cl.PollControl(ctx, c.wait)
	switch {
	case ctx.Err() != nil:
		return 0
	case client.IsAuthError(err):
		c.backoff = 0
		c.log.Printf("control: poll refused, check the token; retrying in %s: %v", controlAuthPause, err)
		return controlAuthPause
	case err != nil:
		if c.backoff == 0 {
			c.backoff = controlBackoffMin
		} else {
			c.backoff = min(2*c.backoff, controlBackoffMax)
		}
		c.log.Printf("control: poll failed; retrying in %s: %v", c.backoff, err)
		return c.backoff
	}
	c.backoff = 0
	if claim == nil {
		// A server that answers 204 at once (shutting down, or a proxy)
		// must not turn the loop into a busy one.
		if time.Since(start) < controlFastPoll {
			return controlFastPoll
		}
		return 0
	}
	c.handle(ctx, cl, *claim)
	return 0
}

// handle runs one claimed request and posts its result, also when ctx ends
// meanwhile. A result the server refuses (the request expired meanwhile) or
// can't take is logged, not retried: the request expires, and the dashboard
// says so.
func (c *controller) handle(ctx context.Context, cl *client.Client, claim api.ControlClaim) {
	req := claim.Request
	if req.Action == api.ActionMoveOut || req.Action == api.ActionMoveIn {
		c.handleMove(ctx, cl, claim)
		return
	}
	in := api.ControlResultIn{State: api.ControlFailed, Detail: "unknown action " + termtext.Clean(req.Action, 40)}
	switch req.Action {
	case api.ActionRemoteControl:
		in = resultFor(c.control(ctx, claim.Session))
	case api.ActionMessage:
		in = api.ControlResultIn{State: api.MessageRefused, Detail: detailNoDelivery}
		if c.deliver != nil {
			in = c.deliver(ctx, claim.Session, claim.Text)
		}
	case api.ActionStart:
		in = api.ControlResultIn{State: api.ControlFailed, Detail: detailNoStarter}
		if c.start != nil && claim.Start != nil {
			in = c.start(ctx, *claim.Start)
		}
	}
	target := "session " + claim.Session.ID
	if req.Action == api.ActionStart {
		target = "a new session"
	}
	c.log.Printf("control: request %s for %s: %s %s", req.ID, target, in.State, in.Detail)
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), controlPostFor)
	defer cancel()
	if err := cl.PostControlResult(pctx, req.ID, in); err != nil {
		c.log.Printf("control: result for request %s not recorded: %v", req.ID, err)
	}
}

// handleMove passes a move claim to the mover. Without one it fails a pack
// or unpack step, so the move ends at once instead of timing out, and logs
// a finish.
func (c *controller) handleMove(ctx context.Context, cl *client.Client, claim api.ControlClaim) {
	if c.move != nil {
		c.move(ctx, cl, claim)
		return
	}
	mv := claim.Move
	if mv == nil || (mv.State != api.MovePacking && mv.State != api.MoveUnpacking) {
		c.log.Printf("control: move claim %s ignored: %s", claim.Request.ID, detailNoMover)
		return
	}
	step := "check pane"
	if mv.State == api.MoveUnpacking {
		step = "download"
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), controlPostFor)
	defer cancel()
	if err := cl.PostMoveResult(pctx, mv.ID, api.MoveResultIn{State: api.MoveFailed, Detail: step + ": " + detailNoMover}); err != nil {
		c.log.Printf("control: move %s: result not recorded: %s", mv.ID, cleanErr(err))
	}
}

// control runs c.run for session s, with the pane the watcher resumed s in
// during the last controlResumedFor, and remembers the pane of a new resume.
func (c *controller) control(ctx context.Context, s api.Session) resume.ControlResult {
	now := c.now()
	for id, r := range c.resumed {
		if now.Sub(r.at) >= controlResumedFor {
			delete(c.resumed, id)
		}
	}
	r := c.run(ctx, s, c.resumed[s.ID].pane)
	// A workspace means Claude may be running there, also when agent.start
	// then failed.
	if r.Workspace != "" && r.Pane != "" {
		c.resumed[s.ID] = resumedPane{pane: r.Pane, at: now}
	}
	return r
}

// resultFor turns what resume.Control did into the result the server
// stores. The detail passes the server's free-text rules: no control
// characters, at most api.MaxControlDetailRunes runes.
func resultFor(r resume.ControlResult) api.ControlResultIn {
	done := func(url, detail string) api.ControlResultIn {
		return api.ControlResultIn{State: api.ControlDone, URL: url, Detail: detail}
	}
	failed := func(detail string) api.ControlResultIn {
		return api.ControlResultIn{State: api.ControlFailed, Detail: detail}
	}
	switch r.Outcome {
	case resume.OutcomeLinked:
		return done(r.URL, "")
	case resume.OutcomeAlreadyOn:
		if r.DialogLeftOpen != "" {
			return done(r.URL, detailDialogStuck)
		}
		return done(r.URL, detailAlreadyOn)
	case resume.OutcomeNoLink:
		return done("", detailNoLink)
	case resume.OutcomeResumed:
		if r.URL == "" {
			return done("", detailResumedNoLink)
		}
		return done(r.URL, detailResumed)
	case resume.OutcomeBlocked:
		return failed(detailBlocked)
	case resume.OutcomeDialogOpen:
		return failed(detailDialogOpen)
	case resume.OutcomeLooksLive:
		return failed(detailLooksLive)
	case resume.OutcomeNoDir:
		return failed(detailNoDir)
	case resume.OutcomeNotRunning:
		return failed(detailNotRunning)
	case resume.OutcomeJustResumed:
		return failed(detailJustResumed)
	}
	msg := detailHerdrFailed
	if r.Err != nil {
		msg = r.Err.Error()
	}
	return failed(termtext.Clean(msg, api.MaxControlDetailRunes))
}
