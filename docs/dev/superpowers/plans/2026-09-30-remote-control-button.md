# Remote Control Button Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One tap on the sessionhub dashboard turns on Claude Code Remote Control for a session, or resumes it with Remote Control on, and shows an **Open in Claude** link; `sessionhub remote-control <id>` uses the same channel for a session on another machine.

**Architecture:** The server stores control requests in a new `control_requests` table (schema v3). The dashboard (read cookie plus `X-Hub-Action`) or a machine token creates a request; the owning machine's plugin watcher holds a long poll open on `GET /v1/machines/self/control`, claims the request, runs one shared function in `internal/resume` against the local herdr socket (inject `/remote-control`, or resume in a new no-focus workspace), and posts the result. The session read model carries the latest request, the last link, and a `controllable` flag; the dashboard and the CLI poll the session for the result.

**Tech Stack:** Go 1.25, standard library `net/http`, `modernc.org/sqlite`, the herdr socket API (herdr 0.9.3), one inline HTML/JS page under a hash-based CSP. No new Go dependencies. The dashboard state test runs under `node` when it is on `PATH`.

**Spec:** `docs/dev/superpowers/specs/2026-09-30-remote-control-button-design.md`. It is the binding authority; read it with this plan.

## Prerequisite

The CLI task (branch `worktree-agent-a106cbf2f0bb3a738`, worktree `.claude/worktrees/agent-a106cbf2f0bb3a738`) merges into `remote-control-cli` before Task 1 starts. Check it on `remote-control-cli`:

```bash
git log --oneline | grep -c "Close the already-on dialog"   # 1
grep -n "func (c \*Client) SendKeys(paneID string, keys ...string) error" internal/herdr/herdr.go
grep -n "func visibleDialog\|func newRemoteControlURL\|func (e \*env) remoteControlLocal\|func (e \*env) closeDialog" internal/resume/remotecontrol.go
```

Expected: each command prints a match. If any is missing, stop: the merge hasn't happened.

This plan quotes `remoteControlLocal` and `closeDialog` as they are at the CLI branch's commit `fcc4638` ("Detect the already-on dialog on the visible screen only"). If a later fix round changed them, Task 3 moves the **merged** steps, whatever they are, into one function; the existing CLI tests are the check that the move changed nothing.

## Global Constraints

Values copied from the spec:

- Request ID: `cr_` + 16 random bytes, base64url (22 characters, no padding).
- `action` is always `remote_control`, checked against a fixed list in code.
- `state` is `pending`, `claimed`, `done`, `failed`, or `expired`.
- `requested_by` is `dashboard` or `machine:<name>`.
- `expires_at` = `created_at` + 2 minutes, for pending and claimed requests alike. An expired request is never retried automatically.
- Schema version 3, upgraded in place like version 2.
- `Controllable`: the session has a herdr pane, and its machine's watcher polled within the last 2 minutes.
- Event kinds: `remote_control_requested` and `remote_control_result`. Every request and result is an event, visible in `sessionhub show`.
- `POST /v1/sessions/{id}/remote-control`: the read cookie with header `X-Hub-Action: remote-control`, or a machine token. The bearer read token and the cookie without the header are refused. `409` when the session has no herdr pane or its watcher is offline; `200` with the open request if one exists; `202` for a new one; `429` when the machine already has 10 pending requests.
- `GET /v1/machines/self/control?wait=30`: machine tokens only; `wait` clamped to 1–30 seconds; records the machine's last poll; claims the oldest pending request in the same transaction; else waits, woken in-process, then `204`. At most 2 open polls per machine; a third gets `429`.
- `POST /v1/control/{request-id}/result`: machine tokens only, only the machine that claimed it. Body `{"state":"done"|"failed","url":"…","detail":"…"}`. `url` must match `^https://claude\.ai/code/session_[A-Za-z0-9_-]+$`. `detail` goes through the free-text rules. `done` with a URL sets `RemoteControlURL` and `RemoteControlAt`.
- Watcher: back off from 5 seconds up to 60 seconds after a network error; on 401 or 403, log and wait 5 minutes. Read the pane for up to 15 seconds for the link.
- Resume: a herdr workspace in the session's recorded directory, without focus, labeled `sessionhub: <title>`; `claude --resume <id> --remote-control` with `agent.start`. If the directory no longer exists, report `failed` and create nothing. Never start a second copy of a running session: "looks live but pane not found".
- CLI: poll every 2 seconds for up to 2 minutes; on a failed create (`409`, network error) or expiry, print `ssh -t <host> '~/.local/bin/sessionhub remote-control <id>'`, and the `herdr --machine` form when a saved machine matches.
- Dashboard: **Remote Control** on a live session (idle, working, or done); **Resume with Remote Control** on an ended or stale one; disabled with "waiting on a prompt in the pane" on a blocked one; **Sending…**; poll every 2 seconds for up to 2 minutes; **Open in Claude** plus a copy button; the failure reason; "the machine didn't respond in time"; the last link and its time until the session ends. The CSP stays strict, all text goes in with `textContent`, and the page sends only this one kind of write request.
- Live tests use scratch herdr workspaces only, never the user's panes.
- Test commands: `GOFLAGS=-count=1 go test ./internal/<pkg>/ -run <Test>` while working, then `make test lint` at the end of every task.
- Every commit message ends with:

  ```
  Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
  ```

Decisions this plan makes where the spec is silent (one line each, so no task decides them again):

- The cookie without `X-Hub-Action` and the bearer read token (with or without the header) get `403` on the remote-control route. A missing or unknown credential gets `401`.
- Create checks run in this order: unknown session `404`, no pane `409`, watcher offline `409`, open request `200`, 10 pending `429`, new `202`.
- A result for a request that isn't `claimed` (pending, expired, done, failed) gets `409` and changes nothing. A late result for an expired request is lost; the watcher logs it.
- The `remote_control_requested` event has source `server`; `remote_control_result` has source `plugin`. Clients can't post either kind.
- Expiry: reads compute `expired` from `expires_at` without writing; create, claim, and result run the `UPDATE` inside their transaction.
- The last link lives in `sessions.rc_url` and `sessions.rc_at`, cleared in the three write paths that end a session: `AddEvent` for `pane_closed` and `ended`, and `ReconcileHerdr` for a session missing from the snapshot.
- `sessionhub remote-control` for a session on this machine is unchanged: it injects or reports "not running", and never resumes. `sessionhub resume --remote-control` keeps its split-and-focus behavior. Only the watcher resumes in a new workspace.
- The long-poll handler extends its own write deadline, because `sessionhub server` sets `WriteTimeout: 30s`. `http.Server.RegisterOnShutdown` ends open polls with `204`.
- Component docs land in Task 7, as the task breakdown asks. Each task's commit body names the doc section Task 7 updates.

## Review Focus

Inputs the spec implies that are most likely to bite, each pinned by a test in the owning task:

1. **A 30-second poll against the server's 30-second `WriteTimeout`.** Without a per-response deadline, every idle poll ends in a dropped connection and the watcher backs off forever. Task 2: `TestControlPollOutlivesWriteTimeout`.
2. **A server restart with polls open.** `Shutdown` would wait its full 10 seconds on each open poll and exit with an error. Task 2: `TestControlPollEndsOnShutdown`.
3. **A result that arrives after its request expired.** It must not flip the request to `done` or set a link that the dashboard already reported as "didn't respond". Task 2: `TestControlResultAfterExpiry`.
4. **A session whose pane ID changed since the last heartbeat.** The watcher must find it in a fresh snapshot and inject, not report "looks live but pane not found" and not start a second copy. Task 3: the "pane moved" row of `TestControlDecisionTable`.
5. **A tap while the watcher went offline or the machine hit its limit.** The card must show the server's refusal as plain text, including one that contains markup. Task 6: the `refusals` rows of `TestDashboardRemoteControlStates`.

---

## File structure

| File | Task | Responsibility |
|---|---|---|
| `internal/api/types.go` | 1 | `ControlRequest`, `ControlResultIn`, `ControlClaim`, new `Session` fields, constants |
| `internal/store/store.go` | 1 | Schema v3, new sentinel errors |
| `internal/store/control.go` (new) | 1 | Create, claim, finish, expiry, poll time, request scanning |
| `internal/store/read.go` | 1 | Session read model: latest request, last link, `Controllable` |
| `internal/store/sessions.go` | 1 | Clear the last link when a session ends |
| `internal/server/control.go` (new) | 2 | The three handlers, the in-process poll sessionhub |
| `internal/server/routes.go`, `server.go`, `run.go` | 2 | Routes, `accessAction` auth, `statusWriter.Unwrap`, shutdown hook |
| `internal/herdr/herdr.go` | 3 | `WorkspaceCreate` |
| `internal/resume/control.go` (new) | 3 | `Control`: the shared inject-or-resume logic |
| `internal/resume/remotecontrol.go` | 3, 5 | The CLI local branch calls `Control`; the remote branch goes through the server |
| `internal/client/http.go` | 4, 5 | `doTimeout`, `PollControl`, `PostControlResult`, `CreateControl` |
| `internal/herdr/herdrtest/herdrtest.go` | 4 | `Handle`: scripted replies for one method |
| `internal/plugin/control.go` (new) | 4 | The watcher's control loop |
| `internal/plugin/watcher.go` | 4 | Start and stop the loop with the watcher |
| `internal/server/dashboard/index.html` | 6 | Button, states, polling, link |
| Docs and evidence | 7 | `docs/dev/SPEC.md`, `docs/dev/PLAN.md`, `docs/*.md`, `README.md`, `docs/dev/IDEAS.md`, `docs/dev/evidence/remote-control-button.md` |

---

### Task 1: Store schema v3, control requests, and API types

**Files:**
- Modify: `internal/api/types.go` (constants block at the top; `Session` struct)
- Modify: `internal/store/store.go` (errors block, `schemaVersion`, new `schemaV3`, `migrate`)
- Create: `internal/store/control.go`
- Modify: `internal/store/read.go` (`sessionSelect`, `scanSession`)
- Modify: `internal/store/sessions.go` (`AddEvent` ended branch; `ReconcileHerdr` end loop)
- Modify: `internal/store/store_test.go` (`TestOpenPragmasAndSchema`)
- Modify: `internal/store/concurrency_test.go`, `internal/store/migrate_race_test.go` (roll back the v3 additions too)
- Create: `internal/store/control_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces (later tasks use these exact names):
  - `api` constants: `SourceServer = "server"`, `KindRemoteControlRequested = "remote_control_requested"`, `KindRemoteControlResult = "remote_control_result"`, `ActionRemoteControl = "remote_control"`, `ControlPending = "pending"`, `ControlClaimed = "claimed"`, `ControlDone = "done"`, `ControlFailed = "failed"`, `ControlExpired = "expired"`, `RequestedByDashboard = "dashboard"`, `RequestedByMachinePrefix = "machine:"`, `HeaderAction = "X-Hub-Action"`, `HeaderActionRemoteControl = "remote-control"`, `MaxControlDetailRunes = 200`.
  - `api.ControlRequest{ID, SessionID, Machine, Action, State, RequestedBy string; CreatedAt, ExpiresAt time.Time; ClaimedAt, FinishedAt *time.Time; URL, Detail string}` with JSON tags `id session_id machine action state requested_by created_at expires_at claimed_at,omitempty finished_at,omitempty url,omitempty detail,omitempty`.
  - `api.ControlResultIn{State, URL, Detail string}` (`state`, `url,omitempty`, `detail,omitempty`).
  - `api.ControlClaim{Request api.ControlRequest; Session api.Session}` (`request`, `session`).
  - `api.Session` gains `RemoteControl *ControlRequest` (`remote_control,omitempty`), `RemoteControlURL string` (`remote_control_url,omitempty`), `RemoteControlAt *time.Time` (`remote_control_at,omitempty`), `Controllable bool` (`controllable`).
  - `store.ControlTTL = 2 * time.Minute`, `store.ControlPollWindow = 2 * time.Minute`, `store.MaxPendingPerMachine = 10`.
  - `store.ErrNotControllable`, `store.ErrTooMany`, `store.ErrRequestClosed` (sentinels; wrap with `%w`).
  - `func store.ValidControlID(id string) bool`, `func store.ValidRemoteControlURL(u string) bool`.
  - `func (s *Store) RecordPoll(ctx context.Context, machineID int64) error`
  - `func (s *Store) CreateControl(ctx context.Context, sessionID, requestedBy string) (api.ControlRequest, bool, error)` (bool: created)
  - `func (s *Store) ClaimControl(ctx context.Context, machineID int64) (api.ControlClaim, bool, error)` (bool: claimed one)
  - `func (s *Store) FinishControl(ctx context.Context, machineID int64, reqID string, in api.ControlResultIn) (api.ControlRequest, error)`

- [ ] **Step 1: Write the failing store tests**

Create `internal/store/control_test.go`:

```go
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

const testLink = "https://claude.ai/code/session_01AbCdEfGhIjKlMnOpQrStUv"

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *testClock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// controlEnv is a store with a fake clock and machines tower and bluebox.
type controlEnv struct {
	t         *testing.T
	s         *Store
	clock     *testClock
	tower, bluebox Machine
}

func newControlEnv(t *testing.T) *controlEnv {
	t.Helper()
	clock := &testClock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	s, err := Open(filepath.Join(t.TempDir(), "sessionhub.db"), Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	e := &controlEnv{t: t, s: s, clock: clock}
	ctx := context.Background()
	for _, m := range []struct {
		name string
		dst  *Machine
	}{{"tower", &e.tower}, {"bluebox", &e.bluebox}} {
		tok, _, err := s.AddMachine(ctx, m.name, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if *m.dst, err = s.MachineByToken(ctx, tok); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// session registers id on m, with a herdr pane unless pane is "".
func (e *controlEnv) session(m Machine, id, pane string) {
	e.t.Helper()
	u := api.SessionUpsert{ID: id, Agent: "claude", Source: api.SourcePlugin, CWD: "/home/user/proj", HerdrPane: pane}
	if pane != "" {
		u.HerdrSession = "default"
	}
	if _, err := e.s.UpsertSession(context.Background(), m.ID, u); err != nil {
		e.t.Fatal(err)
	}
}

func (e *controlEnv) poll(m Machine) {
	e.t.Helper()
	if err := e.s.RecordPoll(context.Background(), m.ID); err != nil {
		e.t.Fatal(err)
	}
}

func (e *controlEnv) get(id string) api.Session {
	e.t.Helper()
	x, err := e.s.GetSession(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return x
}

func (e *controlEnv) create(id string) api.ControlRequest {
	e.t.Helper()
	r, created, err := e.s.CreateControl(context.Background(), id, api.RequestedByDashboard)
	if err != nil || !created {
		e.t.Fatalf("create %s: created=%v err=%v", id, created, err)
	}
	return r
}

func (e *controlEnv) events(id, kind string) []api.Event {
	e.t.Helper()
	d, err := e.s.SessionDetail(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []api.Event
	for _, ev := range d.Events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func (e *controlEnv) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.s.db.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestCreateControlRules(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.session(e.tower, "sess-a", "w1:p1")
	e.session(e.tower, "sess-hooks", "")

	// Refusals change nothing.
	for _, tc := range []struct {
		id, by string
		want   error
		msg    string
	}{
		{"sess-none", api.RequestedByDashboard, ErrNotFound, "sess-none"},
		{"sess-hooks", api.RequestedByDashboard, ErrNotControllable, "not in herdr"},
		{"sess-a", api.RequestedByDashboard, ErrNotControllable, "offline"},
		{"sess-a", "phone", ErrInvalid, "requested_by"},
		{"-bad", api.RequestedByDashboard, ErrInvalid, "session id"},
	} {
		if _, _, err := e.s.CreateControl(ctx, tc.id, tc.by); !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("create %s by %q: err %v, want %v with %q", tc.id, tc.by, err, tc.want, tc.msg)
		}
	}
	if n := e.count(`SELECT COUNT(*) FROM control_requests`); n != 0 {
		t.Fatalf("refused creates stored %d rows", n)
	}

	e.poll(e.tower)
	if x := e.get("sess-a"); !x.Controllable || x.RemoteControl != nil {
		t.Fatalf("after a poll: controllable=%v request=%+v", x.Controllable, x.RemoteControl)
	}
	if x := e.get("sess-hooks"); x.Controllable {
		t.Error("a session without a pane is controllable")
	}
	r := e.create("sess-a")
	now := e.clock.Now()
	if !regexp.MustCompile(`^cr_[A-Za-z0-9_-]{22}$`).MatchString(r.ID) || !ValidControlID(r.ID) {
		t.Errorf("request ID %q", r.ID)
	}
	if r.SessionID != "sess-a" || r.Machine != "tower" || r.Action != api.ActionRemoteControl || r.State != api.ControlPending ||
		r.RequestedBy != api.RequestedByDashboard || !r.CreatedAt.Equal(now) || !r.ExpiresAt.Equal(now.Add(2*time.Minute)) ||
		r.ClaimedAt != nil || r.FinishedAt != nil || r.URL != "" || r.Detail != "" {
		t.Errorf("new request: %+v", r)
	}

	// The open request comes back for a second create, from anyone.
	again, created, err := e.s.CreateControl(ctx, "sess-a", api.RequestedByMachinePrefix+"bluebox")
	if err != nil || created || again.ID != r.ID {
		t.Errorf("second create: %+v created=%v err=%v", again, created, err)
	}
	if x := e.get("sess-a"); x.RemoteControl == nil || x.RemoteControl.ID != r.ID || x.RemoteControl.State != api.ControlPending {
		t.Errorf("session's latest request: %+v", x.RemoteControl)
	}
	evs := e.events("sess-a", api.KindRemoteControlRequested)
	if len(evs) != 1 || evs[0].Source != api.SourceServer || !strings.Contains(string(evs[0].Payload), r.ID) ||
		!strings.Contains(string(evs[0].Payload), `"requested_by":"dashboard"`) {
		t.Errorf("requested events: %+v", evs)
	}
}

func TestControllableWindow(t *testing.T) {
	e := newControlEnv(t)
	e.session(e.tower, "sess-a", "w1:p1")
	e.poll(e.tower)
	e.clock.Advance(2 * time.Minute)
	if !e.get("sess-a").Controllable {
		t.Error("exactly 2 minutes after a poll: not controllable")
	}
	e.clock.Advance(time.Second)
	if e.get("sess-a").Controllable {
		t.Error("2m1s after a poll: still controllable")
	}
	if _, _, err := e.s.CreateControl(context.Background(), "sess-a", api.RequestedByDashboard); !errors.Is(err, ErrNotControllable) {
		t.Errorf("create with a silent watcher: %v", err)
	}
}

func TestCreateControlLimit(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.poll(e.tower)
	for i := range MaxPendingPerMachine + 1 {
		e.session(e.tower, fmt.Sprintf("limit-%02d", i), fmt.Sprintf("w1:p%d", i+1))
	}
	for i := range MaxPendingPerMachine {
		e.create(fmt.Sprintf("limit-%02d", i))
	}
	if _, _, err := e.s.CreateControl(ctx, "limit-10", api.RequestedByDashboard); !errors.Is(err, ErrTooMany) || !strings.Contains(err.Error(), "10 pending") {
		t.Fatalf("11th create: %v", err)
	}
	if n := e.count(`SELECT COUNT(*) FROM control_requests`); n != MaxPendingPerMachine {
		t.Errorf("%d rows after the refused create, want %d", n, MaxPendingPerMachine)
	}
	// Another machine has its own limit.
	e.session(e.bluebox, "bluebox-a", "w1:p1")
	e.poll(e.bluebox)
	e.create("bluebox-a")
	// A claim frees a slot.
	if _, ok, err := e.s.ClaimControl(ctx, e.tower.ID); !ok || err != nil {
		t.Fatalf("claim: %v %v", ok, err)
	}
	e.create("limit-10")
}

func TestClaimControlOrderAndOwner(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.poll(e.tower)
	e.session(e.tower, "sess-a", "w1:p1")
	e.session(e.tower, "sess-b", "w1:p2")
	a := e.create("sess-a")
	e.clock.Advance(time.Second)
	b := e.create("sess-b")

	if _, ok, err := e.s.ClaimControl(ctx, e.bluebox.ID); ok || err != nil {
		t.Errorf("bluebox claimed tower's request: ok=%v err=%v", ok, err)
	}
	for _, want := range []api.ControlRequest{a, b} {
		c, ok, err := e.s.ClaimControl(ctx, e.tower.ID)
		if err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		if c.Request.ID != want.ID || c.Request.State != api.ControlClaimed || c.Request.ClaimedAt == nil ||
			!c.Request.ClaimedAt.Equal(e.clock.Now()) || c.Session.ID != want.SessionID || c.Session.HerdrPane == "" {
			t.Errorf("claim: %+v, want request %s", c, want.ID)
		}
	}
	if _, ok, err := e.s.ClaimControl(ctx, e.tower.ID); ok || err != nil {
		t.Errorf("third claim: ok=%v err=%v", ok, err)
	}
	if x := e.get("sess-a"); x.RemoteControl.State != api.ControlClaimed {
		t.Errorf("session reads %s, want claimed", x.RemoteControl.State)
	}
}

func TestControlExpiry(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.poll(e.tower)
	e.session(e.tower, "sess-a", "w1:p1")
	e.session(e.tower, "sess-b", "w1:p2")

	// Pending past expires_at: reads say expired, a claim skips it, and a
	// new create opens a fresh request.
	a := e.create("sess-a")
	e.clock.Advance(2 * time.Minute)
	e.poll(e.tower)
	if x := e.get("sess-a"); x.RemoteControl.State != api.ControlExpired {
		t.Errorf("pending after 2m: %s", x.RemoteControl.State)
	}
	if _, ok, _ := e.s.ClaimControl(ctx, e.tower.ID); ok {
		t.Error("an expired request was claimed")
	}
	if a2 := e.create("sess-a"); a2.ID == a.ID {
		t.Error("create returned the expired request")
	}

	// Claimed past expires_at: reads say expired, and a result is refused.
	b := e.create("sess-b")
	// sess-a's fresh request is older, so claim twice to reach sess-b's.
	e.s.ClaimControl(ctx, e.tower.ID)
	if c, ok, err := e.s.ClaimControl(ctx, e.tower.ID); !ok || err != nil || c.Request.ID != b.ID {
		t.Fatalf("claim b: %+v %v %v", c, ok, err)
	}
	e.clock.Advance(2 * time.Minute)
	if x := e.get("sess-b"); x.RemoteControl.State != api.ControlExpired {
		t.Errorf("claimed after 2m: %s", x.RemoteControl.State)
	}
	_, err := e.s.FinishControl(ctx, e.tower.ID, b.ID, api.ControlResultIn{State: api.ControlDone, URL: testLink})
	if !errors.Is(err, ErrRequestClosed) || !strings.Contains(err.Error(), "expired") {
		t.Errorf("late result: %v", err)
	}
	if x := e.get("sess-b"); x.RemoteControlURL != "" || x.RemoteControlAt != nil {
		t.Errorf("a late result set the link: %q", x.RemoteControlURL)
	}
	if n := len(e.events("sess-b", api.KindRemoteControlResult)); n != 0 {
		t.Errorf("a late result recorded %d events", n)
	}
}

func TestFinishControl(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.poll(e.tower)
	e.session(e.tower, "sess-a", "w1:p1")
	r := e.create("sess-a")

	if _, err := e.s.FinishControl(ctx, e.tower.ID, r.ID, api.ControlResultIn{State: api.ControlDone}); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("result before the claim: %v", err)
	}
	if _, ok, err := e.s.ClaimControl(ctx, e.tower.ID); !ok || err != nil {
		t.Fatal(ok, err)
	}
	bad := []struct {
		name string
		id   string
		in   api.ControlResultIn
		want error
	}{
		{"malformed ID", "cr_short", api.ControlResultIn{State: "done"}, ErrInvalid},
		{"state pending", r.ID, api.ControlResultIn{State: "pending"}, ErrInvalid},
		{"http", r.ID, api.ControlResultIn{State: "done", URL: "http://claude.ai/code/session_abc"}, ErrInvalid},
		{"other host", r.ID, api.ControlResultIn{State: "done", URL: "https://claude.ai.evil.example/code/session_abc"}, ErrInvalid},
		{"extra path", r.ID, api.ControlResultIn{State: "done", URL: "https://claude.ai/code/session_abc/x"}, ErrInvalid},
		{"query", r.ID, api.ControlResultIn{State: "done", URL: "https://claude.ai/code/session_abc?x=1"}, ErrInvalid},
		{"empty session", r.ID, api.ControlResultIn{State: "done", URL: "https://claude.ai/code/session_"}, ErrInvalid},
		{"trailing newline", r.ID, api.ControlResultIn{State: "done", URL: testLink + "\n"}, ErrInvalid},
		{"URL on failed", r.ID, api.ControlResultIn{State: "failed", URL: testLink}, ErrInvalid},
		{"control character", r.ID, api.ControlResultIn{State: "failed", Detail: "a\x1b[31mb"}, ErrInvalid},
		{"detail too long", r.ID, api.ControlResultIn{State: "failed", Detail: strings.Repeat("x", 201)}, ErrInvalid},
		{"unknown request", "cr_AAAAAAAAAAAAAAAAAAAAAA", api.ControlResultIn{State: "done"}, ErrNotFound},
	}
	for _, tc := range bad {
		if _, err := e.s.FinishControl(ctx, e.tower.ID, tc.id, tc.in); !errors.Is(err, tc.want) {
			t.Errorf("%s: err %v, want %v", tc.name, err, tc.want)
		}
	}
	if _, err := e.s.FinishControl(ctx, e.bluebox.ID, r.ID, api.ControlResultIn{State: api.ControlDone}); !errors.Is(err, ErrWrongMachine) {
		t.Errorf("another machine's result: %v", err)
	}
	if x := e.get("sess-a"); x.RemoteControl.State != api.ControlClaimed || x.RemoteControlURL != "" {
		t.Fatalf("refused results changed the request: %+v", x.RemoteControl)
	}

	e.clock.Advance(3 * time.Second)
	got, err := e.s.FinishControl(ctx, e.tower.ID, r.ID, api.ControlResultIn{State: api.ControlDone, URL: testLink, Detail: "resumed in a new herdr workspace"})
	if err != nil {
		t.Fatal(err)
	}
	now := e.clock.Now()
	if got.State != api.ControlDone || got.URL != testLink || got.FinishedAt == nil || !got.FinishedAt.Equal(now) {
		t.Errorf("finished request: %+v", got)
	}
	x := e.get("sess-a")
	if x.RemoteControlURL != testLink || x.RemoteControlAt == nil || !x.RemoteControlAt.Equal(now) || x.RemoteControl.State != api.ControlDone {
		t.Errorf("session after done: url %q at %v request %+v", x.RemoteControlURL, x.RemoteControlAt, x.RemoteControl)
	}
	evs := e.events("sess-a", api.KindRemoteControlResult)
	var p map[string]string
	if len(evs) != 1 || evs[0].Source != api.SourcePlugin || json.Unmarshal(evs[0].Payload, &p) != nil ||
		p["request_id"] != r.ID || p["state"] != "done" || p["url"] != testLink {
		t.Errorf("result events: %+v", evs)
	}
	if _, err := e.s.FinishControl(ctx, e.tower.ID, r.ID, api.ControlResultIn{State: api.ControlFailed}); !errors.Is(err, ErrRequestClosed) {
		t.Errorf("second result: %v", err)
	}
}

// The last link goes away in each write path that ends a session.
func TestRemoteControlLinkClearedWhenSessionEnds(t *testing.T) {
	e := newControlEnv(t)
	ctx := context.Background()
	e.poll(e.tower)
	link := func(id string) {
		e.t.Helper()
		e.session(e.tower, id, "w1:"+id)
		r := e.create(id)
		if _, ok, err := e.s.ClaimControl(ctx, e.tower.ID); !ok || err != nil {
			t.Fatal(ok, err)
		}
		if _, err := e.s.FinishControl(ctx, e.tower.ID, r.ID, api.ControlResultIn{State: api.ControlDone, URL: testLink}); err != nil {
			t.Fatal(err)
		}
		if e.get(id).RemoteControlURL != testLink {
			t.Fatalf("%s has no link", id)
		}
	}
	link("p1")
	link("p2")
	link("p3")
	link("p4")

	// A state change keeps the link.
	if err := e.s.AddEvent(ctx, e.tower.ID, "p4", api.EventIn{Kind: api.KindStateChanged, Source: api.SourcePlugin,
		Payload: json.RawMessage(`{"agent_state":"working"}`)}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{api.KindPaneClosed, api.KindEnded} {
		id := map[string]string{api.KindPaneClosed: "p1", api.KindEnded: "p2"}[kind]
		if err := e.s.AddEvent(ctx, e.tower.ID, id, api.EventIn{Kind: kind, Source: api.SourcePlugin}); err != nil {
			t.Fatal(err)
		}
	}
	// p3 is missing from a snapshot that lists p4.
	if _, err := e.s.ReconcileHerdr(ctx, e.tower.ID, api.HerdrSessionsPut{HerdrSession: "default",
		Sessions: []api.SessionUpsert{{ID: "p4", Agent: "claude", HerdrPane: "w1:p4"}}}); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"p1": "", "p2": "", "p3": "", "p4": testLink} {
		x := e.get(id)
		if x.RemoteControlURL != want || (want == "") != (x.RemoteControlAt == nil) {
			t.Errorf("%s: link %q at %v, want %q", id, x.RemoteControlURL, x.RemoteControlAt, want)
		}
	}
}

// A v2 database upgrades to v3 in place and keeps its rows.
func TestMigrateV2Database(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessionhub.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := s.AddMachine(ctx, "tower", "", "")
	if err != nil {
		t.Fatal(err)
	}
	m, _ := s.MachineByToken(ctx, tok)
	if _, err := s.UpsertSession(ctx, m.ID, api.SessionUpsert{ID: "s1", Source: api.SourcePlugin, HerdrSession: "default", HerdrPane: "w1:p1"}); err != nil {
		t.Fatal(err)
	}
	rollbackV3(t, s)
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 2"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var v int
	if err := s2.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil || v != 3 {
		t.Errorf("user_version %d %v, want 3", v, err)
	}
	if err := s2.RecordPoll(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, created, err := s2.CreateControl(ctx, "s1", api.RequestedByDashboard); err != nil || !created {
		t.Errorf("create after migration: %v %v", created, err)
	}
}
```

- [ ] **Step 2: Update the schema and migration tests to v3**

In `internal/store/store_test.go`, `TestOpenPragmasAndSchema`: change `{"user_version", "2"}` to `{"user_version", "3"}`, add `"control_requests"` to the table loop, and replace the `want` map with:

```go
	want := map[string]string{
		"machines":         "herdr_host id last_poll last_seen name ssh_host token_hash",
		"sessions":         "agent agent_state cwd ended_at first_prompt git_branch git_repo herdr_pane herdr_session herdr_workspace id last_seen_at machine_id rc_at rc_url started_at state_ts title title_source",
		"events":           "id kind payload_json session_id source ts",
		"reports":          "done_json id in_flight_json note session_id ts waiting_on_json",
		"control_requests": "action claimed_at created_at detail expires_at finished_at id machine_id requested_by session_id state url",
	}
```

The two tests that roll a file back to v1 drop only `state_ts` today; with v3 the migration would then fail on `table control_requests already exists`. Add this helper at the end of `internal/store/concurrency_test.go`:

```go
// rollbackV3 removes what schema v3 added, so a test can set user_version
// to 1 or 2 and reopen the file as that older release left it.
func rollbackV3(t *testing.T, s *Store) {
	t.Helper()
	for _, q := range []string{
		"DROP TABLE control_requests",
		"ALTER TABLE machines DROP COLUMN last_poll",
		"ALTER TABLE sessions DROP COLUMN rc_url",
		"ALTER TABLE sessions DROP COLUMN rc_at",
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}
```

In `TestMigrateV1Database` (`concurrency_test.go`) and `TestConcurrentOpenOfV1Database` (`migrate_race_test.go`), call `rollbackV3(t, s)` right before the existing `ALTER TABLE sessions DROP COLUMN state_ts` line.

- [ ] **Step 3: Run the tests to see them fail**

Run: `GOFLAGS=-count=1 go test ./internal/store/`
Expected: FAIL to compile, with errors such as `undefined: ErrNotControllable`, `s.RecordPoll undefined`, and `api.KindRemoteControlRequested undefined`.

- [ ] **Step 4: Add the API types**

In `internal/api/types.go`, add after the existing `const (...)` block:

```go
// Remote Control requests. See docs/server.md, "Remote Control requests".
const (
	// SourceServer marks events the server records itself.
	SourceServer = "server"

	KindRemoteControlRequested = "remote_control_requested"
	KindRemoteControlResult    = "remote_control_result"

	// ActionRemoteControl is the only action a control request carries.
	ActionRemoteControl = "remote_control"

	ControlPending = "pending"
	ControlClaimed = "claimed"
	ControlDone    = "done"
	ControlFailed  = "failed"
	ControlExpired = "expired"

	// ControlRequest.RequestedBy is RequestedByDashboard, or
	// RequestedByMachinePrefix followed by the machine name.
	RequestedByDashboard     = "dashboard"
	RequestedByMachinePrefix = "machine:"

	// The dashboard sends HeaderAction: HeaderActionRemoteControl with its
	// one write, POST /v1/sessions/{id}/remote-control.
	HeaderAction              = "X-Hub-Action"
	HeaderActionRemoteControl = "remote-control"

	// MaxControlDetailRunes caps ControlResultIn.Detail.
	MaxControlDetailRunes = 200
)

// ControlRequest is one request to act on a session on its machine.
type ControlRequest struct {
	ID          string     `json:"id"`
	SessionID   string     `json:"session_id"`
	Machine     string     `json:"machine"`
	Action      string     `json:"action"`
	State       string     `json:"state"`
	RequestedBy string     `json:"requested_by"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	ClaimedAt   *time.Time `json:"claimed_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	URL         string     `json:"url,omitempty"`
	Detail      string     `json:"detail,omitempty"`
}

// ControlResultIn is the body of POST /v1/control/{id}/result.
type ControlResultIn struct {
	State  string `json:"state"` // done|failed
	URL    string `json:"url,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// ControlClaim is a request the machine just claimed, with its session: the
// body of a 200 from GET /v1/machines/self/control.
type ControlClaim struct {
	Request ControlRequest `json:"request"`
	Session Session        `json:"session"`
}
```

Add these fields to the end of `type Session struct`:

```go
	// RemoteControl is the session's latest control request.
	RemoteControl *ControlRequest `json:"remote_control,omitempty"`
	// RemoteControlURL and RemoteControlAt are the last Remote Control link
	// a machine reported, cleared when the session ends.
	RemoteControlURL string     `json:"remote_control_url,omitempty"`
	RemoteControlAt  *time.Time `json:"remote_control_at,omitempty"`
	// Controllable is true when the session has a herdr pane and its
	// machine's watcher polled for requests in the last 2 minutes.
	Controllable bool `json:"controllable"`
```

- [ ] **Step 5: Add schema v3 and the errors**

In `internal/store/store.go`, extend the errors block:

```go
var (
	ErrNotFound     = errors.New("not found")
	ErrInvalid      = errors.New("invalid")
	ErrWrongMachine = errors.New("conflict")
	// ErrNotControllable: the session has no herdr pane, or its machine's
	// watcher is offline. The server answers 409.
	ErrNotControllable = errors.New("not controllable")
	// ErrTooMany: the machine has MaxPendingPerMachine pending requests. 429.
	ErrTooMany = errors.New("too many requests")
	// ErrRequestClosed: a result for a request that is not claimed. 409.
	ErrRequestClosed = errors.New("request closed")
)
```

Change `const schemaVersion = 2` to `const schemaVersion = 3`, add after `schemaV2`:

```go
// schemaV3 adds Remote Control requests: the control_requests table,
// machines.last_poll (the watcher's last long poll, which makes the
// machine's sessions controllable), and the session's last Remote Control
// link.
const schemaV3 = `
CREATE TABLE control_requests (
	id           TEXT PRIMARY KEY,        -- cr_ + 16 random bytes, base64url
	session_id   TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	machine_id   INTEGER NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
	action       TEXT NOT NULL,           -- remote_control
	state        TEXT NOT NULL,           -- pending|claimed|done|failed|expired
	requested_by TEXT NOT NULL,           -- dashboard | machine:<name>
	created_at   TEXT NOT NULL,
	expires_at   TEXT NOT NULL,
	claimed_at   TEXT,
	finished_at  TEXT,
	url          TEXT NOT NULL DEFAULT '',
	detail       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX control_requests_machine ON control_requests(machine_id, state, created_at);
CREATE INDEX control_requests_session ON control_requests(session_id, created_at);
ALTER TABLE machines ADD COLUMN last_poll TEXT;
ALTER TABLE sessions ADD COLUMN rc_url TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN rc_at TEXT;
`
```

In `migrate`, after the `if v < 2 {...}` block:

```go
	if v < 3 {
		if _, err := tx.ExecContext(ctx, schemaV3); err != nil {
			return fmt.Errorf("migrate schema to v3: %w", err)
		}
	}
```

- [ ] **Step 6: Write the control request store code**

Create `internal/store/control.go`:

```go
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// Control request limits.
const (
	// ControlTTL is how long a request stays open, pending or claimed.
	ControlTTL = 2 * time.Minute
	// ControlPollWindow is how recent a machine's last poll must be for its
	// sessions to be controllable.
	ControlPollWindow = 2 * time.Minute
	// MaxPendingPerMachine caps one machine's pending requests.
	MaxPendingPerMachine = 10
)

var (
	controlIDRE = regexp.MustCompile(`^cr_[A-Za-z0-9_-]{22}$`)
	// remoteControlURLRE is the only link a result may carry.
	remoteControlURLRE = regexp.MustCompile(`^https://claude\.ai/code/session_[A-Za-z0-9_-]+$`)
)

// ValidControlID reports whether id has the shape of a control request ID.
func ValidControlID(id string) bool { return controlIDRE.MatchString(id) }

// ValidRemoteControlURL reports whether u is a claude.ai Remote Control link.
func ValidRemoteControlURL(u string) bool { return remoteControlURLRE.MatchString(u) }

func newControlID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "cr_" + base64.RawURLEncoding.EncodeToString(b), nil
}

// effectiveState is the state a read reports: a pending or claimed request
// past expires_at is expired, whether or not a write has stored that yet.
func effectiveState(state string, expires, now time.Time) string {
	if (state == api.ControlPending || state == api.ControlClaimed) && !now.Before(expires) {
		return api.ControlExpired
	}
	return state
}

// expireTx stores the expiry of every open request past expires_at. Create,
// claim, and result run it first; reads use effectiveState instead.
func expireTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	n := formatTS(now)
	_, err := tx.ExecContext(ctx, `UPDATE control_requests SET state = ?, finished_at = ?
		WHERE state IN (?, ?) AND expires_at <= ?`,
		api.ControlExpired, n, api.ControlPending, api.ControlClaimed, n)
	return err
}

// nullControl is a control_requests row, possibly read through a LEFT JOIN.
type nullControl struct {
	id, action, state, requestedBy, created, expires, claimed, finished, url, detail sql.NullString
}

// request returns the row as a request, or nil when the join found none.
func (n nullControl) request(sessionID, machine string, now time.Time) (*api.ControlRequest, error) {
	if !n.id.Valid {
		return nil, nil
	}
	r := api.ControlRequest{ID: n.id.String, SessionID: sessionID, Machine: machine, Action: n.action.String,
		State: n.state.String, RequestedBy: n.requestedBy.String, URL: n.url.String, Detail: n.detail.String}
	var err error
	if r.CreatedAt, err = parseTS(n.created.String); err != nil {
		return nil, err
	}
	if r.ExpiresAt, err = parseTS(n.expires.String); err != nil {
		return nil, err
	}
	if r.ClaimedAt, err = parseNullTS(n.claimed); err != nil {
		return nil, err
	}
	if r.FinishedAt, err = parseNullTS(n.finished); err != nil {
		return nil, err
	}
	r.State = effectiveState(r.State, r.ExpiresAt, now)
	return &r, nil
}

// controlSelect reads a request and the name of its machine.
const controlSelect = `SELECT c.id, c.action, c.state, c.requested_by, c.created_at, c.expires_at,
	c.claimed_at, c.finished_at, c.url, c.detail, c.session_id, m.name
FROM control_requests c JOIN machines m ON m.id = c.machine_id`

func scanControl(sc scanner, now time.Time) (api.ControlRequest, error) {
	var n nullControl
	var sessionID, machine string
	if err := sc.Scan(&n.id, &n.action, &n.state, &n.requestedBy, &n.created, &n.expires,
		&n.claimed, &n.finished, &n.url, &n.detail, &sessionID, &machine); err != nil {
		return api.ControlRequest{}, err
	}
	r, err := n.request(sessionID, machine, now)
	if err != nil {
		return api.ControlRequest{}, err
	}
	return *r, nil
}

// RecordPoll stores now as the machine's last long poll.
func (s *Store) RecordPoll(ctx context.Context, machineID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE machines SET last_poll = ? WHERE id = ?`, formatTS(s.Now()), machineID)
	return err
}

// CreateControl opens a Remote Control request for session id, or returns
// the session's open one with created false. requestedBy is
// api.RequestedByDashboard or api.RequestedByMachinePrefix + <name>. Checks
// run in this order: unknown session (ErrNotFound), no herdr pane or an
// offline watcher (ErrNotControllable), an open request (returned), and
// MaxPendingPerMachine (ErrTooMany).
func (s *Store) CreateControl(ctx context.Context, id, requestedBy string) (api.ControlRequest, bool, error) {
	if !ValidSessionID(id) {
		return api.ControlRequest{}, false, invalidf("session id %q: use 1-128 letters, digits, '_', or '-', starting with a letter or digit", id)
	}
	if requestedBy != api.RequestedByDashboard &&
		!(strings.HasPrefix(requestedBy, api.RequestedByMachinePrefix) && ValidMachineName(strings.TrimPrefix(requestedBy, api.RequestedByMachinePrefix))) {
		return api.ControlRequest{}, false, invalidf("requested_by %q: want dashboard or machine:<name>", requestedBy)
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.ControlRequest{}, false, err
	}
	defer tx.Rollback()
	if err := expireTx(ctx, tx, now); err != nil {
		return api.ControlRequest{}, false, err
	}
	var machineID int64
	var machine, pane string
	var lastPoll sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT s.machine_id, m.name, s.herdr_pane, m.last_poll
		FROM sessions s JOIN machines m ON m.id = s.machine_id WHERE s.id = ?`, id).
		Scan(&machineID, &machine, &pane, &lastPoll)
	if errors.Is(err, sql.ErrNoRows) {
		return api.ControlRequest{}, false, fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	if err != nil {
		return api.ControlRequest{}, false, err
	}
	if pane == "" {
		return api.ControlRequest{}, false, fmt.Errorf("%w: session %s is not in herdr", ErrNotControllable, id)
	}
	polled, err := parseNullTS(lastPoll)
	if err != nil {
		return api.ControlRequest{}, false, err
	}
	if polled == nil || now.Sub(*polled) > ControlPollWindow {
		return api.ControlRequest{}, false, fmt.Errorf("%w: the sessionhub watcher on machine %q is offline (no poll in the last %s)",
			ErrNotControllable, machine, ControlPollWindow)
	}
	open, err := scanControl(tx.QueryRowContext(ctx, controlSelect+` WHERE c.session_id = ? AND c.state IN (?, ?)
		ORDER BY c.created_at DESC, c.rowid DESC LIMIT 1`, id, api.ControlPending, api.ControlClaimed), now)
	if err == nil {
		return open, false, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return api.ControlRequest{}, false, err
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_requests WHERE machine_id = ? AND state = ?`,
		machineID, api.ControlPending).Scan(&pending); err != nil {
		return api.ControlRequest{}, false, err
	}
	if pending >= MaxPendingPerMachine {
		return api.ControlRequest{}, false, fmt.Errorf("%w: machine %q already has %d pending requests", ErrTooMany, machine, pending)
	}
	rid, err := newControlID()
	if err != nil {
		return api.ControlRequest{}, false, err
	}
	r := api.ControlRequest{ID: rid, SessionID: id, Machine: machine, Action: api.ActionRemoteControl,
		State: api.ControlPending, RequestedBy: requestedBy, CreatedAt: now, ExpiresAt: now.Add(ControlTTL)}
	if _, err := tx.ExecContext(ctx, `INSERT INTO control_requests (id, session_id, machine_id, action, state,
		requested_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, id, machineID, r.Action, r.State, r.RequestedBy, formatTS(r.CreatedAt), formatTS(r.ExpiresAt)); err != nil {
		return api.ControlRequest{}, false, err
	}
	payload, err := json.Marshal(map[string]string{"request_id": r.ID, "requested_by": requestedBy})
	if err != nil {
		return api.ControlRequest{}, false, err
	}
	if err := insertEventTx(ctx, tx, id, now, api.SourceServer, api.KindRemoteControlRequested, payload); err != nil {
		return api.ControlRequest{}, false, err
	}
	return r, true, tx.Commit()
}

// ClaimControl marks the machine's oldest pending request claimed and
// returns it with its session. ok is false when nothing is pending.
func (s *Store) ClaimControl(ctx context.Context, machineID int64) (api.ControlClaim, bool, error) {
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.ControlClaim{}, false, err
	}
	defer tx.Rollback()
	if err := expireTx(ctx, tx, now); err != nil {
		return api.ControlClaim{}, false, err
	}
	r, err := scanControl(tx.QueryRowContext(ctx, controlSelect+` WHERE c.machine_id = ? AND c.state = ?
		ORDER BY c.created_at, c.rowid LIMIT 1`, machineID, api.ControlPending), now)
	if errors.Is(err, sql.ErrNoRows) {
		return api.ControlClaim{}, false, tx.Commit()
	}
	if err != nil {
		return api.ControlClaim{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE control_requests SET state = ?, claimed_at = ? WHERE id = ?`,
		api.ControlClaimed, formatTS(now), r.ID); err != nil {
		return api.ControlClaim{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return api.ControlClaim{}, false, err
	}
	r.State, r.ClaimedAt = api.ControlClaimed, &now
	// GetSession uses s.db, so it runs after the commit: inside the
	// transaction it would wait forever for the one connection.
	sess, err := s.GetSession(ctx, r.SessionID)
	if err != nil {
		return api.ControlClaim{}, false, err
	}
	return api.ControlClaim{Request: r, Session: sess}, true, nil
}

// FinishControl records the result of a request that machineID claimed. A
// done result with a URL also sets the session's last link.
func (s *Store) FinishControl(ctx context.Context, machineID int64, reqID string, in api.ControlResultIn) (api.ControlRequest, error) {
	if !ValidControlID(reqID) {
		return api.ControlRequest{}, invalidf("request id %q: want cr_ and 22 base64url characters", reqID)
	}
	if in.State != api.ControlDone && in.State != api.ControlFailed {
		return api.ControlRequest{}, invalidf("state %q: want done or failed", in.State)
	}
	if in.URL != "" && (in.State != api.ControlDone || !ValidRemoteControlURL(in.URL)) {
		return api.ControlRequest{}, invalidf("url %q: want https://claude.ai/code/session_<id>, on a done result only", in.URL)
	}
	if err := checkText("detail", in.Detail, api.MaxControlDetailRunes); err != nil {
		return api.ControlRequest{}, err
	}
	now := s.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.ControlRequest{}, err
	}
	defer tx.Rollback()
	if err := expireTx(ctx, tx, now); err != nil {
		return api.ControlRequest{}, err
	}
	var owner int64
	var state, sessionID string
	err = tx.QueryRowContext(ctx, `SELECT machine_id, state, session_id FROM control_requests WHERE id = ?`, reqID).
		Scan(&owner, &state, &sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return api.ControlRequest{}, fmt.Errorf("%w: request %s", ErrNotFound, reqID)
	}
	if err != nil {
		return api.ControlRequest{}, err
	}
	if owner != machineID {
		return api.ControlRequest{}, fmt.Errorf("%w: request %s belongs to another machine", ErrWrongMachine, reqID)
	}
	if state != api.ControlClaimed {
		return api.ControlRequest{}, fmt.Errorf("%w: request %s is %s, not claimed", ErrRequestClosed, reqID, state)
	}
	nowS := formatTS(now)
	if _, err := tx.ExecContext(ctx, `UPDATE control_requests SET state = ?, finished_at = ?, url = ?, detail = ? WHERE id = ?`,
		in.State, nowS, in.URL, in.Detail, reqID); err != nil {
		return api.ControlRequest{}, err
	}
	if in.URL != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET rc_url = ?, rc_at = ? WHERE id = ?`, in.URL, nowS, sessionID); err != nil {
			return api.ControlRequest{}, err
		}
	}
	p := map[string]string{"request_id": reqID, "state": in.State}
	if in.URL != "" {
		p["url"] = in.URL
	}
	if in.Detail != "" {
		p["detail"] = in.Detail
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return api.ControlRequest{}, err
	}
	if err := insertEventTx(ctx, tx, sessionID, now, api.SourcePlugin, api.KindRemoteControlResult, payload); err != nil {
		return api.ControlRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.ControlRequest{}, err
	}
	return scanControl(s.db.QueryRowContext(ctx, controlSelect+` WHERE c.id = ?`, reqID), now)
}
```

- [ ] **Step 7: Read the new fields with each session**

In `internal/store/read.go`, replace `sessionSelect` with:

```go
// sessionSelect reads a session, its machine, its latest report, and its
// latest control request.
const sessionSelect = `SELECT s.id, s.agent, m.name, m.ssh_host, s.cwd, s.git_repo, s.git_branch,
	s.herdr_session, s.herdr_workspace, s.herdr_pane, s.title, s.title_source, s.agent_state,
	s.started_at, s.last_seen_at, s.ended_at,
	r.ts, r.done_json, r.in_flight_json, r.waiting_on_json, r.note,
	m.last_poll, s.rc_url, s.rc_at,
	c.id, c.action, c.state, c.requested_by, c.created_at, c.expires_at, c.claimed_at, c.finished_at, c.url, c.detail
FROM sessions s
JOIN machines m ON m.id = s.machine_id
LEFT JOIN reports r ON r.id = (SELECT id FROM reports WHERE session_id = s.id ORDER BY id DESC LIMIT 1)
LEFT JOIN control_requests c ON c.id = (SELECT id FROM control_requests WHERE session_id = s.id
	ORDER BY created_at DESC, rowid DESC LIMIT 1)`
```

In `scanSession`, add `lastPoll, rcAt` to the `sql.NullString` declarations, declare `var rc nullControl`, extend the `Scan` call after `&rNote` with:

```go
		&lastPoll, &x.RemoteControlURL, &rcAt,
		&rc.id, &rc.action, &rc.state, &rc.requestedBy, &rc.created, &rc.expires, &rc.claimed, &rc.finished, &rc.url, &rc.detail)
```

and add before the final `return x, nil`:

```go
	if x.RemoteControlAt, err = parseNullTS(rcAt); err != nil {
		return x, err
	}
	polled, err := parseNullTS(lastPoll)
	if err != nil {
		return x, err
	}
	x.Controllable = x.HerdrPane != "" && polled != nil && now.Sub(*polled) <= ControlPollWindow
	if x.RemoteControl, err = rc.request(x.ID, x.Machine, now); err != nil {
		return x, err
	}
```

- [ ] **Step 8: Clear the link in the three paths that end a session**

In `internal/store/sessions.go`, `AddEvent`, replace:

```go
	q := "UPDATE sessions SET last_seen_at = ?, ended_at = " + ended
	args := []any{nowS}
	if ended != "NULL" {
		args = append(args, nowS)
	}
```

with:

```go
	q := "UPDATE sessions SET last_seen_at = ?, ended_at = " + ended
	args := []any{nowS}
	if ended != "NULL" {
		// The session ended (pane_closed or ended): its last Remote Control
		// link goes with it.
		q += ", rc_url = '', rc_at = NULL"
		args = append(args, nowS)
	}
```

In `ReconcileHerdr`, replace `UPDATE sessions SET ended_at = ? WHERE id = ?` with `UPDATE sessions SET ended_at = ?, rc_url = '', rc_at = NULL WHERE id = ?`.

- [ ] **Step 9: Run the store tests**

Run: `GOFLAGS=-count=1 go test ./internal/store/`
Expected: `ok  	session-hub/internal/store`.

Then run: `make test lint`
Expected: every package `ok`, and `lint` prints nothing after its two commands. `internal/server`'s `TestUpsertRules` compares two reads of one session, so the new fields match on both sides.

- [ ] **Step 10: Commit**

```bash
git add internal/api/types.go internal/store
git commit -F - <<'MSG'
Add control requests to the store (schema v3)

Schema v3 adds control_requests, machines.last_poll, and the session's
last Remote Control link (rc_url, rc_at). The store creates a request (one
open request per session, 10 pending per machine), claims the oldest
pending one per machine, and records a result from the claiming machine
only, with a strict claude.ai link. Expiry is lazy: reads report expired
from expires_at, and each write stores it first. Sessions now read their
latest request, their last link, and whether they are controllable.

Gotcha: the v1 rollback in the migration tests must drop the v3 additions
too, or the upgrade fails on "table control_requests already exists".

Docs: Task 7 updates docs/server.md (Database, Remote Control requests).

Refs: docs/dev/superpowers/plans/2026-09-30-remote-control-button.md Task 1

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
MSG
```

---
### Task 2: Server endpoints and the auth matrix

**Files:**
- Create: `internal/server/control.go`
- Modify: `internal/server/routes.go` (`accessAction` constant; three routes)
- Modify: `internal/server/server.go` (`Server` fields, `New`, `principal`, `authenticate`, `actor`, `statusWriter.Unwrap`, `storeError`)
- Modify: `internal/server/run.go` (`runWithListeners`: register `StopPolls` on shutdown)
- Modify: `internal/server/helpers_test.go` (`doHdr` and control helpers)
- Modify: `internal/server/auth_test.go` (`authCase`, `TestAuthMatrix`)
- Create: `internal/server/control_test.go`

**Interfaces:**
- Consumes (Task 1): `store.CreateControl`, `store.ClaimControl`, `store.FinishControl`, `store.RecordPoll`, `store.ValidControlID`, `store.ErrNotControllable`, `store.ErrTooMany`, `store.ErrRequestClosed`, `store.ErrWrongMachine`; `api.ControlRequest`, `api.ControlClaim`, `api.ControlResultIn`, `api.HeaderAction`, `api.HeaderActionRemoteControl`, `api.RequestedByDashboard`, `api.RequestedByMachinePrefix`.
- Produces:
  - Routes: `POST /v1/sessions/{id}/remote-control` → `200`/`202` with an `api.ControlRequest`; `GET /v1/machines/self/control?wait=N` → `200` with an `api.ControlClaim`, or `204` with no body; `POST /v1/control/{id}/result` → `200` with the finished `api.ControlRequest`. Errors are `api.Error` JSON as elsewhere.
  - `func (s *Server) StopPolls()`: ends every open poll with `204`; later polls answer `204` at once.
  - Test seam: `Server.after func(time.Duration) <-chan time.Time` (default `time.After`), the long poll's timer.

- [ ] **Step 1: Add the test helpers**

In `internal/server/helpers_test.go`, replace `doWith` with a thin wrapper and add `doHdr` and the control helpers. The file already imports everything they use (`context`, `sync`, `time`, `store`). New code:

```go
// doWith is do plus an optional hub_read cookie value.
func (e *env) doWith(method, path, token, cookie string, body any) (int, []byte, http.Header) {
	e.t.Helper()
	return e.doHdr(method, path, token, cookie, nil, body)
}

// doHdr is doWith plus extra request headers.
func (e *env) doHdr(method, path, token, cookie string, hdr map[string]string, body any) (int, []byte, http.Header) {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		j, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(j)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: readCookie, Value: cookie})
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp.StatusCode, out, resp.Header
}

// machine returns the store's machine for a token.
func (e *env) machine(token string) store.Machine {
	e.t.Helper()
	m, err := e.st.MachineByToken(context.Background(), token)
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

// recordPoll marks the token's machine as polling now, as its watcher would.
func (e *env) recordPoll(token string) {
	e.t.Helper()
	if err := e.st.RecordPoll(context.Background(), e.machine(token).ID); err != nil {
		e.t.Fatal(err)
	}
}

// controllable registers id on tower with a pane and records a tower poll.
func (e *env) controllable(id, pane string) {
	e.t.Helper()
	e.register(e.tokA, api.SessionUpsert{ID: id, HerdrSession: "default", HerdrPane: pane, CWD: "/home/user/proj"})
	e.recordPoll(e.tokA)
}

// tap sends the dashboard's one write: the read cookie and X-Hub-Action.
func (e *env) tap(id string) (int, api.ControlRequest, []byte) {
	e.t.Helper()
	code, b, _ := e.doHdr("POST", "/v1/sessions/"+id+"/remote-control", "", testReadToken,
		map[string]string{api.HeaderAction: api.HeaderActionRemoteControl}, nil)
	var r api.ControlRequest
	json.Unmarshal(b, &r)
	return code, r, b
}

// instantTimer is a Server.after whose timer has already fired.
func instantTimer(time.Duration) <-chan time.Time {
	c := make(chan time.Time, 1)
	c <- time.Time{}
	return c
}

// fakeTimers is a Server.after that records each wait and fires only when
// the test says so.
type fakeTimers struct {
	mu    sync.Mutex
	waits []time.Duration
	chans []chan time.Time
}

func (f *fakeTimers) after(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := make(chan time.Time, 1)
	f.waits = append(f.waits, d)
	f.chans = append(f.chans, c)
	return c
}

// await blocks until n timers exist.
func (f *fakeTimers) await(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		got := len(f.chans)
		f.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d poll timers after 5 s, want %d", got, n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (f *fakeTimers) fire(i int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chans[i] <- time.Time{}
}

func (f *fakeTimers) wait(i int) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.waits[i]
}

type pollResult struct {
	code int
	body []byte
	err  error
}

// pollAsync sends GET /v1/machines/self/control in a goroutine. It never
// calls t.Fatal, which is only allowed on the test's own goroutine.
func (e *env) pollAsync(token, query string) <-chan pollResult {
	ch := make(chan pollResult, 1)
	go func() {
		req, err := http.NewRequest("GET", e.srv.URL+"/v1/machines/self/control"+query, nil)
		if err != nil {
			ch <- pollResult{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			ch <- pollResult{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		ch <- pollResult{code: resp.StatusCode, body: b, err: err}
	}()
	return ch
}

func recv(t *testing.T, ch <-chan pollResult) pollResult {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("poll: %v", r.err)
		}
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not answer within 5 s")
	}
	return pollResult{}
}
```

- [ ] **Step 2: Extend the auth matrix**

In `internal/server/auth_test.go`, add `"context"` and `"slices"` to the imports, and replace `authCase` and `TestAuthMatrix` with:

```go
// authCase is one route with a request that succeeds for a machine token.
type authCase struct {
	path   string // concrete path to request
	body   any
	ok     int // status for an authorized request
	alsoOK int // another status an authorized request may get, or 0
	okB    int // status for the other machine's token (bluebox), if not ok
}

// TestAuthMatrix covers every route × every credential, and proves a
// rejected request changed nothing.
func TestAuthMatrix(t *testing.T) {
	e := newEnv(t)
	e.server.after = instantTimer // a poll with nothing pending answers 204 at once
	e.register(e.tokA, api.SessionUpsert{ID: sid1, HerdrSession: "default", HerdrPane: "p1", CWD: "/home/user/proj"})
	// sid3 carries a request tower claimed, for the result route. Its herdr
	// session differs, so the herdr-sessions case doesn't end it, and its ID
	// prefix differs from sid1's, so the sid1[:8] lookup stays unique.
	e.register(e.tokA, api.SessionUpsert{ID: sid3, HerdrSession: "other", HerdrPane: "p2"})
	e.recordPoll(e.tokA)
	ctx := context.Background()
	if _, _, err := e.st.CreateControl(ctx, sid3, api.RequestedByDashboard); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := e.st.ClaimControl(ctx, e.machine(e.tokA).ID)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	const link = "https://claude.ai/code/session_01AbCdEfGhIjKlMnOpQrStUv"

	cases := map[string]authCase{
		"GET /healthz": {path: "/healthz", ok: 200},
		"GET /{$}":     {path: "/", ok: 200}, // the dashboard page
		"POST /v1/sessions": {path: "/v1/sessions",
			body: api.SessionUpsert{ID: sid1, Source: api.SourcePlugin, GitBranch: "main"}, ok: 200, okB: 409},
		"PUT /v1/machines/self/herdr-sessions": {path: "/v1/machines/self/herdr-sessions",
			body: api.HerdrSessionsPut{HerdrSession: "default", Sessions: []api.SessionUpsert{{ID: sid1, HerdrPane: "p1"}}}, ok: 200},
		"POST /v1/sessions/{id}/events": {path: "/v1/sessions/" + sid1 + "/events",
			body: api.EventIn{Kind: api.KindStateChanged, Source: api.SourcePlugin, Payload: json.RawMessage(`{"agent_state":"working"}`)}, ok: 200, okB: 409},
		"POST /v1/sessions/{id}/report": {path: "/v1/sessions/" + sid1 + "/report",
			body: api.ReportIn{Done: []string{"wrote tests"}}, ok: 200, okB: 409},
		"POST /v1/sessions/{id}/title": {path: "/v1/sessions/" + sid1 + "/title", body: api.TitleIn{Title: "auth work"}, ok: 200, okB: 409},
		// The first authorized request creates (202); later ones return it (200).
		"POST /v1/sessions/{id}/remote-control": {path: "/v1/sessions/" + sid1 + "/remote-control", ok: 202, alsoOK: 200},
		// 204 with nothing pending; 200 once the case above created a request.
		"GET /v1/machines/self/control": {path: "/v1/machines/self/control?wait=1", ok: 204, alsoOK: 200},
		"POST /v1/control/{id}/result": {path: "/v1/control/" + claim.Request.ID + "/result",
			body: api.ControlResultIn{State: api.ControlDone, URL: link}, ok: 200, okB: 409},
		"GET /v1/sessions":      {path: "/v1/sessions", ok: 200},
		"GET /v1/sessions/{id}": {path: "/v1/sessions/" + sid1[:8], ok: 200},
		"GET /v1/machines":      {path: "/v1/machines", ok: 200},
	}

	// Mechanical coverage: every registered route has a case, and no case
	// is left over from a removed route.
	routes := e.server.routes()
	seen := map[string]bool{}
	for _, rt := range routes {
		key := rt.method + " " + rt.path
		seen[key] = true
		if _, ok := cases[key]; !ok {
			t.Errorf("route %s has no auth test case", key)
		}
	}
	for key := range cases {
		if !seen[key] {
			t.Errorf("auth case %s matches no route", key)
		}
	}

	tokens := []struct {
		name   string
		token  string
		cookie bool // send token as the hub_read cookie, not a bearer token
		action bool // send X-Hub-Action: remote-control
	}{
		{"none", "", false, false},
		{"bad", "hub_m_not-a-real-token", false, false},
		{"read", testReadToken, false, false},
		{"read+action", testReadToken, false, true},
		{"machine", e.tokA, false, false},
		{"machineB", e.tokB, false, false},
		{"cookie", testReadToken, true, false},
		{"cookie+action", testReadToken, true, true},
		{"badcookie", "hub_r_wrong", true, false},
	}
	expect := func(rt route, c authCase, tk string) []int {
		switch {
		case rt.access == accessPublic:
		case tk == "none" || tk == "bad" || tk == "badcookie":
			return []int{http.StatusUnauthorized}
		case rt.access == accessWrite && strings.HasPrefix(tk, "cookie"):
			return []int{http.StatusUnauthorized} // the cookie never authorizes a machine write
		case rt.access == accessWrite && strings.HasPrefix(tk, "read"):
			return []int{http.StatusForbidden}
		case rt.access == accessAction && (strings.HasPrefix(tk, "read") || tk == "cookie"):
			return []int{http.StatusForbidden} // the read token never; the cookie only with X-Hub-Action
		case tk == "machineB" && c.okB != 0:
			return []int{c.okB}
		}
		if c.alsoOK != 0 {
			return []int{c.ok, c.alsoOK}
		}
		return []int{c.ok}
	}
	failures := 0
	for _, rt := range routes {
		c := cases[rt.method+" "+rt.path]
		for _, tk := range tokens {
			want := expect(rt, c, tk.name)
			var hdr map[string]string
			if tk.action {
				hdr = map[string]string{api.HeaderAction: api.HeaderActionRemoteControl}
			}
			bearer, cookie := tk.token, ""
			if tk.cookie {
				bearer, cookie = "", tk.token
			}
			before := e.snapshot()
			code, body, h := e.doHdr(rt.method, c.path, bearer, cookie, hdr, c.body)
			if !slices.Contains(want, code) {
				failures++
				t.Errorf("%s %s with %s: status %d, want %v; body %s", rt.method, rt.path, tk.name, code, want, body)
				continue
			}
			if code >= 400 && rt.access == accessPage {
				if ct := h.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") || !strings.Contains(string(body), "?token=") {
					failures++
					t.Errorf("GET / with %s: 401 must be an HTML page naming ?token=; got %q %s", tk.name, ct, body)
				}
				if after := e.snapshot(); after != before {
					failures++
					t.Errorf("GET / with %s changed state", tk.name)
				}
			} else if code >= 400 {
				var ae api.Error
				if err := json.Unmarshal(body, &ae); err != nil || ae.Error == "" {
					failures++
					t.Errorf("%s %s with %s: error body %q is not api.Error", rt.method, rt.path, tk.name, body)
				}
				if ct := h.Get("Content-Type"); ct != "application/json" {
					t.Errorf("%s %s: error Content-Type %q", rt.method, rt.path, ct)
				}
				if code == http.StatusUnauthorized && rt.access != accessPage && !strings.HasPrefix(h.Get("WWW-Authenticate"), "Bearer") {
					t.Errorf("%s %s: 401 without WWW-Authenticate", rt.method, rt.path)
				}
				if after := e.snapshot(); after != before {
					failures++
					t.Errorf("%s %s with %s was rejected but changed state:\nbefore %s\nafter  %s",
						rt.method, rt.path, tk.name, before, after)
				}
			}
		}
	}
	t.Logf("auth matrix: %d routes x %d credentials, %d failures", len(routes), len(tokens), failures)

	// The authorized writes landed, and were attributed to tower.
	d := e.detail(sid1)
	if d.Machine != "tower" || d.Title != "auth work" || d.GitBranch != "main" || d.AgentState != "working" || len(d.Reports) != 1 {
		t.Errorf("authorized writes not visible: %+v", d.Session)
	}
	if d3 := e.detail(sid3); d3.RemoteControlURL != link {
		t.Errorf("authorized result not visible: %q", d3.RemoteControlURL)
	}
}
```

- [ ] **Step 3: Write the endpoint tests**

Create `internal/server/control_test.go`:

```go
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

const testLink = "https://claude.ai/code/session_01AbCdEfGhIjKlMnOpQrStUv"

func TestRemoteControlCreate(t *testing.T) {
	e := newEnv(t)
	e.register(e.tokA, api.SessionUpsert{ID: sid1, HerdrSession: "default", HerdrPane: "w1:p1"})
	e.register(e.tokA, api.SessionUpsert{ID: sid2}) // hooks only: no pane
	before := e.snapshot()
	for _, tc := range []struct {
		id   string
		want int
		msg  string
	}{
		{sid3, 404, "not found"},
		{sid2, 409, "not in herdr"},
		{sid1, 409, "offline"}, // tower's watcher never polled
		{"-x", 400, "invalid session id"},
	} {
		if code, _, b := e.tap(tc.id); code != tc.want || !strings.Contains(string(b), tc.msg) {
			t.Errorf("tap %s: %d %s, want %d with %q", tc.id, code, b, tc.want, tc.msg)
		}
	}
	if after := e.snapshot(); after != before {
		t.Fatalf("refused requests changed state:\nbefore %s\nafter  %s", before, after)
	}

	e.recordPoll(e.tokA)
	if d := e.detail(sid1); !d.Controllable || d.RemoteControl != nil {
		t.Fatalf("after a poll: controllable=%v request=%+v", d.Controllable, d.RemoteControl)
	}
	code, first, b := e.tap(sid1)
	if code != http.StatusAccepted || first.State != api.ControlPending || first.RequestedBy != api.RequestedByDashboard ||
		first.Machine != "tower" || !first.ExpiresAt.Equal(e.clock.Now().Add(2*time.Minute)) {
		t.Fatalf("first tap: %d %s", code, b)
	}
	// A second tap, and bluebox's CLI, get the open request.
	if code, again, _ := e.tap(sid1); code != http.StatusOK || again.ID != first.ID {
		t.Errorf("second tap: %d %+v", code, again)
	}
	var viaBlu api.ControlRequest
	e.must(http.StatusOK, "POST", "/v1/sessions/"+sid1+"/remote-control", e.tokB, nil, &viaBlu)
	if viaBlu.ID != first.ID {
		t.Errorf("bluebox's request: %+v", viaBlu)
	}
	d := e.detail(sid1)
	n := 0
	for _, ev := range d.Events {
		if ev.Kind == api.KindRemoteControlRequested {
			n++
		}
	}
	if d.RemoteControl == nil || d.RemoteControl.ID != first.ID || n != 1 {
		t.Errorf("session: request %+v, %d requested events", d.RemoteControl, n)
	}

	// No poll for over 2 minutes: not controllable, the request expired,
	// and a tap is refused.
	e.clock.Advance(2*time.Minute + time.Second)
	if d := e.detail(sid1); d.Controllable || d.RemoteControl.State != api.ControlExpired {
		t.Errorf("after 2m1s: controllable=%v state=%s", d.Controllable, d.RemoteControl.State)
	}
	if code, _, b := e.tap(sid1); code != http.StatusConflict || !strings.Contains(string(b), "offline") {
		t.Errorf("tap with a silent watcher: %d %s", code, b)
	}
	// A machine token's new request says which machine asked.
	e.recordPoll(e.tokA)
	var viaMachine api.ControlRequest
	e.must(http.StatusAccepted, "POST", "/v1/sessions/"+sid1+"/remote-control", e.tokB, nil, &viaMachine)
	if viaMachine.RequestedBy != "machine:bluebox" || viaMachine.ID == first.ID {
		t.Errorf("machine request: %+v", viaMachine)
	}
}

func TestRemoteControlLimit(t *testing.T) {
	e := newEnv(t)
	e.server.after = instantTimer
	e.recordPoll(e.tokA)
	var ids []string
	for i := range 11 {
		id := fmt.Sprintf("limit-%02d", i)
		e.register(e.tokA, api.SessionUpsert{ID: id, HerdrSession: "default", HerdrPane: fmt.Sprintf("w1:p%d", i+1)})
		ids = append(ids, id)
	}
	for _, id := range ids[:10] {
		if code, _, b := e.tap(id); code != http.StatusAccepted {
			t.Fatalf("tap %s: %d %s", id, code, b)
		}
	}
	before := e.snapshot()
	if code, _, b := e.tap(ids[10]); code != http.StatusTooManyRequests || !strings.Contains(string(b), "10 pending") {
		t.Errorf("11th tap: %d %s", code, b)
	}
	if after := e.snapshot(); after != before {
		t.Error("the refused tap changed state")
	}
	// A claim frees a slot.
	e.must(http.StatusOK, "GET", "/v1/machines/self/control?wait=1", e.tokA, nil, nil)
	if code, _, b := e.tap(ids[10]); code != http.StatusAccepted {
		t.Errorf("tap after a claim: %d %s", code, b)
	}
}

func TestControlPollWakesOnNewRequest(t *testing.T) {
	e := newEnv(t)
	ft := &fakeTimers{}
	e.server.after = ft.after
	e.controllable(sid1, "w1:p1")

	// One at a time, so timer 0 is tower's and timer 1 is bluebox's.
	tower := e.pollAsync(e.tokA, "?wait=30")
	ft.await(t, 1)
	bluebox := e.pollAsync(e.tokB, "?wait=30")
	ft.await(t, 2)
	code, req, b := e.tap(sid1)
	if code != http.StatusAccepted {
		t.Fatalf("tap: %d %s", code, b)
	}
	r := recv(t, tower)
	var claim api.ControlClaim
	if r.code != http.StatusOK || json.Unmarshal(r.body, &claim) != nil {
		t.Fatalf("tower's poll: %d %s", r.code, r.body)
	}
	if claim.Request.ID != req.ID || claim.Request.State != api.ControlClaimed || claim.Request.ClaimedAt == nil ||
		claim.Session.ID != sid1 || claim.Session.HerdrPane != "w1:p1" || claim.Session.CWD != "/home/user/proj" {
		t.Errorf("claim: %+v", claim)
	}
	if d := e.detail(sid1); d.RemoteControl.State != api.ControlClaimed {
		t.Errorf("session reads %s after the claim", d.RemoteControl.State)
	}
	// bluebox's poll was not woken by tower's request; its timer ends it.
	select {
	case r := <-bluebox:
		t.Fatalf("bluebox's poll answered early: %d %s", r.code, r.body)
	case <-time.After(50 * time.Millisecond):
	}
	for i := range 2 {
		if ft.wait(i) != 30*time.Second {
			t.Errorf("timer %d waits %s, want 30s", i, ft.wait(i))
		}
	}
	ft.fire(1)
	if r := recv(t, bluebox); r.code != http.StatusNoContent || len(r.body) != 0 {
		t.Errorf("bluebox's poll: %d %q", r.code, r.body)
	}
	// tower's watcher counts as polling now.
	if d := e.detail(sid1); !d.Controllable {
		t.Error("a poll did not refresh controllable")
	}
}

func TestControlPollWait(t *testing.T) {
	e := newEnv(t)
	ft := &fakeTimers{}
	e.server.after = ft.after
	for i, tc := range []struct {
		query string
		want  time.Duration
	}{
		{"", 30 * time.Second},
		{"?wait=7", 7 * time.Second},
		{"?wait=0", time.Second},
		{"?wait=-5", time.Second},
		{"?wait=31", 30 * time.Second},
		{"?wait=3600", 30 * time.Second},
	} {
		ch := e.pollAsync(e.tokA, tc.query)
		ft.await(t, i+1)
		if got := ft.wait(i); got != tc.want {
			t.Errorf("%q: waits %s, want %s", tc.query, got, tc.want)
		}
		ft.fire(i)
		if r := recv(t, ch); r.code != http.StatusNoContent {
			t.Errorf("%q: %d %s", tc.query, r.code, r.body)
		}
	}
	if code, b, _ := e.do("GET", "/v1/machines/self/control?wait=soon", e.tokA, nil); code != http.StatusBadRequest {
		t.Errorf("wait=soon: %d %s", code, b)
	}
}

func TestControlPollLimit(t *testing.T) {
	e := newEnv(t)
	ft := &fakeTimers{}
	e.server.after = ft.after
	a := e.pollAsync(e.tokA, "")
	b := e.pollAsync(e.tokA, "")
	ft.await(t, 2)
	if code, body, _ := e.do("GET", "/v1/machines/self/control", e.tokA, nil); code != http.StatusTooManyRequests ||
		!strings.Contains(string(body), "2 open polls") {
		t.Errorf("third poll: %d %s", code, body)
	}
	// bluebox is not limited by tower's polls.
	c := e.pollAsync(e.tokB, "")
	ft.await(t, 3)
	ft.fire(2)
	if r := recv(t, c); r.code != http.StatusNoContent {
		t.Errorf("bluebox: %d", r.code)
	}
	ft.fire(0)
	ft.fire(1)
	recv(t, a)
	recv(t, b)
	// Once they ended, tower may poll again.
	d := e.pollAsync(e.tokA, "")
	ft.await(t, 4)
	ft.fire(3)
	if r := recv(t, d); r.code != http.StatusNoContent {
		t.Errorf("poll after the others ended: %d", r.code)
	}
}

func TestControlPollEndsOnShutdown(t *testing.T) {
	e := newEnv(t)
	ft := &fakeTimers{}
	e.server.after = ft.after
	ch := e.pollAsync(e.tokA, "")
	ft.await(t, 1)
	e.server.StopPolls()
	if r := recv(t, ch); r.code != http.StatusNoContent {
		t.Errorf("open poll at shutdown: %d", r.code)
	}
	if code, _, _ := e.do("GET", "/v1/machines/self/control", e.tokA, nil); code != http.StatusNoContent {
		t.Errorf("poll after shutdown: %d", code)
	}
	e.server.StopPolls() // a second call is harmless
}

// sessionhub server sets WriteTimeout 30s; a full 30 s poll must still answer.
func TestControlPollOutlivesWriteTimeout(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewUnstartedServer(e.server.Handler())
	srv.Config.WriteTimeout = 500 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/machines/self/control?wait=1", nil)
	req.Header.Set("Authorization", "Bearer "+e.tokA)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("a poll longer than WriteTimeout failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || time.Since(start) < 900*time.Millisecond {
		t.Errorf("status %d after %s, want 204 after about 1 s", resp.StatusCode, time.Since(start))
	}
}

// claimOne taps sid and claims the request as tower's watcher.
func (e *env) claimOne(sid string) api.ControlClaim {
	e.t.Helper()
	if code, _, b := e.tap(sid); code != http.StatusAccepted && code != http.StatusOK {
		e.t.Fatalf("tap %s: %d %s", sid, code, b)
	}
	var claim api.ControlClaim
	e.must(http.StatusOK, "GET", "/v1/machines/self/control?wait=1", e.tokA, nil, &claim)
	return claim
}

func TestControlResult(t *testing.T) {
	e := newEnv(t)
	e.server.after = instantTimer
	e.controllable(sid1, "w1:p1")
	_, req, _ := e.tap(sid1)
	path := "/v1/control/" + req.ID + "/result"
	if code, b, _ := e.do("POST", path, e.tokA, api.ControlResultIn{State: "done"}); code != http.StatusConflict {
		t.Errorf("result before the claim: %d %s", code, b)
	}
	var claim api.ControlClaim
	e.must(http.StatusOK, "GET", "/v1/machines/self/control?wait=1", e.tokA, nil, &claim)

	before := e.snapshot()
	for _, tc := range []struct {
		name, path, token string
		body              any
		want              int
	}{
		{"another machine", path, e.tokB, api.ControlResultIn{State: "done"}, 409},
		{"unknown request", "/v1/control/cr_AAAAAAAAAAAAAAAAAAAAAA/result", e.tokA, api.ControlResultIn{State: "done"}, 404},
		{"malformed request ID", "/v1/control/nope/result", e.tokA, api.ControlResultIn{State: "done"}, 400},
		{"state pending", path, e.tokA, api.ControlResultIn{State: "pending"}, 400},
		{"http", path, e.tokA, api.ControlResultIn{State: "done", URL: "http://claude.ai/code/session_abc"}, 400},
		{"other host", path, e.tokA, api.ControlResultIn{State: "done", URL: "https://claude.ai.evil.example/code/session_abc"}, 400},
		{"extra path", path, e.tokA, api.ControlResultIn{State: "done", URL: "https://claude.ai/code/session_abc/x"}, 400},
		{"trailing newline", path, e.tokA, api.ControlResultIn{State: "done", URL: testLink + "\n"}, 400},
		{"URL on failed", path, e.tokA, api.ControlResultIn{State: "failed", URL: testLink}, 400},
		{"control character", path, e.tokA, api.ControlResultIn{State: "failed", Detail: "a\x1b[31mb"}, 400},
		{"detail too long", path, e.tokA, api.ControlResultIn{State: "failed", Detail: strings.Repeat("x", 201)}, 400},
		{"bad JSON", path, e.tokA, []byte("{"), 400},
	} {
		if code, b, _ := e.do("POST", tc.path, tc.token, tc.body); code != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, code, b, tc.want)
		}
	}
	if after := e.snapshot(); after != before {
		t.Fatalf("refused results changed state:\nbefore %s\nafter  %s", before, after)
	}

	var done api.ControlRequest
	e.must(http.StatusOK, "POST", path, e.tokA, api.ControlResultIn{State: "done", URL: testLink, Detail: "resumed in a new herdr workspace"}, &done)
	if done.State != api.ControlDone || done.URL != testLink || done.FinishedAt == nil {
		t.Errorf("result: %+v", done)
	}
	d := e.detail(sid1)
	if d.RemoteControlURL != testLink || d.RemoteControlAt == nil || !d.RemoteControlAt.Equal(e.clock.Now()) ||
		d.RemoteControl.State != api.ControlDone || d.RemoteControl.Detail != "resumed in a new herdr workspace" {
		t.Errorf("session after the result: %+v", d.Session)
	}
	found := false
	for _, ev := range d.Events {
		found = found || (ev.Kind == api.KindRemoteControlResult && strings.Contains(string(ev.Payload), req.ID))
	}
	if !found {
		t.Error("no remote_control_result event")
	}
	if code, _, _ := e.do("POST", path, e.tokA, api.ControlResultIn{State: "failed"}); code != http.StatusConflict {
		t.Errorf("second result: %d", code)
	}
}

// A result after its request expired is refused and changes nothing.
func TestControlResultAfterExpiry(t *testing.T) {
	e := newEnv(t)
	e.server.after = instantTimer
	e.controllable(sid1, "w1:p1")
	claim := e.claimOne(sid1)
	e.clock.Advance(2 * time.Minute)
	before := e.snapshot()
	code, b, _ := e.do("POST", "/v1/control/"+claim.Request.ID+"/result", e.tokA, api.ControlResultIn{State: "done", URL: testLink})
	if code != http.StatusConflict || !strings.Contains(string(b), "expired") {
		t.Errorf("late result: %d %s", code, b)
	}
	if after := e.snapshot(); after != before {
		t.Errorf("a late result changed state")
	}
	if d := e.detail(sid1); d.RemoteControlURL != "" || d.RemoteControl.State != api.ControlExpired {
		t.Errorf("session: link %q state %s", d.RemoteControlURL, d.RemoteControl.State)
	}
}
```

- [ ] **Step 4: Run the tests to see them fail**

Run: `GOFLAGS=-count=1 go test ./internal/server/`
Expected: FAIL to compile: `e.server.after undefined`, `undefined: accessAction`, `e.server.StopPolls undefined`.

- [ ] **Step 5: Add auth for the action route and the server plumbing**

In `internal/server/server.go`:

Change the `Server` struct and `New` (the file already imports `time` and `api`):

```go
// Server serves the sessionhub HTTP API.
type Server struct {
	store     *store.Store
	readToken string
	log       *log.Logger
	control   *controlHub
	// after is the long poll's timer. Tests replace it.
	after func(time.Duration) <-chan time.Time
}

// New returns a Server. readToken is the dashboard's read-only token.
func New(st *store.Store, readToken string, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Server{store: st, readToken: readToken, log: logger, control: newControlHub(), after: time.After}
}
```

Extend `principal`:

```go
// principal is who made a request.
type principal struct {
	machine store.Machine // set for a machine token
	read    bool          // set for the read token
	cookie  bool          // the read token came in the hub_read cookie
}
```

In `authenticate`, change the cookie branch's return to `return principal{read: true, cookie: true}, true, nil`.

Add `Unwrap` to `statusWriter`, so `http.NewResponseController` reaches the connection:

```go
// Unwrap lets http.ResponseController reach the underlying writer; the
// long poll uses it to extend its write deadline.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
```

Add `actor` after `writer`:

```go
// actor accepts a machine token, or the read cookie with the header
// X-Hub-Action: remote-control: the dashboard's one write. The custom header
// makes a cross-site request need a CORS preflight, which the server never
// grants, and the cookie is SameSite=Strict. The bearer read token and the
// cookie without the header get 403.
func (s *Server) actor(h authedHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok, err := s.authenticate(r, true)
		switch {
		case err != nil:
			s.internalError(w, err)
			return
		case !ok:
			w.Header().Set("WWW-Authenticate", `Bearer realm="sessionhub"`)
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		case p.read && !p.cookie:
			writeError(w, http.StatusForbidden, "the read token cannot request actions; use the dashboard or a machine token")
			return
		case p.read && r.Header.Get(api.HeaderAction) != api.HeaderActionRemoteControl:
			writeError(w, http.StatusForbidden, "the dashboard must send "+api.HeaderAction+": "+api.HeaderActionRemoteControl+" with this request")
			return
		}
		h(w, r, p)
	})
}
```

In `storeError`, add before `default:`:

```go
	case errors.Is(err, store.ErrNotControllable), errors.Is(err, store.ErrRequestClosed):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrTooMany):
		writeError(w, http.StatusTooManyRequests, err.Error())
```

In `internal/server/routes.go`, replace the access constants (gofmt realigns the comments):

```go
// Access levels a route requires.
const (
	accessPublic = "public"
	accessRead   = "read"   // machine token, read token, or read cookie
	accessPage   = "page"   // the dashboard: read credentials, else an HTML 401
	accessWrite  = "write"  // machine token only
	accessAction = "action" // machine token, or the read cookie with X-Hub-Action
)
```

and add these routes after the `title` route:

```go
		{"POST", "/v1/sessions/{id}/remote-control", accessAction, s.actor(s.postRemoteControl)},
		{"GET", "/v1/machines/self/control", accessWrite, s.writer(s.pollControl)},
		{"POST", "/v1/control/{id}/result", accessWrite, s.writer(s.postControlResult)},
```

- [ ] **Step 6: Write the handlers and the poll sessionhub**

Create `internal/server/control.go`:

```go
package server

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

const (
	// maxPollsPerMachine caps a machine's open long polls. One watcher runs
	// per machine; the second slot covers a restart's overlap.
	maxPollsPerMachine = 2
	// maxPollWait is the longest, and the default, poll wait.
	maxPollWait = 30 * time.Second
	// pollWriteSlack is added to a poll's write deadline on top of its wait.
	pollWriteSlack = 10 * time.Second
)

// controlHub tracks open long polls and wakes them when a request is
// created for their machine. It is in-process: a restart drops open polls,
// and the watchers reconnect. Pending requests are in the database.
type controlHub struct {
	mu   sync.Mutex
	open map[string]int           // machine name → open polls
	wake map[string]chan struct{} // machine name → closed on its next new request
	done chan struct{}            // closed by StopPolls
	stop sync.Once
}

func newControlHub() *controlHub {
	return &controlHub{open: map[string]int{}, wake: map[string]chan struct{}{}, done: make(chan struct{})}
}

func (h *controlHub) acquire(machine string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open[machine] >= maxPollsPerMachine {
		return false
	}
	h.open[machine]++
	return true
}

func (h *controlHub) release(machine string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open[machine]--; h.open[machine] <= 0 {
		delete(h.open, machine)
	}
}

// waiter returns a channel that closes on the machine's next new request.
func (h *controlHub) waiter(machine string) <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.wake[machine]
	if !ok {
		c = make(chan struct{})
		h.wake[machine] = c
	}
	return c
}

func (h *controlHub) notify(machine string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.wake[machine]; ok {
		close(c)
		delete(h.wake, machine)
	}
}

// StopPolls ends every open long poll with 204, and makes later polls answer
// 204 at once. `sessionhub server` registers it with http.Server.RegisterOnShutdown,
// so a restart doesn't wait out open polls.
func (s *Server) StopPolls() { s.control.stop.Do(func() { close(s.control.done) }) }

func (s *Server) postRemoteControl(w http.ResponseWriter, r *http.Request, p principal) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	by := api.RequestedByDashboard
	if !p.read {
		by = api.RequestedByMachinePrefix + p.machine.Name
	}
	req, created, err := s.store.CreateControl(r.Context(), id, by)
	if err != nil {
		s.storeError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
		s.control.notify(req.Machine)
	}
	writeJSON(w, status, req)
}

func (s *Server) pollControl(w http.ResponseWriter, r *http.Request, p principal) {
	wait := maxPollWait
	if v := r.URL.Query().Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("wait=%q: want a number of seconds", v))
			return
		}
		wait = time.Duration(min(max(n, 1), 30)) * time.Second
	}
	name := p.machine.Name
	if !s.control.acquire(name) {
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("machine %q already has %d open polls", name, maxPollsPerMachine))
		return
	}
	defer s.control.release(name)
	// sessionhub server's WriteTimeout (30 s) is shorter than a full poll plus its
	// answer, so extend this response's deadline.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(wait + pollWriteSlack))
	if err := s.store.RecordPoll(r.Context(), p.machine.ID); err != nil {
		s.internalError(w, err)
		return
	}
	timeout := s.after(wait)
	for {
		// Take the wake-up channel before the claim, so a request created
		// between the claim and the select still wakes this poll.
		woken := s.control.waiter(name)
		claim, ok, err := s.store.ClaimControl(r.Context(), p.machine.ID)
		if err != nil {
			s.internalError(w, err)
			return
		}
		if ok {
			writeJSON(w, http.StatusOK, claim)
			return
		}
		select {
		case <-woken:
		case <-timeout:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-s.control.done:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) postControlResult(w http.ResponseWriter, r *http.Request, p principal) {
	id := r.PathValue("id")
	if !store.ValidControlID(id) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request id %q", id))
		return
	}
	var in api.ControlResultIn
	if !decode(w, r, &in) {
		return
	}
	req, err := s.store.FinishControl(r.Context(), p.machine.ID, id, in)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}
```

- [ ] **Step 7: End open polls on shutdown**

In `internal/server/run.go`, `runWithListeners`, replace the `srv := &http.Server{ Handler: New(st, cfg.ReadToken, logger).Handler(), ...}` block with:

```go
	sessionhub := New(st, cfg.ReadToken, logger)
	srv := &http.Server{
		Handler:           sessionhub.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second, // the long poll extends its own deadline
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          logger,
	}
	// Shutdown waits for active requests; an open long poll would hold it
	// for up to 30 s. StopPolls answers them with 204 first.
	srv.RegisterOnShutdown(sessionhub.StopPolls)
```

- [ ] **Step 8: Run the server tests**

Run: `GOFLAGS=-count=1 go test ./internal/server/ -run 'TestAuthMatrix|TestRemoteControl|TestControl' -v 2>&1 | grep -E '^(=== RUN|--- |ok|FAIL|.*auth matrix)'`
Expected: every test `--- PASS`, and the log line `auth matrix: 13 routes x 9 credentials, 0 failures`.

Then run: `make test lint`
Expected: every package `ok`; `lint` prints nothing.

- [ ] **Step 9: Commit**

```bash
git add internal/server
git commit -F - <<'MSG'
Add the Remote Control request endpoints

POST /v1/sessions/{id}/remote-control takes the read cookie with
X-Hub-Action: remote-control, or a machine token; the bearer read token and
the bare cookie get 403. GET /v1/machines/self/control is a long poll (wait
clamped to 1-30 s, 2 open polls per machine) that claims the oldest pending
request and is woken in-process when one is created. POST
/v1/control/{id}/result takes the claiming machine's result.

Gotchas: sessionhub server's WriteTimeout is 30 s, so the poll extends its own
write deadline through http.ResponseController, which needs
statusWriter.Unwrap. Shutdown would wait on open polls, so StopPolls runs
from RegisterOnShutdown. The poll takes its wake-up channel before the
claim, so no request is missed in between.

The auth matrix now sends the cookie with and without X-Hub-Action, the
bearer read token with the header, and bluebox's token, to every route.

Docs: Task 7 updates docs/server.md (Authentication, Endpoints, Remote
Control requests).

Refs: docs/dev/superpowers/plans/2026-09-30-remote-control-button.md Task 2

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
MSG
```

---
### Task 3: Shared remote-control logic in `internal/resume`

**Files:**
- Modify: `internal/herdr/herdr.go` (add `WorkspaceCreateParams`, `WorkspaceCreated`, `WorkspaceCreate` after `AgentStart`)
- Create: `testdata/herdr/socket/workspace-create.ndjson` (captured live, Step 1)
- Modify: `testdata/README.md` (one fixture row)
- Modify: `internal/herdr/herdr_test.go` (`TestWorkspaceCreateCaptured`)
- Create: `internal/resume/control.go`
- Modify: `internal/resume/remotecontrol.go` (`remoteControlLocal` calls `Control`; the poll loop moves out)
- Modify: `internal/resume/resume.go` (`agentName`, `remoteControlArgs`; `claudeArgs` and `startIn` use them)
- Create: `internal/resume/control_test.go`

**Interfaces:**
- Consumes: `api.Session` (Task 1 fields are not used here), `api.StatusLive`, `api.StatusBlocked`; from the merged CLI task: `herdr.Client.PaneGet`, `Snapshot`, `PaneRead`, `AgentPrompt`, `SendKeys(paneID string, keys ...string) error`, `AgentStart`, `herdr.BlockedError`, `herdr.SourceRecentUnwrapped`, `herdr.SourceVisible`, and in `internal/resume`: `validID`, `validPaneID`, `isLive`, `clean`, `shortID`, `visibleDialog`, `newRemoteControlURL`, `rcLines`, `rcCommand`, `rcPollEvery`, `rcPollFor`.
- Produces:
  - `herdr.WorkspaceCreateParams{CWD, Label string; Focus bool}` (`cwd,omitempty`, `label,omitempty`, `focus`), `herdr.WorkspaceCreated{WorkspaceID string; RootPane herdr.PaneInfo}`, `func (c *herdr.Client) WorkspaceCreate(p WorkspaceCreateParams) (WorkspaceCreated, error)`.
  - `type resume.Outcome string` with `OutcomeLinked = "linked"`, `OutcomeAlreadyOn = "already_on"`, `OutcomeNoLink = "no_link"`, `OutcomeResumed = "resumed"`, `OutcomeBlocked = "blocked"`, `OutcomeDialogOpen = "dialog_open"`, `OutcomeNotRunning = "not_running"`, `OutcomeLooksLive = "looks_live"`, `OutcomeNoDir = "no_dir"`, `OutcomeError = "error"`.
  - `resume.ControlOptions{Socket string; Resume bool; PollEvery, PollFor time.Duration}`.
  - `resume.ControlResult{Outcome Outcome; URL, Pane, Workspace, DialogLeftOpen string; Err error}`. `DialogLeftOpen` is "" when the already-on dialog was closed, else the line the CLI prints about it.
  - `func resume.Control(ctx context.Context, s api.Session, o ControlOptions) ControlResult`.

- [ ] **Step 1: Capture the `workspace.create` exchange**

No fixture exists for `workspace.create`. The schema (`herdr api schema`) gives params `cwd`, `env`, `focus` (default `false`), `label`, `source_workspace_id`, and a result `{"type":"workspace_created","workspace":{...},"tab":{...},"root_pane":{...}}`. Capture one real exchange on `bluebox` in a scratch workspace that you close right away:

```bash
mkdir -p /tmp/sessionhub-rc-fixture
python3 - <<'EOF'
import json, os, socket
path = os.environ.get("HERDR_SOCKET_PATH") or os.path.expanduser("~/.config/herdr/herdr.sock")
req = {"id": "cap_1", "method": "workspace.create",
       "params": {"cwd": "/tmp/sessionhub-rc-fixture", "label": "sessionhub: fixture capture", "focus": False}}
s = socket.socket(socket.AF_UNIX)
s.connect(path)
s.sendall((json.dumps(req) + "\n").encode())
buf = b""
while not buf.endswith(b"\n"):
    buf += s.recv(65536)
resp = json.loads(buf)
open("testdata/herdr/socket/workspace-create.ndjson", "w").write(json.dumps(req) + "\n" + json.dumps(resp, separators=(",", ":")) + "\n")
print(resp["result"]["workspace"]["workspace_id"], resp["result"]["root_pane"]["pane_id"])
EOF
herdr workspace close <the workspace ID the script printed>
```

Expected: the script prints a workspace ID and its root pane (for example `wX wX:p1`), and `herdr workspace close` succeeds. The focused workspace doesn't change (`focus: false`). The fixture holds no session data, so it needs no scrubbing; check with `grep -c claude.ai testdata/herdr/socket/workspace-create.ndjson` (prints `0`).

Add a row to the socket table in `testdata/README.md`:

```markdown
| `workspace-create` | `workspace.create` `{"cwd":"/tmp/sessionhub-rc-fixture","label":"sessionhub: fixture capture","focus":false}` on `bluebox` (herdr 0.9.3); the scratch workspace was closed right after. The result has `workspace`, `tab`, and `root_pane` |
```

- [ ] **Step 2: Write the failing herdr test**

Add to `internal/herdr/herdr_test.go`:

```go
func TestWorkspaceCreateCaptured(t *testing.T) {
	s := herdrtest.New(t, "workspace-create.ndjson")
	ws, err := dial(t, s).WorkspaceCreate(herdr.WorkspaceCreateParams{CWD: "/tmp/sessionhub-rc-fixture", Label: "sessionhub: fixture capture"})
	if err != nil {
		t.Fatal(err)
	}
	if ws.WorkspaceID == "" || !strings.HasPrefix(ws.RootPane.PaneID, ws.WorkspaceID+":") {
		t.Errorf("workspace %+v", ws)
	}
	reqs := s.Requests()
	var p map[string]any
	if len(reqs) != 1 || reqs[0].Method != "workspace.create" || json.Unmarshal(reqs[0].Params, &p) != nil ||
		p["cwd"] != "/tmp/sessionhub-rc-fixture" || p["label"] != "sessionhub: fixture capture" || p["focus"] != false || len(p) != 3 {
		t.Errorf("requests: %+v", reqs)
	}
	// Any other params have no fixture: an error, never a workspace.
	if _, err := dial(t, s).WorkspaceCreate(herdr.WorkspaceCreateParams{CWD: "/elsewhere"}); err == nil {
		t.Error("unknown params: want an error")
	}
}
```

Run: `GOFLAGS=-count=1 go test ./internal/herdr/ -run TestWorkspaceCreateCaptured`
Expected: FAIL to compile: `undefined: herdr.WorkspaceCreateParams`.

- [ ] **Step 3: Add `WorkspaceCreate`**

In `internal/herdr/herdr.go`, after `AgentStart`:

```go
// WorkspaceCreateParams is the part of herdr's WorkspaceCreateParams sessionhub
// sends. Focus false leaves the user's focus where it is.
type WorkspaceCreateParams struct {
	CWD   string `json:"cwd,omitempty"`
	Label string `json:"label,omitempty"`
	Focus bool   `json:"focus"`
}

// WorkspaceCreated is the part of a workspace.create result sessionhub reads.
type WorkspaceCreated struct {
	WorkspaceID string
	RootPane    PaneInfo
}

// WorkspaceCreate calls workspace.create (captured in
// testdata/herdr/socket/workspace-create.ndjson). It errors when the result
// has no workspace or root pane, so no caller starts an agent in pane "".
func (c *Client) WorkspaceCreate(p WorkspaceCreateParams) (WorkspaceCreated, error) {
	res, err := c.Call("workspace.create", p)
	if err != nil {
		return WorkspaceCreated{}, err
	}
	var out struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspace"`
		RootPane PaneInfo `json:"root_pane"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return WorkspaceCreated{}, err
	}
	if out.Workspace.WorkspaceID == "" || out.RootPane.PaneID == "" {
		return WorkspaceCreated{}, errors.New("herdr: workspace.create response has no workspace or root pane")
	}
	return WorkspaceCreated{WorkspaceID: out.Workspace.WorkspaceID, RootPane: out.RootPane}, nil
}
```

Run: `GOFLAGS=-count=1 go test ./internal/herdr/`
Expected: `ok  	session-hub/internal/herdr`.

- [ ] **Step 4: Write the failing `Control` tests**

Create `internal/resume/control_test.go`. It reuses `newFakeHerdr`, `paneResult`, `fixtureText`, `sess`, `uuid`, and `rcURL` from the package's other tests:

```go
package resume

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// ctlScript scripts herdr for Control.
type ctlScript struct {
	mu        sync.Mutex
	pane      string           // the pane pane.get knows ("" for none)
	agent     string           // its detected agent ("" at a shell prompt)
	session   string           // its agent_session
	snapPanes []map[string]any // panes in session.snapshot
	snapErr   string
	promptErr string
	wsErr     string
	startErr  string
	before    string // pane text until /remote-control is sent or Claude starts
	after     string // pane text from then on
	acted     bool
}

func newCtlScript(t *testing.T) *ctlScript {
	return &ctlScript{
		pane: "w1:p1", agent: "claude", session: uuid,
		before: fixtureText(t, "pane-read-before-remote-control.ndjson"),
		after:  fixtureText(t, "pane-read-after-remote-control.ndjson"),
	}
}

// paneOf is one snapshot pane.
func paneOf(id, sessionUUID, agent string) map[string]any {
	return paneResult(id, sessionUUID, agent)["pane"].(map[string]any)
}

func (c *ctlScript) herdr(t *testing.T) *fakeHerdr {
	return newFakeHerdr(t, func(m string, p map[string]any) (any, string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		switch m {
		case "pane.get":
			if c.pane != "" && p["pane_id"] == c.pane {
				return paneResult(c.pane, c.session, c.agent), ""
			}
			return nil, "pane_not_found"
		case "session.snapshot":
			if c.snapErr != "" {
				return nil, c.snapErr
			}
			panes := c.snapPanes
			if panes == nil {
				panes = []map[string]any{}
			}
			return map[string]any{"type": "session_snapshot", "snapshot": map[string]any{"panes": panes}}, ""
		case "pane.read":
			text := c.before
			if c.acted {
				text = c.after
			}
			return map[string]any{"type": "pane_read", "read": map[string]any{"pane_id": p["pane_id"], "text": text}}, ""
		case "agent.prompt":
			if c.promptErr != "" {
				return nil, c.promptErr
			}
			c.acted = true
			return map[string]any{"type": "agent_prompted"}, ""
		case "pane.send_keys":
			return map[string]any{"type": "ok"}, ""
		case "workspace.create":
			if c.wsErr != "" {
				return nil, c.wsErr
			}
			return map[string]any{"type": "workspace_created",
				"workspace": map[string]any{"workspace_id": "w9"},
				"root_pane": map[string]any{"pane_id": "w9:p1", "workspace_id": "w9"}}, ""
		case "agent.start":
			if c.startErr != "" {
				return nil, c.startErr
			}
			c.acted = true
			return map[string]any{"type": "agent_started"}, ""
		}
		return nil, "no_fixture"
	})
}

func fastControl(sock string) ControlOptions {
	return ControlOptions{Socket: sock, Resume: true, PollEvery: 5 * time.Millisecond, PollFor: 150 * time.Millisecond}
}

func TestControlDecisionTable(t *testing.T) {
	dir := t.TempDir()
	gone := filepath.Join(dir, "gone")
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	deadSock := filepath.Join(t.TempDir(), "none.sock")
	at := func(status, cwd string) api.Session {
		s := sess("tower", "w1:p1")
		s.Status, s.CWD = status, cwd
		return s
	}
	cases := []struct {
		name     string
		sess     api.Session
		script   func(c *ctlScript)
		noHerdr  bool
		want     Outcome
		wantURL  string
		wantPane string
		errHas   string
		calls    []string // herdr methods that must run
		never    []string // herdr methods that must not run
	}{
		{name: "Claude runs it in the recorded pane: inject",
			sess: at(api.StatusLive, dir), script: func(c *ctlScript) {},
			want: OutcomeLinked, wantURL: rcURL, wantPane: "w1:p1",
			calls: []string{"pane.get", "agent.prompt"}, never: []string{"session.snapshot", "workspace.create", "agent.start"}},
		{name: "the pane moved since the heartbeat: found in a fresh snapshot, inject there",
			sess: at(api.StatusLive, dir),
			script: func(c *ctlScript) {
				c.pane = ""
				c.snapPanes = []map[string]any{
					paneOf("w2:p4", "99999999-0000-4000-8000-000000000000", "claude"),
					paneOf("w2:p3", uuid, "claude"),
				}
			},
			want: OutcomeLinked, wantURL: rcURL, wantPane: "w2:p3",
			calls: []string{"pane.get", "session.snapshot", "agent.prompt"}, never: []string{"workspace.create", "agent.start"}},
		{name: "blocked on a prompt: nothing typed, nothing started",
			sess: at(api.StatusBlocked, dir), script: func(c *ctlScript) { c.promptErr = "agent_blocked" },
			want: OutcomeBlocked, calls: []string{"agent.prompt"}, never: []string{"workspace.create", "agent.start", "pane.send_keys"}},
		{name: "a Remote Control dialog is already open: nothing sent",
			sess:   at(api.StatusLive, dir),
			script: func(c *ctlScript) { c.before = fixtureText(t, "pane-read-remote-control-dialog.ndjson") },
			want:   OutcomeDialogOpen, never: []string{"agent.prompt", "pane.send_keys", "workspace.create"}},
		{name: "Remote Control was already on: the link from the dialog, dialog closed",
			sess:   at(api.StatusLive, dir),
			script: func(c *ctlScript) { c.after = fixtureText(t, "pane-read-remote-control-dialog.ndjson") },
			want:   OutcomeAlreadyOn, wantURL: rcURL, calls: []string{"agent.prompt", "pane.send_keys"}, never: []string{"workspace.create"}},
		{name: "ended and the pane is gone: resume in a new workspace",
			sess: at(api.StatusEnded, dir), script: func(c *ctlScript) { c.pane = "" },
			want: OutcomeResumed, wantURL: rcURL, wantPane: "w9:p1",
			calls: []string{"pane.get", "session.snapshot", "workspace.create", "agent.start", "pane.read"}, never: []string{"agent.prompt"}},
		{name: "resumed, but no link within the wait",
			sess: at(api.StatusEnded, dir), script: func(c *ctlScript) { c.pane = ""; c.after = "starting" },
			want: OutcomeResumed, wantPane: "w9:p1", calls: []string{"agent.start"}},
		{name: "stale and no pane runs it: resume",
			sess: at(api.StatusStale, dir), script: func(c *ctlScript) { c.pane = "" },
			want: OutcomeResumed, wantURL: rcURL, calls: []string{"workspace.create"}},
		{name: "the pane kept the session at a shell prompt: resume in a new workspace, not there",
			sess: at(api.StatusEnded, dir), script: func(c *ctlScript) { c.agent = "" },
			want: OutcomeResumed, wantPane: "w9:p1", wantURL: rcURL,
			calls: []string{"workspace.create"}, never: []string{"agent.prompt"}},
		{name: "live but no pane runs it: looks live, start nothing",
			sess: at(api.StatusLive, dir), script: func(c *ctlScript) { c.pane = "" },
			want: OutcomeLooksLive, calls: []string{"session.snapshot"}, never: []string{"workspace.create", "agent.start", "agent.prompt"}},
		{name: "blocked but no pane runs it: looks live, start nothing",
			sess: at(api.StatusBlocked, dir), script: func(c *ctlScript) { c.pane = "" },
			want: OutcomeLooksLive, never: []string{"workspace.create", "agent.start"}},
		{name: "the directory is gone: create nothing",
			sess: at(api.StatusEnded, gone), script: func(c *ctlScript) { c.pane = "" },
			want: OutcomeNoDir, never: []string{"workspace.create", "agent.start"}},
		{name: "the directory is a file: create nothing",
			sess: at(api.StatusEnded, file), script: func(c *ctlScript) { c.pane = "" },
			want: OutcomeNoDir, never: []string{"workspace.create", "agent.start"}},
		{name: "no directory recorded: create nothing",
			sess: at(api.StatusEnded, ""), script: func(c *ctlScript) { c.pane = "" },
			want: OutcomeNoDir, never: []string{"workspace.create", "agent.start"}},
		{name: "workspace.create fails: no agent.start",
			sess: at(api.StatusEnded, dir), script: func(c *ctlScript) { c.pane = ""; c.wsErr = "internal_error" },
			want: OutcomeError, errHas: "create workspace", never: []string{"agent.start"}},
		{name: "agent.start fails: the error names the new workspace",
			sess: at(api.StatusEnded, dir), script: func(c *ctlScript) { c.pane = ""; c.startErr = "pane_not_ready" },
			want: OutcomeError, errHas: "w9", calls: []string{"workspace.create", "agent.start"}},
		{name: "the snapshot fails: start nothing",
			sess: at(api.StatusEnded, dir), script: func(c *ctlScript) { c.pane = ""; c.snapErr = "internal_error" },
			want: OutcomeError, errHas: "snapshot", never: []string{"workspace.create"}},
		{name: "recorded in another herdr server: skip pane.get, look in the snapshot",
			sess:   func() api.Session { s := at(api.StatusEnded, dir); s.HerdrSession = "other"; return s }(),
			script: func(c *ctlScript) {},
			want:   OutcomeResumed, wantURL: rcURL, calls: []string{"session.snapshot", "workspace.create"}, never: []string{"pane.get"}},
		{name: "herdr is not running",
			sess: at(api.StatusEnded, dir), noHerdr: true, want: OutcomeError, errHas: "herdr is not running"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sock := deadSock
			var fh *fakeHerdr
			if !tc.noHerdr {
				c := newCtlScript(t)
				tc.script(c)
				fh = c.herdr(t)
				sock = fh.path
			}
			r := Control(context.Background(), tc.sess, fastControl(sock))
			if r.Outcome != tc.want || r.URL != tc.wantURL {
				t.Fatalf("outcome %s url %q err %v; want %s %q", r.Outcome, r.URL, r.Err, tc.want, tc.wantURL)
			}
			if tc.wantPane != "" && r.Pane != tc.wantPane {
				t.Errorf("pane %q, want %q", r.Pane, tc.wantPane)
			}
			if tc.errHas == "" && r.Err != nil {
				t.Errorf("err %v", r.Err)
			}
			if tc.errHas != "" && (r.Err == nil || !strings.Contains(r.Err.Error(), tc.errHas)) {
				t.Errorf("err %v, want %q", r.Err, tc.errHas)
			}
			if fh != nil {
				got := "," + strings.Join(fh.methods(), ",") + ","
				for _, m := range tc.calls {
					if !strings.Contains(got, ","+m+",") {
						t.Errorf("%s never ran; methods %s", m, got)
					}
				}
				for _, m := range tc.never {
					if strings.Contains(got, ","+m+",") {
						t.Errorf("%s ran; methods %s", m, got)
					}
				}
			}
		})
	}
}

// The resume path: the exact workspace.create and agent.start params.
func TestControlResumeParams(t *testing.T) {
	dir := t.TempDir()
	c := newCtlScript(t)
	c.pane = ""
	fh := c.herdr(t)
	s := sess("tower", "w1:p1")
	s.CWD, s.Title = dir, "fix CI token rotation"
	r := Control(context.Background(), s, fastControl(fh.path))
	if r.Outcome != OutcomeResumed || r.Workspace != "w9" || r.Pane != "w9:p1" {
		t.Fatalf("result %+v", r)
	}
	if p := fh.params("workspace.create"); p["cwd"] != dir || p["label"] != "sessionhub: fix CI token rotation" || p["focus"] != false || len(p) != 3 {
		t.Errorf("workspace.create params = %v", p)
	}
	p := fh.params("agent.start")
	args, _ := json.Marshal(p["args"])
	if p["name"] != "sessionhub-0a1b2c3d" || p["kind"] != "claude" || p["pane_id"] != "w9:p1" ||
		string(args) != `["--resume","`+uuid+`","--remote-control"]` || len(p) != 4 {
		t.Errorf("agent.start params = %v", p)
	}
	if p := fh.params("pane.read"); p["pane_id"] != "w9:p1" {
		t.Errorf("pane.read params = %v", p)
	}
}

func TestWorkspaceLabel(t *testing.T) {
	for _, tc := range []struct{ title, want string }{
		{"fix CI token rotation", "sessionhub: fix CI token rotation"},
		{"", "sessionhub: 0a1b2c3d"},
		{" \t ", "sessionhub: 0a1b2c3d"},
		{"a\x1b[31mb\nc", "sessionhub: a[31mb c"},
		{strings.Repeat("x", 50), "sessionhub: " + strings.Repeat("x", 39) + "…"},
	} {
		s := sess("tower", "")
		s.Title = tc.title
		if got := workspaceLabel(s); got != tc.want {
			t.Errorf("title %q: label %q, want %q", tc.title, got, tc.want)
		}
	}
}
```

Run: `GOFLAGS=-count=1 go test ./internal/resume/ -run 'TestControl|TestWorkspaceLabel'`
Expected: FAIL to compile: `undefined: Control`, `undefined: ControlOptions`, `undefined: workspaceLabel`.

- [ ] **Step 5: Write `Control`**

Create `internal/resume/control.go`. `injectAndWait` and `closeDialog` hold the steps of `remoteControlLocal` and `(*env).closeDialog` from the merged CLI task. The bodies below are those steps at commit `fcc4638`. If the merged code differs, copy the merged steps into these two functions in the same order instead, changing only printed lines into returned values as shown here. Step 7 proves the move kept every herdr call and every printed line.

```go
package resume

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
	"github.com/abdallah/session-hub/internal/herdr"
)

// Outcome names what Control did.
type Outcome string

const (
	OutcomeLinked     Outcome = "linked"      // sent /remote-control; a new link appeared
	OutcomeAlreadyOn  Outcome = "already_on"  // Claude showed its already-on dialog; the link is from it
	OutcomeNoLink     Outcome = "no_link"     // sent /remote-control; no link within PollFor
	OutcomeResumed    Outcome = "resumed"     // started claude --resume <id> --remote-control in a new workspace
	OutcomeBlocked    Outcome = "blocked"     // herdr refused: Claude waits on a prompt; nothing was sent
	OutcomeDialogOpen Outcome = "dialog_open" // a Remote Control dialog was already open; nothing was sent
	OutcomeNotRunning Outcome = "not_running" // no pane runs the session (only without Resume)
	OutcomeLooksLive  Outcome = "looks_live"  // the server says live or blocked, but no pane runs it; nothing started
	OutcomeNoDir      Outcome = "no_dir"      // the session's directory is missing; nothing created
	OutcomeError      Outcome = "error"       // a herdr call failed; Err says which
)

// ControlOptions configures Control.
type ControlOptions struct {
	// Socket is the herdr socket path.
	Socket string
	// Resume starts a session that no pane runs in a new herdr workspace.
	// Without it (`sessionhub remote-control`), that session is OutcomeNotRunning.
	Resume bool
	// PollEvery and PollFor bound the wait for the link. Zero means 500 ms
	// and 10 s.
	PollEvery, PollFor time.Duration
}

// ControlResult is what Control did.
type ControlResult struct {
	Outcome Outcome
	// URL is the Remote Control link, or "" when none was seen.
	URL string
	// Pane got /remote-control, or is the new workspace's pane.
	Pane string
	// Workspace is the workspace Control created, if it created one.
	Workspace string
	// DialogLeftOpen is "" when Control closed the already-on dialog (or it
	// closed on its own), else why it stayed open, as the CLI prints it.
	DialogLeftOpen string
	// Err says what failed, for OutcomeError.
	Err error
}

// maxLabelTitle caps the title in a new workspace's label.
const maxLabelTitle = 40

// Control turns on Remote Control for session s in this machine's herdr.
//
// When herdr detects Claude running the session in a pane (the recorded one,
// or with o.Resume any pane in a fresh snapshot), Control sends
// /remote-control there and waits for the link. Otherwise, with o.Resume, it
// starts `claude --resume <id> --remote-control` in a new workspace in the
// session's directory, without focus. It starts nothing when the server
// reports the session live or blocked: it may run where herdr can't see, and
// a second copy would write to the same transcript.
func Control(ctx context.Context, s api.Session, o ControlOptions) ControlResult {
	if !validID(s.ID) {
		return failure(fmt.Errorf("refusing session ID %q: expected letters, digits, and hyphens, not starting with a hyphen", s.ID))
	}
	if s.HerdrPane != "" && !validPaneID.MatchString(s.HerdrPane) {
		return failure(fmt.Errorf("refusing pane ID %q: not a herdr pane ID", clean(s.HerdrPane)))
	}
	// Pane IDs belong to one herdr server: another server's pane is not ours.
	sameServer := s.HerdrSession == "" || s.HerdrSession == herdr.SessionName(o.Socket)
	if !o.Resume && (s.HerdrPane == "" || !sameServer) {
		return ControlResult{Outcome: OutcomeNotRunning}
	}
	h, err := herdr.Dial(o.Socket)
	if err != nil {
		if !o.Resume {
			return ControlResult{Outcome: OutcomeNotRunning}
		}
		return failure(fmt.Errorf("herdr is not running: %w", err))
	}
	pane, err := findPane(h, s, sameServer, o.Resume)
	switch {
	case err != nil:
		return failure(err)
	case pane != "":
		return injectAndWait(ctx, h, pane, s.ID, o)
	case !o.Resume:
		return ControlResult{Outcome: OutcomeNotRunning}
	case isLive(s):
		return ControlResult{Outcome: OutcomeLooksLive}
	}
	return resumeInWorkspace(ctx, h, s, o)
}

func failure(err error) ControlResult { return ControlResult{Outcome: OutcomeError, Err: err} }

// runs reports whether herdr detects Claude running session id in p.
func runs(p herdr.PaneInfo, id string) bool { return p.Agent == "claude" && p.SessionID() == id }

// findPane returns the pane where Claude runs session s: the recorded pane
// first, then with scan any pane in a fresh snapshot (the pane can move
// between heartbeats). "" means none. An error other than pane_not_found
// ends Control.
func findPane(h *herdr.Client, s api.Session, sameServer, scan bool) (string, error) {
	if s.HerdrPane != "" && sameServer {
		p, err := h.PaneGet(s.HerdrPane)
		var he *herdr.Error
		switch {
		case errors.As(err, &he) && he.Code == "pane_not_found":
		case err != nil:
			return "", fmt.Errorf("look up pane %s: %w", clean(s.HerdrPane), err)
		case runs(p, s.ID):
			return p.PaneID, nil
		}
	}
	if !scan {
		return "", nil
	}
	snap, err := h.Snapshot()
	if err != nil {
		return "", fmt.Errorf("read herdr snapshot: %w", err)
	}
	for _, p := range snap.Panes {
		if runs(p, s.ID) && validPaneID.MatchString(p.PaneID) {
			return p.PaneID, nil
		}
	}
	return "", nil
}

func pollBounds(o ControlOptions) (every, total time.Duration) {
	every, total = o.PollEvery, o.PollFor
	if every <= 0 {
		every = rcPollEvery
	}
	if total <= 0 {
		total = rcPollFor
	}
	return every, total
}

// injectAndWait sends /remote-control to a pane where Claude runs the
// session and waits for the link. These are the CLI task's steps from
// remoteControlLocal, moved here: they return an Outcome instead of printing.
func injectAndWait(ctx context.Context, h *herdr.Client, pane, sessionID string, o ControlOptions) ControlResult {
	r := ControlResult{Pane: pane}
	fail := func(err error) ControlResult {
		r.Outcome, r.Err = OutcomeError, err
		return r
	}
	// Read first, so a URL from an earlier run is not mistaken for the new one.
	before, err := h.PaneRead(pane, herdr.SourceRecentUnwrapped, rcLines)
	if err != nil {
		return fail(fmt.Errorf("read pane %s: %w", clean(pane), err))
	}
	screen, err := h.PaneRead(pane, herdr.SourceVisible, 0)
	if err != nil {
		return fail(fmt.Errorf("read pane %s: %w", clean(pane), err))
	}
	if _, open := visibleDialog(screen); open {
		r.Outcome = OutcomeDialogOpen
		return r
	}
	if err := h.AgentPrompt(pane, rcCommand); err != nil {
		var blocked *herdr.BlockedError
		if errors.As(err, &blocked) {
			r.Outcome = OutcomeBlocked
			return r
		}
		return fail(fmt.Errorf("send %s to pane %s: %w", rcCommand, clean(pane), err))
	}
	every, total := pollBounds(o)
	deadline := time.Now().Add(total)
	for {
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(every):
		}
		// A failed read is treated like no URL yet; the deadline ends the wait.
		after, err := h.PaneRead(pane, herdr.SourceRecentUnwrapped, rcLines)
		if err == nil {
			// Remote Control was already on when the visible screen shows
			// Claude's dialog instead of a new line.
			if vis, verr := h.PaneRead(pane, herdr.SourceVisible, 0); verr == nil {
				if u, open := visibleDialog(vis); open {
					r.Outcome, r.URL = OutcomeAlreadyOn, u
					r.DialogLeftOpen = closeDialog(h, pane, sessionID)
					return r
				}
			}
			if u := newRemoteControlURL(before, after); u != "" {
				r.Outcome, r.URL = OutcomeLinked, u
				return r
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
	}
	r.Outcome = OutcomeNoLink
	return r
}

// closeDialog presses Esc, which picks the dialog's default (Continue), but
// only when the dialog is still on the visible screen and Claude isn't
// working: Esc during a turn interrupts it. It re-checks just before sending,
// because the screen may have changed since the last read. It returns "" when
// the dialog is closed, else the line that tells the user to press Esc.
func closeDialog(h *herdr.Client, pane, sessionID string) string {
	p, err := h.PaneGet(pane)
	if err != nil || !runs(p, sessionID) {
		return "Could not check the pane before closing its dialog. Press Esc in the pane."
	}
	if p.AgentStatus == "working" {
		return "Claude is working, so sessionhub did not press Esc. Press Esc in the pane to close the dialog."
	}
	vis, err := h.PaneRead(pane, herdr.SourceVisible, 0)
	if err != nil {
		return fmt.Sprintf("Could not close its dialog (%v). Press Esc in the pane.", err)
	}
	if _, open := visibleDialog(vis); !open {
		return "" // it closed on its own
	}
	if err := h.SendKeys(pane, "esc"); err != nil {
		return fmt.Sprintf("Could not close its dialog (%v). Press Esc in the pane.", err)
	}
	return ""
}

// resumeInWorkspace starts `claude --resume <id> --remote-control` in a new
// herdr workspace in the session's directory, without focus, and waits for
// the link. A missing directory creates nothing.
func resumeInWorkspace(ctx context.Context, h *herdr.Client, s api.Session, o ControlOptions) ControlResult {
	if s.CWD == "" {
		return ControlResult{Outcome: OutcomeNoDir}
	}
	if st, err := os.Stat(s.CWD); err != nil || !st.IsDir() {
		return ControlResult{Outcome: OutcomeNoDir}
	}
	ws, err := h.WorkspaceCreate(herdr.WorkspaceCreateParams{CWD: s.CWD, Label: workspaceLabel(s)})
	if err != nil {
		return failure(fmt.Errorf("create workspace: %w", err))
	}
	r := ControlResult{Workspace: ws.WorkspaceID, Pane: ws.RootPane.PaneID}
	if _, err := h.AgentStart(herdr.AgentStartParams{
		Name:   agentName(s.ID),
		Kind:   "claude",
		PaneID: r.Pane,
		Args:   remoteControlArgs(s.ID),
	}); err != nil {
		r.Outcome, r.Err = OutcomeError, fmt.Errorf("start claude in pane %s of new workspace %s: %w", clean(r.Pane), clean(r.Workspace), err)
		return r
	}
	r.Outcome = OutcomeResumed
	r.URL = waitForLink(ctx, h, r.Pane, o)
	return r
}

// waitForLink reads a pane Claude just started in until a Remote Control
// link appears, for up to PollFor. Every link counts: the pane is new.
func waitForLink(ctx context.Context, h *herdr.Client, pane string, o ControlOptions) string {
	every, total := pollBounds(o)
	deadline := time.Now().Add(total)
	for {
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(every):
		}
		if text, err := h.PaneRead(pane, herdr.SourceRecentUnwrapped, rcLines); err == nil {
			if u := newRemoteControlURL("", text); u != "" {
				return u
			}
		}
		if !time.Now().Before(deadline) {
			return ""
		}
	}
}

// workspaceLabel is "sessionhub: <title>", with the title cleaned and cut to
// maxLabelTitle runes, or the short session ID when there is no title.
func workspaceLabel(s api.Session) string {
	t := termtext.Clean(s.Title, maxLabelTitle)
	if strings.TrimSpace(t) == "" {
		t = shortID(s.ID)
	}
	return "sessionhub: " + t
}
```

- [ ] **Step 6: Share the agent name and args, and point the CLI at `Control`**

In `internal/resume/resume.go`, add after `claudeArgs`, and make `claudeArgs` and `startIn` use them:

```go
// agentName is the herdr agent name sessionhub gives a Claude it starts.
func agentName(id string) string { return "sessionhub-" + strings.ToLower(shortID(id)) }

// remoteControlArgs starts Claude on session id with Remote Control on.
func remoteControlArgs(id string) []string { return []string{"--resume", id, "--remote-control"} }
```

```go
// claudeArgs is the argument list that starts Claude on the session.
func (e *env) claudeArgs(s api.Session) []string {
	if e.remoteControl {
		return remoteControlArgs(s.ID)
	}
	return []string{"--resume", s.ID}
}
```

In `startIn`, change `Name: "sessionhub-" + strings.ToLower(shortID(s.ID)),` to `Name: agentName(s.ID),`.

In `internal/resume/remotecontrol.go`, delete `(*env).closeDialog` (it moved to `control.go`), and replace the whole body of `remoteControlLocal` (the loop now lives in `injectAndWait`) with a call to `Control` that prints what the CLI printed before. Keep the merged branch's wording where it differs from the lines below:

```go
// remoteControlLocal turns on Remote Control for a session on this machine.
// It never resumes: a session no pane runs is "not running".
func (e *env) remoteControlLocal(ctx context.Context, s api.Session) error {
	o := ControlOptions{Socket: e.socket, PollEvery: e.pollEvery, PollFor: e.pollFor}
	r := Control(ctx, s, o)
	switch r.Outcome {
	case OutcomeLinked:
		fmt.Fprintf(e.out, "Remote Control is on for session %s:\n%s\n", shortID(s.ID), r.URL)
	case OutcomeAlreadyOn:
		fmt.Fprintf(e.out, "Remote Control is already on for session %s:\n%s\n", shortID(s.ID), r.URL)
		if r.DialogLeftOpen != "" {
			fmt.Fprintln(e.out, r.DialogLeftOpen)
		}
	case OutcomeNoLink:
		_, total := pollBounds(o)
		fmt.Fprintf(e.out, "Sent %s to pane %s, but no Remote Control URL appeared within %s. Check the pane or the Claude app.\n",
			rcCommand, clean(r.Pane), total)
	case OutcomeBlocked:
		return errors.New("the session is waiting on a prompt in its pane; answer it, then run this again")
	case OutcomeDialogOpen:
		return errors.New("a Remote Control dialog is open in the pane; press Esc there, then run this again")
	case OutcomeNotRunning:
		return notRunning(s)
	default:
		return r.Err
	}
	return nil
}
```

Remove the `session-hub/internal/herdr` import from `remotecontrol.go`: nothing there calls herdr any more (`time` stays for the constants). `go build ./...` names any other leftover.

- [ ] **Step 7: Run the resume and herdr tests**

Run: `GOFLAGS=-count=1 go test ./internal/resume/ ./internal/herdr/`
Expected: both `ok`. The CLI task's tables (`TestRemoteControlDecisionTable`, `TestRemoteControlAlreadyOnDialog`, `TestRemoteControlRequestParams`, `TestRemoteControlHostileOutputIsCleaned`, `TestRemoteControlRefusals`, `TestResumeRemoteControlFlag`) pass **without edits**: that is the proof the move kept every herdr call and every printed line. If one fails, fix `injectAndWait` or the mapping, never the old test.

Then run: `make test lint`
Expected: every package `ok`; `lint` prints nothing.

- [ ] **Step 8: Commit**

```bash
git add internal/herdr internal/resume testdata
git commit -F - <<'MSG'
Move remote-control logic into resume.Control and add the resume path

resume.Control is what the CLI and the plugin watcher both call. It finds
the pane where herdr detects Claude running the session (the recorded
pane, then with Resume a fresh snapshot, because the pane can move between
heartbeats), and sends /remote-control there. With Resume, a session no
pane runs starts in a new herdr workspace in its directory, without focus,
labeled "sessionhub: <title>", through agent.start with --resume <id>
--remote-control. It starts nothing for a session the server reports live
or blocked ("looks live"), or whose directory is gone.

sessionhub remote-control on this machine now calls Control without Resume; its
output and herdr calls are unchanged, which the CLI task's tables check.
herdr gains WorkspaceCreate, with a workspace.create exchange captured on
bluebox in a scratch workspace that was closed right after.

Docs: Task 7 updates docs/cli.md and docs/client.md (internal/herdr).

Refs: docs/dev/superpowers/plans/2026-09-30-remote-control-button.md Task 3

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
MSG
```

---
### Task 4: The watcher's control loop

**Files:**
- Modify: `internal/client/http.go` (`New`, `do` → `doTimeout`; add `PollControl`, `PostControlResult`)
- Modify: `internal/client/http_test.go` (`TestPollControl`, `TestPostControlResult`)
- Modify: `internal/herdr/herdrtest/herdrtest.go` (`Handler`, `Handle`)
- Create: `internal/plugin/control.go`
- Modify: `internal/plugin/watcher.go` (`runWatcher` starts and stops the loop)
- Create: `internal/plugin/control_test.go`

**Interfaces:**
- Consumes: Task 1's `api.ControlClaim`, `api.ControlResultIn`, `api.ControlRequest`, `api.ControlDone`, `api.ControlFailed`, `api.ActionRemoteControl`, `api.MaxControlDetailRunes`, `api.RequestedByDashboard`, `store.RecordPoll`, `store.CreateControl`; Task 2's routes and `server.New(st *store.Store, readToken string, logger *log.Logger) *server.Server`; Task 3's `resume.Control`, `resume.ControlOptions`, `resume.ControlResult`, and the `resume.Outcome*` constants.
- Produces:
  - `func (c *client.Client) PollControl(ctx context.Context, wait time.Duration) (*api.ControlClaim, error)`: `nil, nil` on `204`; an error for a `200` without a request.
  - `func (c *client.Client) PostControlResult(ctx context.Context, id string, in api.ControlResultIn) error`.
  - `client.PollSlack = 10 * time.Second`: added to a poll's wait for its timeout.
  - `type herdrtest.Handler func(params json.RawMessage) (result any, code string)`, `func (s *herdrtest.Server) Handle(method string, fn Handler)`.
  - In `internal/plugin`: `controlRunner(socket string, every, wait time.Duration) controlFunc`, `newController(...) *controller`, `(*controller).loop(ctx)`, `(*controller).once(ctx) time.Duration`, `resultFor(resume.ControlResult) api.ControlResultIn`.

- [ ] **Step 1: Write the failing client tests**

Add to `internal/client/http_test.go` (add `"sync"` to its imports if it isn't there):

```go
// pollServer answers each request with the status and body set last.
type pollServer struct {
	*httptest.Server
	mu     sync.Mutex
	status int
	body   string
	delay  time.Duration
	query  string
	path   string
	got    string // the last request body
}

func newPollServer(t *testing.T) *pollServer {
	p := &pollServer{status: 204}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.query, p.path, p.got = r.URL.RawQuery, r.URL.EscapedPath(), string(b)
		status, body, delay := p.status, p.body, p.delay
		p.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer hub_m_x" {
			t.Errorf("Authorization %q", r.Header.Get("Authorization"))
		}
		time.Sleep(delay)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *pollServer) set(status int, body string, delay time.Duration) {
	p.mu.Lock()
	p.status, p.body, p.delay = status, body, delay
	p.mu.Unlock()
}

func (p *pollServer) last() (query, path, body string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.query, p.path, p.got
}

func TestPollControl(t *testing.T) {
	ctx := context.Background()
	p := newPollServer(t)
	c, _ := New(Config{ServerURL: p.URL, Token: "hub_m_x"})

	claim, err := c.PollControl(ctx, 30*time.Second)
	if q, path, _ := p.last(); claim != nil || err != nil || q != "wait=30" || path != "/v1/machines/self/control" {
		t.Errorf("204: %+v %v, query %q path %q", claim, err, q, path)
	}
	// A poll outlives the 2 s limit of every other call: wait plus PollSlack.
	p.set(204, "", 2500*time.Millisecond)
	start := time.Now()
	if claim, err := c.PollControl(ctx, time.Second); claim != nil || err != nil || time.Since(start) < 2500*time.Millisecond {
		t.Errorf("slow 204: %+v %v after %s", claim, err, time.Since(start))
	}
	if q, _, _ := p.last(); q != "wait=1" {
		t.Errorf("query %q", q)
	}
	p.set(200, `{"request":{"id":"cr_x","state":"claimed"},"session":{"id":"s1","herdr_pane":"w1:p1"}}`, 0)
	if claim, err := c.PollControl(ctx, time.Second); err != nil || claim == nil || claim.Request.ID != "cr_x" || claim.Session.HerdrPane != "w1:p1" {
		t.Errorf("200: %+v %v", claim, err)
	}
	// A 200 without a request is a broken server, not "nothing pending".
	p.set(200, `{}`, 0)
	if claim, err := c.PollControl(ctx, time.Second); err == nil || claim != nil {
		t.Errorf("empty 200: %+v %v", claim, err)
	}
	p.set(401, `{"error":"missing or invalid bearer token"}`, 0)
	if _, err := c.PollControl(ctx, time.Second); !IsAuthError(err) {
		t.Errorf("401: %v", err)
	}
}

func TestPostControlResult(t *testing.T) {
	p := newPollServer(t)
	p.set(200, `{"id":"cr_x","state":"done"}`, 0)
	c, _ := New(Config{ServerURL: p.URL, Token: "hub_m_x"})
	in := api.ControlResultIn{State: "done", URL: "https://claude.ai/code/session_abc", Detail: "sent"}
	if err := c.PostControlResult(context.Background(), "cr_x/../y", in); err != nil {
		t.Fatal(err)
	}
	_, path, body := p.last()
	var got api.ControlResultIn
	// The ID is one escaped path segment: it can't climb to another route.
	if path != "/v1/control/cr_x%2F..%2Fy/result" {
		t.Errorf("path %q", path)
	}
	if json.Unmarshal([]byte(body), &got) != nil || got != in {
		t.Errorf("body %s", body)
	}
}
```

Run: `GOFLAGS=-count=1 go test ./internal/client/ -run 'TestPollControl|TestPostControlResult|TestTimeoutAndUnreachable'`
Expected: FAIL to compile: `c.PollControl undefined`.

- [ ] **Step 2: Add the client calls**

In `internal/client/http.go`, change `New` so the per-call context is the only time limit (the long poll needs more than `Timeout`):

```go
	return &Client{
		base:  strings.TrimRight(cfg.ServerURL, "/"),
		token: cfg.Token,
		// No http.Client.Timeout: doTimeout bounds every call with a context
		// deadline, which the long poll sets longer than Timeout.
		http: &http.Client{},
	}, nil
```

Rename `func (c *Client) do(ctx context.Context, method, path string, in, out any) error` to `doTimeout(ctx context.Context, timeout time.Duration, method, path string, in, out any) error`, change its `context.WithTimeout(ctx, Timeout)` to `context.WithTimeout(ctx, timeout)`, and add:

```go
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	return c.doTimeout(ctx, Timeout, method, path, in, out)
}

// PollSlack is added to a long poll's wait for its timeout.
const PollSlack = 10 * time.Second

// PollControl holds GET /v1/machines/self/control open for up to wait
// (whole seconds, at least 1) and returns the request this machine claimed,
// or nil when none arrived (204).
func (c *Client) PollControl(ctx context.Context, wait time.Duration) (*api.ControlClaim, error) {
	secs := max(int(wait/time.Second), 1)
	var raw json.RawMessage
	if err := c.doTimeout(ctx, time.Duration(secs)*time.Second+PollSlack, http.MethodGet,
		"/v1/machines/self/control?wait="+strconv.Itoa(secs), nil, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil // 204
	}
	var claim api.ControlClaim
	if err := json.Unmarshal(raw, &claim); err != nil {
		return nil, err
	}
	if claim.Request.ID == "" {
		return nil, errors.New("sessionhub: control poll answered without a request")
	}
	return &claim, nil
}

// PostControlResult reports what happened to a claimed request.
func (c *Client) PostControlResult(ctx context.Context, id string, in api.ControlResultIn) error {
	return c.do(ctx, http.MethodPost, "/v1/control/"+url.PathEscape(id)+"/result", in, nil)
}
```

Add `"strconv"` to the imports.

Run: `GOFLAGS=-count=1 go test ./internal/client/`
Expected: `ok`. `TestTimeoutAndUnreachable` still gives up after about 2 s: the context deadline does what `http.Client.Timeout` did.

- [ ] **Step 3: Let `herdrtest` script one method**

In `internal/herdr/herdrtest/herdrtest.go`, add a `handlers map[string]Handler` field to `Server`, and:

```go
// Handler answers one method in place of the fixtures: a result object, or
// a non-empty error code.
type Handler func(params json.RawMessage) (result any, code string)

// Handle sends every request for method to fn instead of the fixtures. Use
// it where one request must get different answers over time, such as a
// pane.read before and after a prompt. fn runs without the server's lock, so
// it may call Requests.
func (s *Server) Handle(method string, fn Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handlers == nil {
		s.handlers = map[string]Handler{}
	}
	s.handlers[method] = fn
}
```

Replace `handle` with:

```go
func (s *Server) handle(c net.Conn) {
	defer c.Close()
	// Like the real herdr, answer one request and close the connection.
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	if !sc.Scan() {
		return
	}
	var req Request
	if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
		c.Write([]byte(`{"id":"","error":{"code":"invalid_request","message":"bad json"}}` + "\n"))
		return
	}
	s.mu.Lock()
	s.requests = append(s.requests, req)
	fn := s.handlers[req.Method]
	reply, ok := s.replies[key(req.Method, req.Params)]
	s.mu.Unlock()
	var out []byte
	switch {
	case fn != nil:
		res, code := fn(req.Params)
		if code != "" {
			out, _ = json.Marshal(map[string]any{"id": req.ID, "error": map[string]string{"code": code, "message": code}})
		} else {
			out, _ = json.Marshal(map[string]any{"id": req.ID, "result": res})
		}
	case !ok:
		out, _ = json.Marshal(map[string]any{"id": req.ID, "error": map[string]string{
			"code": "no_fixture", "message": "no fixture for " + key(req.Method, req.Params)}})
	default:
		// Keep the captured response, swap in the caller's request ID.
		var m map[string]json.RawMessage
		if json.Unmarshal(reply, &m) != nil {
			return
		}
		id, _ := json.Marshal(req.ID)
		m["id"] = id
		out, _ = json.Marshal(m)
	}
	c.Write(append(out, '\n'))
}
```

Run: `GOFLAGS=-count=1 go test ./internal/herdr/...`
Expected: `ok` for `internal/herdr` (herdrtest has no tests of its own; the plugin tests below use `Handle`).

- [ ] **Step 4: Write the failing watcher tests**

Create `internal/plugin/control_test.go`:

```go
package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr/herdrtest"
	"github.com/abdallah/session-hub/internal/resume"
	"github.com/abdallah/session-hub/internal/server"
	"github.com/abdallah/session-hub/internal/store"
)

const rcLink = "https://claude.ai/code/session_01AbCdEfGhIjKlMnOpQrStUv"

// liveHub is the real sessionhub server over a temp database, with machine tower.
type liveHub struct {
	st  *store.Store
	srv *httptest.Server
	tok string
	id  int64
}

func newLiveHub(t *testing.T) *liveHub {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "sessionhub.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	tok, _, err := st.AddMachine(ctx, "tower", "", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.MachineByToken(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(st, "hub_r_watcher-test-read-token-0123456789", nil).Handler())
	t.Cleanup(srv.Close)
	return &liveHub{st: st, srv: srv, tok: tok, id: m.ID}
}

func (h *liveHub) clientFunc() func() (*client.Client, error) {
	return func() (*client.Client, error) { return client.New(client.Config{ServerURL: h.srv.URL, Token: h.tok}) }
}

// open registers the session (ended if asked), marks tower's watcher as
// polling, and opens a request as a dashboard tap does.
func (h *liveHub) open(t *testing.T, u api.SessionUpsert, ended bool) api.ControlRequest {
	t.Helper()
	ctx := context.Background()
	c, _ := h.clientFunc()()
	if err := c.UpsertSession(ctx, u); err != nil {
		t.Fatal(err)
	}
	if ended {
		if err := c.PostEvent(ctx, u.ID, api.EventIn{Kind: api.KindEnded, Source: api.SourcePlugin}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.st.RecordPoll(ctx, h.id); err != nil {
		t.Fatal(err)
	}
	req, created, err := h.st.CreateControl(ctx, u.ID, api.RequestedByDashboard)
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	return req
}

// scriptHerdr is a herdr where pane (if not "") runs Claude with session,
// using the captured agent.prompt answers for wS:p1 (ok) and wS:p2 (blocked).
func scriptHerdr(t *testing.T, pane, session string) *herdrtest.Server {
	hs := herdrtest.New(t, "agent-prompt-ok.ndjson", "agent-prompt-blocked.ndjson")
	acted := func() bool {
		for _, r := range hs.Requests() {
			if r.Method == "agent.prompt" || r.Method == "agent.start" {
				return true
			}
		}
		return false
	}
	hs.Handle("pane.get", func(p json.RawMessage) (any, string) {
		var in struct {
			PaneID string `json:"pane_id"`
		}
		json.Unmarshal(p, &in)
		if pane == "" || in.PaneID != pane {
			return nil, "pane_not_found"
		}
		return map[string]any{"type": "pane_info", "pane": map[string]any{
			"pane_id": pane, "workspace_id": "wS", "agent": "claude", "agent_status": "idle",
			"agent_session": map[string]any{"source": "herdr:claude", "agent": "claude", "kind": "id", "value": session}}}, ""
	})
	hs.Handle("session.snapshot", func(json.RawMessage) (any, string) {
		return map[string]any{"type": "session_snapshot", "snapshot": map[string]any{"panes": []any{}}}, ""
	})
	hs.Handle("pane.read", func(json.RawMessage) (any, string) {
		text := "\n❯ \n"
		if acted() {
			text = "\n❯ /remote-control\n  /remote-control is active · Continue here, on your phone, or at " + rcLink + "\n"
		}
		return map[string]any{"type": "pane_read", "read": map[string]any{"text": text}}, ""
	})
	hs.Handle("workspace.create", func(json.RawMessage) (any, string) {
		return map[string]any{"type": "workspace_created", "workspace": map[string]any{"workspace_id": "wN"},
			"root_pane": map[string]any{"pane_id": "wN:p1", "workspace_id": "wN"}}, ""
	})
	hs.Handle("agent.start", func(json.RawMessage) (any, string) { return map[string]any{"type": "agent_started"}, "" })
	return hs
}

// The watcher, end to end: the real server, a herdrtest socket, and
// resume.Control. Each row opens a request, runs one poll, and reads the
// result back from the API.
func TestControlDecisionTable(t *testing.T) {
	dir := t.TempDir()
	gone := filepath.Join(dir, "gone")
	cases := []struct {
		name       string
		pane       string // recorded herdr pane
		herdrPane  string // the pane herdr knows ("" for none)
		ended      bool
		cwd        string
		wantState  string
		wantURL    string
		wantDetail string
		calls      []string
		never      []string
	}{
		{"inject into the running pane", "wS:p1", "wS:p1", false, dir, api.ControlDone, rcLink, "",
			[]string{"pane.get", "agent.prompt"}, []string{"workspace.create", "agent.start"}},
		{"blocked: fail, type nothing", "wS:p2", "wS:p2", false, dir, api.ControlFailed, "", "waiting on a prompt in the pane",
			[]string{"agent.prompt"}, []string{"workspace.create", "agent.start", "pane.send_keys"}},
		{"ended: resume in a new workspace", "wS:p3", "", true, dir, api.ControlDone, rcLink, "resumed in a new herdr workspace",
			[]string{"workspace.create", "agent.start"}, []string{"agent.prompt"}},
		{"looks live but pane not found", "wS:p3", "", false, dir, api.ControlFailed, "", "looks live but pane not found",
			[]string{"session.snapshot"}, []string{"workspace.create", "agent.start", "agent.prompt"}},
		{"missing directory", "wS:p3", "", true, gone, api.ControlFailed, "", "directory no longer exists",
			nil, []string{"workspace.create", "agent.start"}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			sessionhub := newLiveHub(t)
			id := fmt.Sprintf("0a1b2c3d-1111-4222-8333-%012d", i)
			hs := scriptHerdr(t, tc.herdrPane, id)
			req := sessionhub.open(t, api.SessionUpsert{ID: id, Agent: "claude", Source: api.SourcePlugin, CWD: tc.cwd,
				HerdrSession: "default", HerdrPane: tc.pane, TitleHint: "rc scratch"}, tc.ended)
			var logs bytes.Buffer
			ctl := newController(sessionhub.clientFunc(), controlRunner(hs.Path, 5*time.Millisecond, 300*time.Millisecond), log.New(&logs, "", 0))
			ctl.wait = time.Second
			if d := ctl.once(ctx); d != 0 {
				t.Fatalf("once paused %s; log:\n%s", d, logs.String())
			}
			c, _ := sessionhub.clientFunc()()
			d, err := c.GetSession(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			rc := d.RemoteControl
			if rc == nil || rc.ID != req.ID || rc.State != tc.wantState || rc.URL != tc.wantURL || !strings.Contains(rc.Detail, tc.wantDetail) {
				t.Fatalf("request after the poll: %+v; log:\n%s", rc, logs.String())
			}
			if d.RemoteControlURL != tc.wantURL {
				t.Errorf("session link %q, want %q", d.RemoteControlURL, tc.wantURL)
			}
			results := 0
			for _, ev := range d.Events {
				if ev.Kind == api.KindRemoteControlResult {
					results++
				}
			}
			if results != 1 {
				t.Errorf("%d result events, want 1", results)
			}
			var methods []string
			for _, r := range hs.Requests() {
				methods = append(methods, r.Method)
			}
			got := "," + strings.Join(methods, ",") + ","
			for _, m := range tc.calls {
				if !strings.Contains(got, ","+m+",") {
					t.Errorf("%s never ran; methods %s", m, got)
				}
			}
			for _, m := range tc.never {
				if strings.Contains(got, ","+m+",") {
					t.Errorf("%s ran; methods %s", m, got)
				}
			}
			if tc.name == "ended: resume in a new workspace" {
				for _, r := range hs.Requests() {
					var p map[string]any
					json.Unmarshal(r.Params, &p)
					if r.Method == "workspace.create" && (p["cwd"] != dir || p["label"] != "sessionhub: rc scratch" || p["focus"] != false) {
						t.Errorf("workspace.create params %v", p)
					}
					if r.Method == "agent.start" && fmt.Sprint(p["args"]) != "[--resume "+id+" --remote-control]" {
						t.Errorf("agent.start params %v", p)
					}
				}
			}
		})
	}
}

func TestResultFor(t *testing.T) {
	long := strings.Repeat("é", 300)
	cases := []struct {
		in   resume.ControlResult
		want api.ControlResultIn
	}{
		{resume.ControlResult{Outcome: resume.OutcomeLinked, URL: rcLink}, api.ControlResultIn{State: "done", URL: rcLink}},
		{resume.ControlResult{Outcome: resume.OutcomeAlreadyOn, URL: rcLink}, api.ControlResultIn{State: "done", URL: rcLink, Detail: "Remote Control was already on"}},
		{resume.ControlResult{Outcome: resume.OutcomeAlreadyOn, URL: rcLink, DialogLeftOpen: "Claude is working, so sessionhub did not press Esc. Press Esc in the pane to close the dialog."},
			api.ControlResultIn{State: "done", URL: rcLink, Detail: "Remote Control was already on; press Esc in the pane to close its dialog"}},
		{resume.ControlResult{Outcome: resume.OutcomeNoLink}, api.ControlResultIn{State: "done", Detail: "sent, link not seen"}},
		{resume.ControlResult{Outcome: resume.OutcomeResumed, URL: rcLink}, api.ControlResultIn{State: "done", URL: rcLink, Detail: "resumed in a new herdr workspace"}},
		{resume.ControlResult{Outcome: resume.OutcomeResumed}, api.ControlResultIn{State: "done", Detail: "resumed in a new herdr workspace, link not seen"}},
		{resume.ControlResult{Outcome: resume.OutcomeBlocked}, api.ControlResultIn{State: "failed", Detail: "waiting on a prompt in the pane"}},
		{resume.ControlResult{Outcome: resume.OutcomeDialogOpen}, api.ControlResultIn{State: "failed", Detail: "a Remote Control dialog is open in the pane; press Esc there"}},
		{resume.ControlResult{Outcome: resume.OutcomeLooksLive}, api.ControlResultIn{State: "failed", Detail: "looks live but pane not found"}},
		{resume.ControlResult{Outcome: resume.OutcomeNoDir}, api.ControlResultIn{State: "failed", Detail: "the session's directory no longer exists"}},
		{resume.ControlResult{Outcome: resume.OutcomeNotRunning}, api.ControlResultIn{State: "failed", Detail: "not running"}},
		// herdr error text can hold anything: control characters go, and
		// the detail fits the server's free-text rules.
		{resume.ControlResult{Outcome: resume.OutcomeError, Err: errors.New("herdr: agent_not_found: x\x1b[31m\nnext")},
			api.ControlResultIn{State: "failed", Detail: "herdr: agent_not_found: x[31m next"}},
		{resume.ControlResult{Outcome: resume.OutcomeError, Err: errors.New(long)},
			api.ControlResultIn{State: "failed", Detail: strings.Repeat("é", 199) + "…"}},
		{resume.ControlResult{Outcome: resume.OutcomeError}, api.ControlResultIn{State: "failed", Detail: "herdr call failed"}},
	}
	for _, tc := range cases {
		if got := resultFor(tc.in); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.in.Outcome, got, tc.want)
		}
	}
}

// statusServer answers the control poll with one status per call, in order;
// a 200 carries "{}".
func statusServer(t *testing.T, statuses ...int) *httptest.Server {
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		st := statuses[min(n, len(statuses)-1)]
		n++
		mu.Unlock()
		if r.URL.Path != "/v1/machines/self/control" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(st)
		if st == 200 {
			io.WriteString(w, "{}")
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestControlBackoff(t *testing.T) {
	ctx := context.Background()
	quiet := log.New(io.Discard, "", 0)
	srv := statusServer(t, 502, 502, 502, 502, 502, 502, 204, 503, 401, 403, 429, 200)
	ctl := newController(func() (*client.Client, error) {
		return client.New(client.Config{ServerURL: srv.URL, Token: "hub_m_test"})
	}, nil, quiet)
	want := []time.Duration{
		5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second, 60 * time.Second, // network errors
		time.Second,                      // a 204 that came back at once: at most one poll a second
		5 * time.Second,                  // the backoff starts over after a success
		5 * time.Minute, 5 * time.Minute, // 401, 403
		5 * time.Second,  // 429 (too many polls) backs off like a network error
		10 * time.Second, // a 200 without a request is a broken server
	}
	for i, w := range want {
		if got := ctl.once(ctx); got != w {
			t.Errorf("poll %d: pause %s, want %s", i+1, got, w)
		}
	}
	// A server that isn't there backs off the same way.
	dead := newController(func() (*client.Client, error) {
		return client.New(client.Config{ServerURL: deadURL(t), Token: "hub_m_test"})
	}, nil, quiet)
	if got := dead.once(ctx); got != 5*time.Second {
		t.Errorf("dead server: pause %s", got)
	}
	// No server_url: a config problem, paused like an auth error.
	nocfg := newController(func() (*client.Client, error) { return client.New(client.Config{}) }, nil, quiet)
	if got := nocfg.once(ctx); got != 5*time.Minute {
		t.Errorf("no config: pause %s", got)
	}
}

// loop keeps polling and pausing until its context ends, then returns.
func TestControlLoopStopsWithContext(t *testing.T) {
	srv := statusServer(t, 502)
	var logs bytes.Buffer
	ctl := newController(func() (*client.Client, error) {
		return client.New(client.Config{ServerURL: srv.URL, Token: "hub_m_test"})
	}, nil, log.New(&logs, "", 0))
	ctx, cancel := context.WithCancel(context.Background())
	var pauses []time.Duration
	ctl.sleep = func(ctx context.Context, d time.Duration) bool {
		pauses = append(pauses, d)
		if len(pauses) == 3 {
			cancel()
			return false
		}
		return true
	}
	done := make(chan struct{})
	go func() { ctl.loop(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not return after its context ended")
	}
	if fmt.Sprint(pauses) != "[5s 10s 20s]" {
		t.Errorf("pauses %v", pauses)
	}
	if !strings.Contains(logs.String(), "control: poll failed; retrying in 5s") {
		t.Errorf("log:\n%s", logs.String())
	}
}
```

Run: `GOFLAGS=-count=1 go test ./internal/plugin/ -run 'TestControl|TestResultFor'`
Expected: FAIL to compile: `undefined: newController`, `undefined: controlRunner`, `undefined: resultFor`.

- [ ] **Step 5: Write the control loop**

Create `internal/plugin/control.go`:

```go
package plugin

import (
	"context"
	"log"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
	"github.com/abdallah/session-hub/internal/client"
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
)

// controlFunc turns on Remote Control for one session on this machine.
type controlFunc func(ctx context.Context, s api.Session) resume.ControlResult

// controlRunner is the watcher's controlFunc: resume.Control on the herdr
// socket, resuming a session that no pane runs. Zero every or wait means
// resume's defaults.
func controlRunner(socket string, every, wait time.Duration) controlFunc {
	return func(ctx context.Context, s api.Session) resume.ControlResult {
		return resume.Control(ctx, s, resume.ControlOptions{Socket: socket, Resume: true, PollEvery: every, PollFor: wait})
	}
}

// controller keeps one long poll open to the sessionhub and runs each Remote
// Control request it claims. It never touches the queue or its lock.
type controller struct {
	newClient func() (*client.Client, error)
	run       controlFunc
	log       *log.Logger
	wait      time.Duration
	// sleep pauses for d, or returns false when ctx ends first. Tests
	// replace it.
	sleep   func(ctx context.Context, d time.Duration) bool
	backoff time.Duration // the last network-error pause; 0 after a success
}

func newController(newClient func() (*client.Client, error), run controlFunc, logger *log.Logger) *controller {
	return &controller{newClient: newClient, run: run, log: logger, wait: controlWait, sleep: sleepCtx}
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

// handle runs one claimed request and posts its result. A result the server
// refuses (the request expired meanwhile) or can't take is logged, not
// retried: the request expires, and the dashboard says so.
func (c *controller) handle(ctx context.Context, cl *client.Client, claim api.ControlClaim) {
	req := claim.Request
	in := api.ControlResultIn{State: api.ControlFailed, Detail: "unknown action " + termtext.Clean(req.Action, 40)}
	if req.Action == api.ActionRemoteControl {
		in = resultFor(c.run(ctx, claim.Session))
	}
	c.log.Printf("control: request %s for session %s: %s %s", req.ID, claim.Session.ID, in.State, in.Detail)
	if err := cl.PostControlResult(ctx, req.ID, in); err != nil {
		c.log.Printf("control: result for request %s not recorded: %v", req.ID, err)
	}
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
	}
	msg := detailHerdrFailed
	if r.Err != nil {
		msg = r.Err.Error()
	}
	return failed(termtext.Clean(msg, api.MaxControlDetailRunes))
}
```

- [ ] **Step 6: Start the loop with the watcher**

In `internal/plugin/watcher.go`, `runWatcher`, after the `logger.Printf("watcher started: ...")` line:

```go
	// The control loop runs beside the 2 s ticks and stops with them. Wait
	// for it on the way out, so a request it is running ends with the
	// watcher, not after it.
	ctl := newController(newClient, controlRunner(socketPath, 0, controlPollFor), logger)
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
```

- [ ] **Step 7: Run the plugin tests**

Run: `GOFLAGS=-count=1 go test ./internal/plugin/ -run 'TestControl|TestResultFor|TestRunWatcher' -v 2>&1 | grep -E '^(--- |ok|FAIL)'`
Expected: every test `--- PASS`. `TestRunWatcherLifecycle` still passes: its fake sessionhub answers the control poll with `200 {}`, which the loop treats as a broken server and pauses 5 s on, and the socket's removal cancels that pause.

Then run: `make test lint`
Expected: every package `ok`; `lint` prints nothing.

- [ ] **Step 8: Commit**

```bash
git add internal/client internal/herdr/herdrtest internal/plugin
git commit -F - <<'MSG'
Run Remote Control requests from the plugin watcher

The watcher now keeps a long poll open on GET /v1/machines/self/control
beside its 2 s ticks. For each claimed request it calls resume.Control
with Resume on, then posts done or failed with the link or the reason. A
network error backs off from 5 s to 60 s; 401 or 403 waits 5 minutes; a
204 that comes back at once waits 1 s, so a shutting-down server can't
spin the loop.

client.PollControl sets its own timeout (wait + 10 s): every call is now
bounded by a context deadline instead of http.Client.Timeout. herdrtest
gains Handle, so a test can answer one method with a script, such as
pane.read before and after /remote-control.

Docs: Task 7 updates docs/plugin.md (Watcher) and docs/client.md.

Refs: docs/dev/superpowers/plans/2026-09-30-remote-control-button.md Task 4

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
MSG
```

---
### Task 5: The CLI's remote branch goes through the server

**Files:**
- Modify: `internal/client/http.go` (add `CreateControl`)
- Modify: `internal/client/http_test.go` (`TestCreateControl`)
- Modify: `internal/resume/resume.go` (`hubAPI` gains `CreateControl`; `env` gains `ctlEvery`, `ctlFor`)
- Modify: `internal/resume/remotecontrol.go` (`remoteControlRemote` asks the server; the printing moves to `remoteControlSSH`; add `awaitControl`)
- Modify: `internal/resume/hostile_test.go` (`stubAPI.CreateControl`)
- Create: `internal/resume/remote_test.go`

**Interfaces:**
- Consumes: Task 1's `api.ControlRequest`, `api.ControlResultIn`, `api.ControlDone`, `api.ControlFailed`, `api.ControlExpired`, `store.Open`, `store.Options`, `store.RecordPoll`; Task 2's `server.New` and routes; Task 4's `client.PollControl`, `client.PostControlResult`.
- Produces:
  - `func (c *client.Client) CreateControl(ctx context.Context, id string) (api.ControlRequest, error)`.
  - In `internal/resume`: `hubAPI.CreateControl`, `env.ctlEvery` and `env.ctlFor` (zero means 2 s and 2 min), `(*env).remoteControlSSH(ctx, s api.Session, why string) error` (the old printing, preceded by the line `why` once the hosts pass their check), `(*env).awaitControl(ctx, sessionID, reqID string) (api.ControlRequest, bool)`.

- [ ] **Step 1: Write the failing client test**

Add to `internal/client/http_test.go` (it reuses `newPollServer` from Task 4):

```go
func TestCreateControl(t *testing.T) {
	p := newPollServer(t)
	p.set(202, `{"id":"cr_x","session_id":"s1","state":"pending"}`, 0)
	c, _ := New(Config{ServerURL: p.URL, Token: "hub_m_x"})
	r, err := c.CreateControl(context.Background(), "s1")
	if _, path, _ := p.last(); err != nil || r.ID != "cr_x" || r.State != "pending" || path != "/v1/sessions/s1/remote-control" {
		t.Errorf("202: %+v %v %q", r, err, path)
	}
	p.set(409, `{"error":"not controllable: session s1 is not in herdr"}`, 0)
	var se *StatusError
	if _, err := c.CreateControl(context.Background(), "s1"); !errors.As(err, &se) || se.Status != 409 || se.Message != "not controllable: session s1 is not in herdr" {
		t.Errorf("409: %v", err)
	}
}
```

Run: `GOFLAGS=-count=1 go test ./internal/client/ -run TestCreateControl`
Expected: FAIL to compile: `c.CreateControl undefined`.

- [ ] **Step 2: Add `CreateControl`**

In `internal/client/http.go`:

```go
// CreateControl asks the sessionhub to turn on Remote Control for session id. It
// returns the new request, or the session's open one.
func (c *Client) CreateControl(ctx context.Context, id string) (api.ControlRequest, error) {
	var r api.ControlRequest
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(id)+"/remote-control", struct{}{}, &r)
	return r, err
}
```

Run: `GOFLAGS=-count=1 go test ./internal/client/`
Expected: `ok`.

- [ ] **Step 3: Write the failing CLI tests**

Create `internal/resume/remote_test.go`:

```go
package resume

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/server"
	"github.com/abdallah/session-hub/internal/store"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *testClock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// realHub is the real sessionhub server with machines bluebox and tower, and one tower
// session with a herdr pane.
type realHub struct {
	st              *store.Store
	clock           *testClock
	srv             *httptest.Server
	tokBlu, tokTower string
}

// newRealHub starts the sessionhub. With polled, tower's watcher counts as polling.
func newRealHub(t *testing.T, polled bool) *realHub {
	t.Helper()
	ctx := context.Background()
	clock := &testClock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	st, err := store.Open(filepath.Join(t.TempDir(), "sessionhub.db"), store.Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tokTower, _, err := st.AddMachine(ctx, "tower", "tower.example.com", "tower.example.com")
	if err != nil {
		t.Fatal(err)
	}
	tokBlu, _, err := st.AddMachine(ctx, "bluebox", "bluebox.example.com", "bluebox.example.com")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(st, "hub_r_resume-test-read-token-0123456789", nil).Handler())
	t.Cleanup(srv.Close)
	h := &realHub{st: st, clock: clock, srv: srv, tokBlu: tokBlu, tokTower: tokTower}
	tower := h.client(t, tokTower)
	if err := tower.UpsertSession(ctx, api.SessionUpsert{ID: uuid, Agent: "claude", Source: api.SourcePlugin,
		CWD: "/home/user/my project", HerdrSession: "default", HerdrPane: "w1:p1"}); err != nil {
		t.Fatal(err)
	}
	if polled {
		m, err := st.MachineByToken(ctx, tokTower)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.RecordPoll(ctx, m.ID); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (h *realHub) client(t *testing.T, token string) *client.Client {
	t.Helper()
	c, err := client.New(client.Config{ServerURL: h.srv.URL, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// cli is `sessionhub` on bluebox, polling fast.
func (h *realHub) cli(t *testing.T, ctlFor time.Duration) (*env, *bytes.Buffer) {
	cfg := client.Config{ServerURL: h.srv.URL, Token: h.tokBlu, Machine: "bluebox"}
	out := &bytes.Buffer{}
	return &env{
		cfg: cfg, api: h.client(t, h.tokBlu), socket: filepath.Join(t.TempDir(), "none.sock"),
		saved: func(context.Context) []SavedMachine { return nil },
		in:    strings.NewReader(""), out: out,
		ctlEvery: 5 * time.Millisecond, ctlFor: ctlFor,
	}, out
}

// answer plays tower's watcher: it claims the next request and posts in.
func (h *realHub) answer(t *testing.T, in api.ControlResultIn) <-chan error {
	c := h.client(t, h.tokTower)
	ch := make(chan error, 1)
	go func() {
		claim, err := c.PollControl(context.Background(), 5*time.Second)
		if err != nil || claim == nil {
			ch <- fmt.Errorf("poll: %v %v", claim, err)
			return
		}
		ch <- c.PostControlResult(context.Background(), claim.Request.ID, in)
	}()
	return ch
}

func TestRemoteControlThroughHub(t *testing.T) {
	ssh := "ssh -t tower.example.com '~/.local/bin/sessionhub remote-control " + uuid + "'"
	ctx := context.Background()

	t.Run("done with a link: print it, no ssh", func(t *testing.T) {
		h := newRealHub(t, true)
		done := h.answer(t, api.ControlResultIn{State: api.ControlDone, URL: rcURL})
		e, out := h.cli(t, 3*time.Second)
		if err := e.remoteControlRun(ctx, uuid[:8]); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "Remote Control is on for session 0a1b2c3d:\n"+rcURL+"\n") || strings.Contains(out.String(), "ssh -t") {
			t.Errorf("output:\n%s", out)
		}
		// One request, recorded as bluebox's.
		d, _ := h.client(t, h.tokBlu).GetSession(ctx, uuid)
		if d.RemoteControl == nil || d.RemoteControl.RequestedBy != "machine:bluebox" || d.RemoteControlURL != rcURL {
			t.Errorf("session: %+v", d.Session)
		}
	})
	t.Run("done without a link: say so, no ssh", func(t *testing.T) {
		h := newRealHub(t, true)
		done := h.answer(t, api.ControlResultIn{State: api.ControlDone, Detail: "sent, link not seen"})
		e, out := h.cli(t, 3*time.Second)
		if err := e.remoteControlRun(ctx, uuid[:8]); err != nil {
			t.Fatal(err)
		}
		<-done
		if !strings.Contains(out.String(), "no link appeared (sent, link not seen)") || strings.Contains(out.String(), "ssh -t") {
			t.Errorf("output:\n%s", out)
		}
	})
	t.Run("failed: exit 1 with the reason, no ssh", func(t *testing.T) {
		h := newRealHub(t, true)
		done := h.answer(t, api.ControlResultIn{State: api.ControlFailed, Detail: "waiting on a prompt in the pane"})
		e, out := h.cli(t, 3*time.Second)
		err := e.remoteControlRun(ctx, uuid[:8])
		<-done
		if err == nil || !strings.Contains(err.Error(), `machine "tower" could not turn on Remote Control: waiting on a prompt in the pane`) {
			t.Errorf("err = %v", err)
		}
		if strings.Contains(out.String(), "ssh -t") {
			t.Errorf("output:\n%s", out)
		}
	})
	t.Run("expired: print the ssh backup", func(t *testing.T) {
		h := newRealHub(t, true)
		// No watcher answers. Once the request exists, move the sessionhub's clock
		// past its expiry. (The client is made here: t.Fatal must not run on
		// another goroutine.)
		c := h.client(t, h.tokBlu)
		go func() {
			for range 200 {
				if d, err := c.GetSession(ctx, uuid); err == nil && d.RemoteControl != nil {
					h.clock.Advance(3 * time.Minute)
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
		e, out := h.cli(t, 3*time.Second)
		if err := e.remoteControlRun(ctx, uuid[:8]); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), `Machine "tower" didn't respond in time.`) || !strings.Contains(out.String(), ssh) {
			t.Errorf("output:\n%s", out)
		}
	})
	t.Run("no answer within the wait: print the ssh backup", func(t *testing.T) {
		h := newRealHub(t, true)
		e, out := h.cli(t, 100*time.Millisecond)
		if err := e.remoteControlRun(ctx, uuid[:8]); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "didn't respond in time") || !strings.Contains(out.String(), ssh) {
			t.Errorf("output:\n%s", out)
		}
	})
	t.Run("watcher offline: 409, ssh backup at once", func(t *testing.T) {
		h := newRealHub(t, false)
		e, out := h.cli(t, 3*time.Second)
		start := time.Now()
		if err := e.remoteControlRun(ctx, uuid[:8]); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) > time.Second {
			t.Errorf("waited %s after a refused request", time.Since(start))
		}
		if !strings.Contains(out.String(), `Could not ask machine "tower" to turn on Remote Control`) ||
			!strings.Contains(out.String(), "offline") || !strings.Contains(out.String(), ssh) {
			t.Errorf("output:\n%s", out)
		}
	})
}
```

In `internal/resume/hostile_test.go`, add this method after `stubAPI.ListMachines`, with a blank line between them (and `"errors"` to its imports):

```go
func (s stubAPI) CreateControl(context.Context, string) (api.ControlRequest, error) {
	return api.ControlRequest{}, errors.New("stub: no sessionhub")
}
```

Run: `GOFLAGS=-count=1 go test ./internal/resume/ -run TestRemoteControlThroughHub`
Expected: FAIL to compile: `unknown field ctlEvery in struct literal of type env`.

- [ ] **Step 4: Ask the server, then fall back to the printed commands**

In `internal/resume/resume.go`, add to `hubAPI`:

```go
	CreateControl(ctx context.Context, id string) (api.ControlRequest, error)
```

and to `env`, after `pollEvery, pollFor time.Duration`:

```go
	// ctlEvery and ctlFor bound the wait for a request that another
	// machine's watcher runs; zero means 2 s and 2 min. Tests shorten them.
	ctlEvery, ctlFor time.Duration
```

In `internal/resume/remotecontrol.go`, add the constants:

```go
const (
	// ctlPollEvery and ctlPollFor bound the wait for another machine's
	// watcher to answer a request.
	ctlPollEvery = 2 * time.Second
	ctlPollFor   = 2 * time.Minute
)
```

Rename the existing `remoteControlRemote` to `remoteControlSSH`, give it a `why string` parameter, and print `why` right after the host check, so a refused host still prints nothing (`TestRemoteControlRefusals` checks that):

```go
// remoteControlSSH prints the commands that turn Remote Control on for a
// session on another machine, after the line why. It never runs them.
func (e *env) remoteControlSSH(ctx context.Context, s api.Session, why string) error {
	sshHost, herdrHost := e.hosts(ctx, s)
	if !validHost(sshHost) || !validHost(herdrHost) {
		return fmt.Errorf("refusing to print commands for machine %q: its SSH host %q or herdr host %q is empty or starts with a hyphen", s.Machine, sshHost, herdrHost)
	}
	fmt.Fprintln(e.out, why)
	// The rest of the old remoteControlRemote body, unchanged:
	fmt.Fprintf(e.out, "Session %s runs on machine %s, not on this machine. To turn on Remote Control:\n\n", shortID(s.ID), strconv.Quote(s.Machine))
	fmt.Fprintf(e.out, "ssh -t %s %s\n", shellQuote(sshHost), shellQuote(remoteHub+" remote-control "+s.ID))
	if s.HerdrPane == "" {
		return nil
	}
	if label := matchSaved(e.savedMachines(ctx), herdrHost); validHost(label) {
		// Comments never carry session data.
		fmt.Fprintf(e.out, "herdr --machine %s agent prompt %s %s     # saved herdr machine; sends the command without checking the pane\n",
			shellQuote(label), shellQuote(s.HerdrPane), rcCommand)
	}
	return nil
}
```

Then add:

```go
// remoteControlRemote asks the sessionhub to have the session's machine turn
// Remote Control on, and waits for the answer. When the request can't be
// made, or the machine doesn't answer before it expires, it prints the SSH
// commands instead.
func (e *env) remoteControlRemote(ctx context.Context, s api.Session) error {
	machine := strconv.Quote(s.Machine)
	req, err := e.api.CreateControl(ctx, s.ID)
	if err != nil {
		return e.remoteControlSSH(ctx, s, fmt.Sprintf("Could not ask machine %s to turn on Remote Control: %s", machine, clean(err.Error())))
	}
	fmt.Fprintf(e.out, "Asked machine %s to turn on Remote Control for session %s. Waiting for it...\n", machine, shortID(s.ID))
	r, ok := e.awaitControl(ctx, s.ID, req.ID)
	switch {
	case !ok || r.State == api.ControlExpired:
		return e.remoteControlSSH(ctx, s, fmt.Sprintf("Machine %s didn't respond in time.", machine))
	case r.State == api.ControlFailed:
		return fmt.Errorf("machine %s could not turn on Remote Control: %s", machine, clean(r.Detail))
	case r.URL != "":
		fmt.Fprintf(e.out, "Remote Control is on for session %s:\n%s\n", shortID(s.ID), clean(r.URL))
		if r.Detail != "" {
			fmt.Fprintf(e.out, "(%s)\n", clean(r.Detail))
		}
	default:
		fmt.Fprintf(e.out, "Machine %s sent /remote-control, but no link appeared (%s). Check the Claude app.\n", machine, clean(r.Detail))
	}
	return nil
}

// awaitControl reads the session every ctlEvery until request id is done,
// failed, or expired, for up to ctlFor. ok is false when time ran out first.
func (e *env) awaitControl(ctx context.Context, sessionID, id string) (api.ControlRequest, bool) {
	every, total := e.ctlEvery, e.ctlFor
	if every <= 0 {
		every = ctlPollEvery
	}
	if total <= 0 {
		total = ctlPollFor
	}
	deadline := time.Now().Add(total)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return api.ControlRequest{}, false
		case <-time.After(every):
		}
		d, err := e.api.GetSession(ctx, sessionID)
		if err != nil {
			continue // a failed read is like no answer yet
		}
		if r := d.RemoteControl; r != nil && r.ID == id {
			switch r.State {
			case api.ControlDone, api.ControlFailed, api.ControlExpired:
				return *r, true
			}
		}
	}
	return api.ControlRequest{}, false
}
```

`remoteControlSession` already validates the ID and pane before it calls `remoteControlRemote`, so a hostile ID never reaches the server.

- [ ] **Step 5: Run the resume tests**

Run: `GOFLAGS=-count=1 go test ./internal/resume/`
Expected: `ok`. The CLI task's `TestRemoteControlRemoteForms` and `TestHostileRemoteControlForms` still pass: their hubs refuse the request (the fake sessionhub answers 404, `stubAPI` an error), so they print one "Could not ask machine" line and then the same commands. The hostile test parses everything after the first blank line, which still starts at the commands.

Then run: `make test lint`
Expected: every package `ok`; `lint` prints nothing.

- [ ] **Step 6: Commit**

```bash
git add internal/client internal/resume
git commit -F - <<'MSG'
Send sessionhub remote-control for another machine through the sessionhub

For a session on another machine, sessionhub remote-control now posts a control
request and reads the session every 2 s for up to 2 minutes. It prints
the link, the note when no link appeared, or exits 1 with the machine's
reason. If the request can't be made (409 when the session isn't in herdr
or its watcher is offline, a network error) or it expires, it prints the
ssh command, and the herdr --machine form when a saved machine matches, as
before.

Docs: Task 7 updates docs/cli.md (Remote Control).

Refs: docs/dev/superpowers/plans/2026-09-30-remote-control-button.md Task 5

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
MSG
```

---
### Task 6: The dashboard button

**Files:**
- Modify: `internal/server/dashboard/index.html` (CSS rules; script: state, `rc-state` block, `rcRow`, `rcLink`, `requestRC`, `pollRC`, `refreshCard`, `card`, `render`)
- Modify: `internal/server/dashboard_test.go` (`TestDashboardRemoteControlStates`, `TestDashboardSendsOneKindOfWrite`)

**Interfaces:**
- Consumes: the session JSON fields from Task 1 (`controllable`, `status`, `remote_control` with `id`, `state`, `url`, `detail`; `remote_control_url`, `remote_control_at`); Task 2's `POST /v1/sessions/{id}/remote-control` with header `X-Hub-Action: remote-control` (answers `202`/`200` with a request, or an `api.Error`), and `GET /v1/sessions/{id}` with the cookie.
- Produces: in the page's script, between the markers `// rc-state:begin` and `// rc-state:end`, three pure functions the test runs under `node`: `rcButton(s)` → `{label, disabled, note}` or `null`; `rcOutcome(req)` → `{kind, text, url}` or `null`, with `kind` one of `sending`, `link`, `note`, `error`; `rcRefusal(status, body)` → string. Also `RC_URL`, the link pattern.

- [ ] **Step 1: Write the failing dashboard tests**

Add to `internal/server/dashboard_test.go` (add `"bytes"`, `"encoding/json"`, `"os/exec"`, and `"reflect"` to its imports):

```go
// The card logic runs under node, the only JavaScript engine on the
// machines that build sessionhub. Without node the table is skipped; the evidence
// for this task records a run with it.
func TestDashboardRemoteControlStates(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; this table runs the page's rc-state block under node")
	}
	m := regexp.MustCompile(`(?s)// rc-state:begin\n(.*?)// rc-state:end`).FindSubmatch(dashboardHTML)
	if m == nil {
		t.Fatal("dashboard/index.html has no rc-state block")
	}
	type button struct {
		Label    string `json:"label"`
		Disabled bool   `json:"disabled"`
		Note     string `json:"note"`
	}
	type outcome struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
		URL  string `json:"url"`
	}
	const link = "https://claude.ai/code/session_01AbCdEfGhIjKlMnOpQrStUv"
	rc := &button{Label: "Remote Control"}
	resume := &button{Label: "Resume with Remote Control"}
	blocked := &button{Label: "Remote Control", Disabled: true, Note: "waiting on a prompt in the pane"}
	buttons := []struct {
		s    map[string]any
		want *button
	}{
		{map[string]any{"status": "live", "agent_state": "idle", "controllable": true}, rc},
		{map[string]any{"status": "live", "agent_state": "working", "controllable": true}, rc},
		{map[string]any{"status": "live", "agent_state": "done", "controllable": true}, rc},
		{map[string]any{"status": "blocked", "agent_state": "blocked", "controllable": true}, blocked},
		{map[string]any{"status": "stale", "agent_state": "idle", "controllable": true}, resume},
		{map[string]any{"status": "ended", "controllable": true}, resume},
		{map[string]any{"status": "live", "agent_state": "idle", "controllable": false}, nil},
		{map[string]any{"status": "ended"}, nil},
	}
	outcomes := []struct {
		req  map[string]any
		want *outcome
	}{
		{nil, nil},
		{map[string]any{"state": "pending"}, &outcome{"sending", "Sending…", ""}},
		{map[string]any{"state": "claimed"}, &outcome{"sending", "Sending…", ""}},
		{map[string]any{"state": "done", "url": link}, &outcome{"link", "", link}},
		{map[string]any{"state": "done", "url": link, "detail": "resumed in a new herdr workspace"}, &outcome{"link", "resumed in a new herdr workspace", link}},
		{map[string]any{"state": "done", "detail": "sent, link not seen"}, &outcome{"note", "sent, link not seen", ""}},
		{map[string]any{"state": "done", "url": "javascript:alert(1)"}, &outcome{"note", "Sent, but no link appeared.", ""}},
		{map[string]any{"state": "done", "url": "https://evil.example/code/session_x"}, &outcome{"note", "Sent, but no link appeared.", ""}},
		{map[string]any{"state": "done", "url": link + "\n"}, &outcome{"note", "Sent, but no link appeared.", ""}},
		{map[string]any{"state": "failed", "detail": "looks live but pane not found"}, &outcome{"error", "looks live but pane not found", ""}},
		{map[string]any{"state": "failed"}, &outcome{"error", "The machine could not turn on Remote Control.", ""}},
		{map[string]any{"state": "expired"}, &outcome{"error", "The machine didn't respond in time.", ""}},
		{map[string]any{"state": "surprise"}, nil},
	}
	refusals := []struct {
		status int
		body   any
		want   string
	}{
		{409, map[string]any{"error": `not controllable: the sessionhub watcher on machine "tower" is offline (no poll in the last 2m0s)`},
			`not controllable: the sessionhub watcher on machine "tower" is offline (no poll in the last 2m0s)`},
		// Markup in a refusal is text: the card puts it in with textContent.
		{429, map[string]any{"error": "<img src=x onerror=alert(1)>"}, "<img src=x onerror=alert(1)>"},
		{502, nil, "The server returned 502."},
		{500, map[string]any{"error": 5}, "The server returned 500."},
	}
	in := map[string]any{"buttons": []any{}, "outcomes": []any{}, "refusals": []any{}}
	for _, b := range buttons {
		in["buttons"] = append(in["buttons"].([]any), b.s)
	}
	for _, o := range outcomes {
		in["outcomes"] = append(in["outcomes"].([]any), o.req)
	}
	for _, r := range refusals {
		in["refusals"] = append(in["refusals"].([]any), []any{r.status, r.body})
	}
	input, _ := json.Marshal(in)
	script := string(m[1]) + `
var input = JSON.parse(require("fs").readFileSync(0, "utf8"));
process.stdout.write(JSON.stringify({
  buttons: input.buttons.map(rcButton),
  outcomes: input.outcomes.map(rcOutcome),
  refusals: input.refusals.map(function (r) { return rcRefusal(r[0], r[1]); })
}));`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got struct {
		Buttons  []*button  `json:"buttons"`
		Outcomes []*outcome `json:"outcomes"`
		Refusals []string   `json:"refusals"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node output %s: %v", out, err)
	}
	failures, n := 0, 0
	for i, b := range buttons {
		n++
		if !reflect.DeepEqual(got.Buttons[i], b.want) {
			failures++
			t.Errorf("rcButton(%v) = %+v, want %+v", b.s, got.Buttons[i], b.want)
		}
	}
	for i, o := range outcomes {
		n++
		if !reflect.DeepEqual(got.Outcomes[i], o.want) {
			failures++
			t.Errorf("rcOutcome(%v) = %+v, want %+v", o.req, got.Outcomes[i], o.want)
		}
	}
	for i, r := range refusals {
		n++
		if got.Refusals[i] != r.want {
			failures++
			t.Errorf("rcRefusal(%d, %v) = %q, want %q", r.status, r.body, got.Refusals[i], r.want)
		}
	}
	t.Logf("dashboard states: %d cases, %d failures", n, failures)
}

// The page's one write is the Remote Control request, with its header.
func TestDashboardSendsOneKindOfWrite(t *testing.T) {
	page := string(dashboardHTML)
	for _, c := range []struct {
		s    string
		want int
	}{
		{"method:", 1},
		{`method: "POST"`, 1},
		{`"X-Hub-Action": "remote-control"`, 1},
		{`"/remote-control"`, 1},
		{"fetch(", 3}, // the list, the request, and the session poll
		{"// rc-state:begin", 1},
		{"// rc-state:end", 1},
	} {
		if got := strings.Count(page, c.s); got != c.want {
			t.Errorf("page has %q %d times, want %d", c.s, got, c.want)
		}
	}
	// The link opens in a new tab without the page as its opener.
	for _, want := range []string{`a.target = "_blank"`, `a.rel = "noopener noreferrer"`, "RC_URL.test("} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}
```

The existing `TestDashboardHTMLAndCSP` keeps checking the rest: one inline script, CSP hashes, no `https://` or ` href=` in the page, and no `innerHTML`, `insertAdjacentHTML`, or `document.write`.

Run: `GOFLAGS=-count=1 go test ./internal/server/ -run 'TestDashboard'`
Expected: FAIL: `dashboard/index.html has no rc-state block` (or, without node, a skip plus the `TestDashboardSendsOneKindOfWrite` failures for `method:` and the markers).

- [ ] **Step 2: Add the styles**

In `internal/server/dashboard/index.html`, add before `</style>`:

```css
.rc { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; margin-top: 10px; }
.rc .note { color: var(--muted); font-size: .85rem; }
.rc .rc-error { color: var(--blocked); font-size: .85rem; }
.rc-link { width: 100%; }
.rc-link .cmd { margin-top: 4px; }
a.open {
  display: inline-flex; align-items: center; min-height: 44px; padding: 0 12px; border-radius: 6px;
  background: var(--accent); color: var(--card); font-size: .85rem; font-weight: 600;
  text-decoration: none; white-space: nowrap;
}
a.open:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
button:disabled { color: var(--muted); border-color: var(--line); cursor: default; }
```

- [ ] **Step 3: Add the state and the pure functions**

In the script, after `var openEnded = {};`:

```js
  var RC_POLL_MS = 2000;
  var RC_POLL_FOR_MS = 120000;
  var rcState = {};      // session id -> {phase: "sending"|"waiting"|"error", req, error, until}
  var cardEls = {};      // session id -> its card element, for in-place updates
  var sessionsById = {}; // session id -> the latest session read

  // rc-state:begin
  // The only link the page opens. The server checks results against the same rule.
  var RC_URL = /^https:\/\/claude\.ai\/code\/session_[A-Za-z0-9_-]+$/;

  // rcButton is the Remote Control button a card shows, or null for none.
  function rcButton(s) {
    if (!s.controllable) return null;
    if (s.status === "blocked") return { label: "Remote Control", disabled: true, note: "waiting on a prompt in the pane" };
    if (s.status === "live") return { label: "Remote Control", disabled: false, note: "" };
    return { label: "Resume with Remote Control", disabled: false, note: "" };
  }

  // rcOutcome is what a card says about a request, or null.
  function rcOutcome(req) {
    if (!req) return null;
    switch (req.state) {
      case "pending":
      case "claimed":
        return { kind: "sending", text: "Sending…", url: "" };
      case "done":
        if (typeof req.url === "string" && RC_URL.test(req.url)) return { kind: "link", text: req.detail || "", url: req.url };
        return { kind: "note", text: req.detail || "Sent, but no link appeared.", url: "" };
      case "failed":
        return { kind: "error", text: req.detail || "The machine could not turn on Remote Control.", url: "" };
      case "expired":
        return { kind: "error", text: "The machine didn't respond in time.", url: "" };
    }
    return null;
  }

  // rcRefusal is the text for a refused request: the server's error, or its status.
  function rcRefusal(status, body) {
    if (body && typeof body.error === "string" && body.error) return body.error;
    return "The server returned " + status + ".";
  }
  // rc-state:end
```

- [ ] **Step 4: Render the row, send the request, and poll**

Add after the `copy` function:

```js
  function rcLink(s, url, label) {
    var box = el("div", "rc-link");
    if (label) box.appendChild(el("div", "meta", label));
    var row = el("div", "cmd");
    var a = el("a", "open", "Open in Claude");
    a.href = url;
    a.target = "_blank";
    a.rel = "noopener noreferrer";
    var code = el("code", null, url);
    var btn = el("button", null, "Copy");
    btn.type = "button";
    btn.setAttribute("aria-label", "Copy Remote Control link for " + (s.title || s.id));
    btn.addEventListener("click", function () { copy(url, btn, code); });
    row.appendChild(a);
    row.appendChild(code);
    row.appendChild(btn);
    box.appendChild(row);
    return box;
  }

  // rcRow is the card's Remote Control row: the button, what the last tap
  // did, and the last link until the session ends. null when there is none.
  function rcRow(s, now) {
    var st = rcState[s.id];
    var out = null;
    if (st && st.phase === "sending") out = { kind: "sending", text: "Sending…", url: "" };
    else if (st && st.phase === "error") out = { kind: "error", text: st.error, url: "" };
    else if (st && st.req) out = rcOutcome(st.req);
    else if (s.remote_control && (s.remote_control.state === "pending" || s.remote_control.state === "claimed")) out = rcOutcome(s.remote_control);

    var box = el("div", "rc");
    var b = rcButton(s);
    if (b) {
      var btn = el("button", null, b.label);
      btn.type = "button";
      btn.disabled = b.disabled || (out !== null && out.kind === "sending");
      btn.setAttribute("aria-label", b.label + " for " + (s.title || s.id));
      btn.addEventListener("click", function () { requestRC(s.id); });
      box.appendChild(btn);
      if (b.note) box.appendChild(el("span", "note", b.note));
    }
    if (out && out.text) box.appendChild(el("span", out.kind === "error" ? "rc-error" : "note", out.text));
    var url = out && out.kind === "link" ? out.url : s.remote_control_url;
    if (url && RC_URL.test(url)) {
      var label = "Remote Control link" + (s.remote_control_at ? ", " + age(s.remote_control_at, now) : "");
      box.appendChild(rcLink(s, url, label));
    }
    return box.childNodes.length ? box : null;
  }

  function refreshCard(id) {
    var old = cardEls[id];
    var s = sessionsById[id];
    if (old && s && old.parentNode) old.replaceWith(card(s, Date.now()));
  }

  // requestRC sends the page's one write request.
  function requestRC(id) {
    rcState[id] = { phase: "sending" };
    refreshCard(id);
    fetch("/v1/sessions/" + encodeURIComponent(id) + "/remote-control", {
      method: "POST",
      headers: { "Accept": "application/json", "X-Hub-Action": "remote-control" },
      cache: "no-store"
    })
      .then(function (resp) {
        return resp.json().then(
          function (body) { return { resp: resp, body: body }; },
          function () { return { resp: resp, body: null }; });
      })
      .then(function (r) {
        if (!r.resp.ok || !r.body || typeof r.body.id !== "string") {
          rcState[id] = { phase: "error", error: rcRefusal(r.resp.status, r.body) };
          refreshCard(id);
          return;
        }
        rcState[id] = { phase: "waiting", req: r.body, until: Date.now() + RC_POLL_FOR_MS };
        refreshCard(id);
        setTimeout(function () { pollRC(id, r.body.id); }, RC_POLL_MS);
      })
      .catch(function () {
        rcState[id] = { phase: "error", error: "Could not reach the sessionhub." };
        refreshCard(id);
      });
  }

  // pollRC reads the session every 2 s until the request ends, for up to
  // 2 minutes.
  function pollRC(id, reqID) {
    var st = rcState[id];
    if (!st || !st.req || st.req.id !== reqID) return; // a newer tap took over
    if (Date.now() > st.until) {
      st.req = { id: reqID, state: "expired" };
      refreshCard(id);
      return;
    }
    function again() { setTimeout(function () { pollRC(id, reqID); }, RC_POLL_MS); }
    fetch("/v1/sessions/" + encodeURIComponent(id), { headers: { "Accept": "application/json" }, cache: "no-store" })
      .then(function (resp) {
        if (!resp.ok) throw new Error("status " + resp.status);
        return resp.json();
      })
      .then(function (d) {
        sessionsById[id] = d;
        var req = d.remote_control;
        if (req && req.id === reqID) st.req = req;
        refreshCard(id);
        if (!req || req.id !== reqID || req.state === "pending" || req.state === "claimed") again();
      })
      .catch(again);
  }
```

In `card`, after the `if (s.resume_command) {...}` block and before `return c;`:

```js
    var rc = rcRow(s, now);
    if (rc) c.appendChild(rc);
    cardEls[s.id] = c;
```

In `render`, first lines:

```js
    cardEls = {};
    sessionsById = {};
    sessions.forEach(function (s) { sessionsById[s.id] = s; });
```

- [ ] **Step 5: Run the dashboard tests**

Run: `GOFLAGS=-count=1 go test ./internal/server/ -run 'TestDashboard|TestAuthMatrix' -v 2>&1 | grep -E '^(--- |ok|FAIL|.*dashboard states|.*auth matrix)'`
Expected: every test `--- PASS`, including `TestDashboardHTMLAndCSP` (the CSP hashes follow the edited script), and the log line `dashboard states: 25 cases, 0 failures`. If `node` is missing, `TestDashboardRemoteControlStates` shows `--- SKIP`: install node and run it once before you commit, and paste its output into the evidence in Task 7.

Then run: `make test lint`
Expected: every package `ok`; `lint` prints nothing.

- [ ] **Step 6: Check the page at phone width**

Build and run a local server with a seeded controllable session, then render the page in the headless shell at 360 px:

```bash
make build
d=$(mktemp -d)
export SESSIONHUB_SERVER_CONFIG=$d/none.toml SESSIONHUB_DB=$d/sessionhub.db SESSIONHUB_READ_TOKEN=hub_r_evidence-read-token-0123456789abcdef SESSIONHUB_LISTEN=127.0.0.1:18787
tok=$(./bin/sessionhub machine add tower --json | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')
./bin/sessionhub server & srv=$!
sleep 1
curl -fsS -X POST -H "Authorization: Bearer $tok" -d '{"id":"0a1b2c3d-1111-4222-8333-444455556666","agent":"claude","source":"plugin","herdr_session":"default","herdr_pane":"w1:p1","title_hint":"rc layout check"}' http://127.0.0.1:18787/v1/sessions
curl -fsS -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $tok" 'http://127.0.0.1:18787/v1/machines/self/control?wait=1'
shell=$(ls -d ~/.cache/ms-playwright/chromium_headless_shell-*/*/chrome-headless-shell | tail -1)
"$shell" --window-size=360,900 --virtual-time-budget=5000 --screenshot=/tmp/sessionhub-rc-360.png "http://127.0.0.1:18787/?token=$SESSIONHUB_READ_TOKEN"
kill $srv
```

Expected: the session POST prints the session JSON; the poll prints `204` (it records tower's poll, so the card is controllable); the screenshot shows the card with a **Remote Control** button, no horizontal overflow, and the button at least 44 px tall. `SESSIONHUB_SERVER_CONFIG` points at a missing file, so the machine's own `server.toml`, if any, is not read. Keep the PNG out of the repo; Task 7 records the live one.

- [ ] **Step 7: Commit**

```bash
git add internal/server/dashboard/index.html internal/server/dashboard_test.go
git commit -F - <<'MSG'
Add the Remote Control button to the dashboard

A controllable card shows Remote Control (live), Resume with Remote
Control (ended or stale), or a disabled button with "waiting on a prompt
in the pane" (blocked). A tap sends the page's only write, with
X-Hub-Action: remote-control, shows Sending..., and polls the session
every 2 s for up to 2 minutes. Done shows Open in Claude and a copy
button; failed shows the machine's reason; no answer shows "The machine
didn't respond in time." The last link and its age stay until the session
ends.

Every string still goes in with textContent, the only link is checked
against the same claude.ai pattern as the server, and the CSP hashes
follow the page. The card logic sits in an rc-state block that a test runs
under node, when node is on PATH.

Docs: Task 7 updates docs/server.md (Dashboard).

Refs: docs/dev/superpowers/plans/2026-09-30-remote-control-button.md Task 6

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
MSG
```

---
### Task 7: Docs, deploy, and the live end-to-end check

**Files:**
- Modify: `docs/dev/SPEC.md` (component 1 endpoints and dashboard; component 3; component 5)
- Modify: `docs/dev/PLAN.md` (HTTP API table; Data model changes; Watcher; Remote Control)
- Modify: `docs/server.md` (Authentication; Endpoints; Dashboard; Events; new "Remote Control requests"; Field validation; Database)
- Modify: `docs/plugin.md` (Watcher: new "Control loop"; Troubleshooting)
- Modify: `docs/client.md` (`internal/client`; `internal/herdr`; `internal/herdr/herdrtest`)
- Modify: `docs/cli.md` (Remote Control table, "Another machine" row)
- Modify: `README.md` (Use it; Troubleshoot)
- Modify: `docs/dev/IDEAS.md`
- Create: `docs/dev/evidence/remote-control-button-tap.mjs`
- Create: `docs/dev/evidence/remote-control-button.md`

**Interfaces:**
- Consumes: everything from Tasks 1–6, as built. Where the code differs from this plan, the docs follow the code, and `docs/dev/PLAN.md` says why.
- Produces: documentation and evidence only.

- [ ] **Step 1: Update `docs/dev/SPEC.md`**

In component 1, add to the endpoint list:

```markdown
  - `POST /v1/sessions/{id}/remote-control` — ask the session's machine to turn on Claude Code Remote Control (dashboard cookie with `X-Hub-Action: remote-control`, or a machine token)
  - `GET /v1/machines/self/control?wait=30` — the plugin watcher's long poll for those requests
  - `POST /v1/control/{request-id}/result` — the watcher's answer
```

Replace the dashboard bullet's "Read-only." with: "Read-only, with one exception: each session in herdr has a **Remote Control** button that turns on Claude Code Remote Control, or resumes the session with it on, and shows an **Open in Claude** link. The design is in `docs/dev/superpowers/specs/2026-09-30-remote-control-button-design.md`."

In component 3, add: "- The watcher also keeps a long poll open for Remote Control requests and runs them in the local herdr (inject `/remote-control`, or resume in a new workspace)."

In component 5, append to the `sessionhub remote-control` bullet: "For a session on another machine it asks that machine's watcher through the sessionhub, and prints the SSH command when the sessionhub can't reach it."

- [ ] **Step 2: Update `docs/dev/PLAN.md`**

Add to the HTTP API table, before `GET /healthz`:

```markdown
| `POST /v1/sessions/{id}/remote-control` | dashboard (cookie + `X-Hub-Action`), CLI | Open a Remote Control request for the session's machine. `202` new, `200` the open one, `409` not in herdr or watcher offline, `429` at 10 pending. |
| `GET /v1/machines/self/control?wait=30` | plugin watcher | Long poll: claims the machine's oldest pending request (`200`), or `204` after `wait` seconds (1–30). At most 2 open per machine. |
| `POST /v1/control/{request-id}/result` | plugin watcher | `{"state":"done"\|"failed","url","detail"}` from the claiming machine. |
```

Add to "Data model changes":

```markdown
- Schema v3 (the Remote Control button): `control_requests` (`id`, `session_id`,
  `machine_id`, `action`, `state`, `requested_by`, `created_at`, `expires_at`,
  `claimed_at`, `finished_at`, `url`, `detail`), `machines.last_poll`, and the
  session's last link in `sessions.rc_url` and `rc_at`, cleared when the
  session ends. Expiry is computed on read and stored on the next write.
```

Add to the Watcher bullets:

```markdown
- Beside the 2 s ticks, a control loop holds `GET /v1/machines/self/control`
  open and runs each claimed request through `resume.Control` (see
  `docs/plugin.md`, "Control loop").
```

Add to the "Remote Control" section: "The dashboard button and the remote branch of `sessionhub remote-control` go through control requests: the sessionhub stores them, the session's watcher claims and runs them with `resume.Control`, and the dashboard or CLI reads the result from the session. The watcher resumes a session no pane runs in a new workspace; `sessionhub remote-control` on the session's own machine still never does."

Add a line under any section where the build differed from this plan, with the reason (for example, if the merged fix round changed the injection steps that Task 3 moved).

- [ ] **Step 3: Update `docs/server.md`**

Under "Authentication", after the cookie paragraph:

```markdown
One write accepts the cookie: `POST /v1/sessions/{id}/remote-control`, and
only with the header `X-Hub-Action: remote-control`. The header makes a
cross-site request need a CORS preflight, which the server never grants, and
the cookie is `SameSite=Strict`. The same route accepts a machine token. The
bearer read token, and the cookie without the header, get `403`.
```

Add to the Endpoints table:

```markdown
| `POST /v1/sessions/{id}/remote-control` | machine, or cookie with `X-Hub-Action` | `202` new, `200` open one; a `ControlRequest` | Ask the session's machine to turn on Remote Control. See [Remote Control requests](#remote-control-requests). |
| `GET /v1/machines/self/control?wait=30` | machine | `200` a `ControlClaim`; `204` none | The watcher's long poll. |
| `POST /v1/control/{id}/result` | machine (the claiming one) | `200`, the `ControlRequest` | The watcher's result (`ControlResultIn`). |
```

In "Dashboard", replace "`GET /` is a read-only page" with "`GET /` is a page" and add after its first paragraph:

```markdown
A session in herdr whose machine's watcher polled in the last 2 minutes
(`controllable`) gets a button: **Remote Control** when it is live,
**Resume with Remote Control** when it is ended or stale, and a disabled
button with "waiting on a prompt in the pane" when it is blocked. A tap is
the page's only write request. The card shows **Sending…**, reads the session
every 2 seconds for up to 2 minutes, and then shows **Open in Claude** with a
copy button, the machine's reason, or "The machine didn't respond in time."
The last link and its age stay on the card until the session ends. The link
must match `https://claude.ai/code/session_<id>`; the page opens nothing
else.
```

In "Events", add: "`remote_control_requested` (source `server`) and `remote_control_result` (source `plugin`) are recorded by the server only; their payloads carry `request_id` and, for a result, `state`, `url`, and `detail`."

Add a section before "Query parameters and IDs":

```markdown
### Remote Control requests

A request asks the session's machine to turn on Claude Code Remote Control.
Its `id` is `cr_` plus 16 random bytes in base64url, its only `action` is
`remote_control`, and `requested_by` is `dashboard` or `machine:<name>`.

| State | Meaning |
|---|---|
| `pending` | Created, not yet claimed. |
| `claimed` | The machine's watcher took it and is working on it. |
| `done` | The watcher sent `/remote-control`, or resumed the session. `url` is the link when it saw one. |
| `failed` | `detail` says why: "waiting on a prompt in the pane", "looks live but pane not found", a missing directory, or a herdr error. |
| `expired` | Two minutes passed since creation while it was pending or claimed. It is never retried. |

- `POST /v1/sessions/{id}/remote-control` answers `404` for an unknown
  session, `409` when the session has no herdr pane or its machine's
  watcher hasn't polled in 2 minutes, `200` with the session's open request
  if it has one, `429` when the machine already has 10 pending requests, and
  `202` with a new request, in that order.
- `GET /v1/machines/self/control?wait=N` records the machine's poll time
  (which drives `controllable`), then claims its oldest pending request in
  one transaction and answers `200` with `{"request": ..., "session": ...}`.
  With nothing pending it waits up to `N` seconds (clamped to 1–30, default
  30), and a new request wakes it at once. It then answers `204` with no
  body. A machine may hold 2 polls open; a third gets `429`. On shutdown the
  server answers open polls with `204`, and the watchers reconnect; pending
  requests are in the database.
- `POST /v1/control/{id}/result` takes `{"state": "done"|"failed", "url":
  "...", "detail": "..."}` from the machine that claimed the request. `url`
  must match `^https://claude\.ai/code/session_[A-Za-z0-9_-]+$` and only on
  `done`; `detail` follows the free-text rules, at most 200 characters. A
  request that isn't `claimed`, including one that expired while the
  machine worked on it, gets `409`. A `done` result with a `url` sets the
  session's `remote_control_url` and `remote_control_at`.
- Every session carries `remote_control` (its latest request),
  `remote_control_url` and `remote_control_at` (cleared by `pane_closed`, by
  `ended`, and when a snapshot ends it), and `controllable`.
- A request expires on read: the state reads `expired` once `expires_at`
  passes, and the next write stores it.
```

In "Field validation", add rows:

```markdown
| Result `url` | `^https://claude\.ai/code/session_[A-Za-z0-9_-]+$`, on `done` only. |
| Result `detail` | At most 200 characters, no control characters. |
```

In "Database", change "(version 2 adds `sessions.state_ts`)" to "(version 2 adds `sessions.state_ts`; version 3 adds `control_requests`, `machines.last_poll`, and `sessions.rc_url` and `rc_at`)".

- [ ] **Step 4: Update `docs/plugin.md`, `docs/client.md`, `docs/cli.md`, the README, and `docs/dev/IDEAS.md`**

`docs/plugin.md`, add at the end of "Watcher":

```markdown
### Control loop

Beside the 2-second ticks, the watcher keeps one long poll open on
`GET /v1/machines/self/control?wait=30`. It never takes the queue lock.

- For each claimed request it calls `resume.Control` with the herdr socket
  and posts the result:
  - **herdr detects Claude running the session** (in its recorded pane, or
    any pane in a fresh snapshot): send `/remote-control` with
    `agent.prompt`, read the pane for up to 15 seconds for a link that wasn't
    there before, and report `done` with it, or `done` with "sent, link not
    seen". If Remote Control was already on, Claude shows a dialog; the
    watcher takes the link from it and closes it with **Esc**, unless
    Claude is working (Esc would interrupt the turn).
  - **`agent_blocked`:** `failed`, "waiting on a prompt in the pane". Nothing
    is typed.
  - **No pane runs it, and the sessionhub says it ended or is stale:** create a
    workspace in the session's directory without focus, labeled
    `sessionhub: <title>`, start `claude --resume <id> --remote-control` there with
    `agent.start`, and wait for the link the same way. A missing directory
    is `failed`, and nothing is created.
  - **No pane runs it, but the sessionhub says it is live or blocked:** `failed`,
    "looks live but pane not found". It never starts a second copy.
- After a network error the loop waits 5 seconds, doubling to 60. After
  `401` or `403` it logs and waits 5 minutes. A `204` that came back in under
  a second waits 1 second, so a server that is shutting down can't spin the
  loop.
- A result the server refuses (the request expired meanwhile) is logged, not
  retried. The dashboard then says the machine didn't respond in time.
- Log lines start with `control:`. `control: waiting for Remote Control
  requests` means the loop runs.
```

In "Troubleshooting", add: "- If the dashboard shows no **Remote Control** button for a session in herdr, the machine's watcher isn't polling. Look for `control:` lines in `watcher.log`; a `401` means the token is wrong."

`docs/client.md`, `internal/client`: add "`PollControl(ctx, wait)` holds the control long poll open (timeout `wait` + 10 s) and returns `nil` on `204`; `PostControlResult` and `CreateControl` are ordinary calls. Every call's time limit is a context deadline (2 s, or the poll's own), not `http.Client.Timeout`." In `internal/herdr`, add `WorkspaceCreate(WorkspaceCreateParams)` (`workspace.create` with `cwd`, `label`, `focus`; captured in `workspace-create.ndjson`). In `internal/herdr/herdrtest`, add: "`Handle(method, fn)` answers one method from a Go function instead of the fixtures, for a request that must get different answers over time."

`docs/cli.md`, in the Remote Control table, replace the "Another machine" row with:

```markdown
| Another machine | Any | Posts a control request through the sessionhub, then reads the session every 2 s for up to 2 minutes. Prints the link (exit 0), says that no link appeared (exit 0), or exits 1 with the machine's reason. If the sessionhub refuses the request (`409`: not in herdr, or the machine's watcher is offline), can't be reached, or the request expires, prints `ssh -t <ssh_host> '~/.local/bin/sessionhub remote-control <uuid>'`, and `herdr --machine <label> agent prompt <pane_id> /remote-control` when a saved herdr machine matches. Never runs either. |
```

`README.md`, "Use it": change the `sessionhub remote-control` bullet's last sentence to "For a session on another machine it asks that machine through the sessionhub, and prints the `ssh` command when that fails." Add: "- On the dashboard, **Remote Control** turns on Claude Code Remote Control for a session in herdr, and **Resume with Remote Control** restarts an ended one with it on, in a new herdr workspace on its machine. Tap **Open in Claude** to continue in the Claude app." In "Troubleshoot", add: "- **The dashboard shows no Remote Control button.** The session's machine isn't polling: run `sessionhub install-plugin` there and look for `control:` lines in `~/.local/state/sessionhub/watcher.log`."

`docs/dev/IDEAS.md`, add:

```markdown
- Retry a Remote Control result the watcher couldn't post. Today the request
  expires and the dashboard says the machine didn't respond.
- Remote Control for sessions outside herdr (hooks-only).
```

Run: `make test lint`
Expected: every package `ok`; `lint` prints nothing (docs don't affect it, but the branch must stay green).

Commit:

```bash
git add docs/dev/SPEC.md docs/dev/PLAN.md README.md docs/dev/IDEAS.md docs/server.md docs/plugin.md docs/client.md docs/cli.md
git commit -F - <<'MSG'
Document the Remote Control button

docs/dev/SPEC.md and docs/dev/PLAN.md record the one write the dashboard may now send and
the three control endpoints. docs/server.md describes the request states,
the order of the create checks, the long poll, and schema v3;
docs/plugin.md the watcher's control loop; docs/cli.md the remote branch
of sessionhub remote-control; docs/client.md the new client and herdr calls.

Refs: docs/dev/superpowers/plans/2026-09-30-remote-control-button.md Task 7

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
MSG
```

- [ ] **Step 5: Write the tap script**

Create `docs/dev/evidence/remote-control-button-tap.mjs`. It drives the real dashboard in the headless shell through the DevTools protocol, with no dependencies (Node 22 or later has `fetch` and `WebSocket`):

```js
// Taps a session's Remote Control button on the real dashboard in headless
// Chrome and prints what the card says before and after, with Remote
// Control session IDs masked. The screenshot masks the link text too.
//
//   SESSIONHUB_READ_TOKEN=... HEADLESS_SHELL=/path/to/chrome-headless-shell \
//     node docs/dev/evidence/remote-control-button-tap.mjs '<card title>' <screenshot.png>
//
// The token is read from the environment and never printed.
import { spawn } from "node:child_process";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const [title, shot] = process.argv.slice(2);
const token = process.env.SESSIONHUB_READ_TOKEN;
const shell = process.env.HEADLESS_SHELL;
const base = process.env.SESSIONHUB_URL || "https://sessionhub.example.com";
if (!title || !shot || !token || !shell) {
  console.error("usage: SESSIONHUB_READ_TOKEN=... HEADLESS_SHELL=... node remote-control-button-tap.mjs '<card title>' <screenshot.png>");
  process.exit(2);
}
const mask = (s) => s.replace(/session_[A-Za-z0-9_-]+/g, "session_<masked>");
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const port = 9333;
const profile = mkdtempSync(join(tmpdir(), "sessionhub-tap-"));
const chrome = spawn(shell, [`--remote-debugging-port=${port}`, `--user-data-dir=${profile}`, "--window-size=360,1600", "about:blank"], { stdio: "ignore" });
try {
  let pages = [];
  for (let i = 0; i < 100 && !pages.length; i++) {
    try {
      pages = (await (await fetch(`http://127.0.0.1:${port}/json`)).json()).filter((t) => t.type === "page");
    } catch {
      await sleep(100);
    }
  }
  const ws = new WebSocket(pages[0].webSocketDebuggerUrl);
  await new Promise((r) => ws.addEventListener("open", r, { once: true }));
  let seq = 0;
  const waiting = new Map();
  ws.addEventListener("message", (m) => {
    const d = JSON.parse(m.data);
    const f = waiting.get(d.id);
    if (f) { waiting.delete(d.id); f(d); }
  });
  const send = (method, params = {}) => new Promise((r) => { const id = ++seq; waiting.set(id, r); ws.send(JSON.stringify({ id, method, params })); });
  const evaluate = async (expr) => (await send("Runtime.evaluate", { expression: expr, returnByValue: true })).result.result.value;
  const card = `[...document.querySelectorAll("article.card")].find((c) => c.querySelector(".title").textContent === ${JSON.stringify(title)})`;
  const cardText = `(() => { document.querySelectorAll("details.ended").forEach((d) => { d.open = true; }); const c = ${card}; return c ? c.textContent : ""; })()`;

  await send("Page.navigate", { url: `${base}/?token=${encodeURIComponent(token)}` });
  let before = "";
  for (let i = 0; i < 50 && !before; i++) { await sleep(200); before = await evaluate(cardText); }
  if (!before) throw new Error(`no card titled ${title}`);
  console.log("before:", mask(before));
  console.log(await evaluate(`(() => { const c = ${card};
    const b = [...c.querySelectorAll("button")].find((x) => /Remote Control/.test(x.textContent));
    if (!b) return "no Remote Control button";
    if (b.disabled) return "button disabled";
    b.click();
    return "tapped: " + b.textContent; })()`));
  let after = "";
  for (let i = 0; i < 130; i++) {
    await sleep(1000);
    after = await evaluate(cardText);
    if (!after.includes("Sending…")) break;
  }
  console.log("after:", mask(after));
  await evaluate(`document.querySelectorAll(".rc code").forEach((c) => { c.textContent = c.textContent.replace(/session_[A-Za-z0-9_-]+/, "session_<masked>"); })`);
  const png = await send("Page.captureScreenshot", { format: "png", captureBeyondViewport: true });
  writeFileSync(shot, Buffer.from(png.result.data, "base64"));
} finally {
  chrome.kill();
}
```

Check it parses: `node --check docs/dev/evidence/remote-control-button-tap.mjs` (prints nothing).

- [ ] **Step 6: Deploy to `tower` and update `bluebox`**

```bash
make test lint
make deploy
make install && sessionhub install-plugin
grep -c 'control: waiting for Remote Control requests' ~/.local/state/sessionhub/watcher.log
ssh tower 'grep -c "control: waiting for Remote Control requests" ~/.local/state/sessionhub/watcher.log'
```

Expected: `make deploy` ends with `sessionhub version` printing this branch's `git describe`; both `grep -c` print at least `1`. Then check that both machines count as polling (the machine token can read):

```bash
TOKEN=$(grep '^token' ~/.config/sessionhub/config.toml | cut -d'"' -f2)
curl -fsS -H "Authorization: Bearer $TOKEN" 'https://sessionhub.example.com/v1/sessions?live=true' \
  | python3 -c 'import json,sys; [print(s["machine"], s["id"][:8], s["status"], s["controllable"]) for s in json.load(sys.stdin)]'
```

Expected: every session with a herdr pane on `bluebox` and `tower` shows `True`. Record both outputs in the evidence.

- [ ] **Step 7: Inject path on `bluebox`, in a scratch workspace**

```bash
mkdir -p /tmp/sessionhub-rc-button
herdr workspace create --no-focus --label sessionhub-rc-button --cwd /tmp/sessionhub-rc-button   # note the workspace (wX) and root pane (wX:p1)
herdr agent start sessionhub-rc-btn-bluebox --kind claude --pane wX:p1
herdr agent prompt wX:p1 'Reply with the single word ok.'
sessionhub ls --machine bluebox                                                                # note the scratch session's ID
curl -fsS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"title":"rc-button-bluebox"}' "https://sessionhub.example.com/v1/sessions/<full scratch ID>/title" >/dev/null
export SESSIONHUB_READ_TOKEN=$(ssh tower 'grep ^read_token ~/.config/sessionhub/server.toml' | cut -d'"' -f2)
export HEADLESS_SHELL=$(ls -d ~/.cache/ms-playwright/chromium_headless_shell-*/*/chrome-headless-shell | tail -1)
node docs/dev/evidence/remote-control-button-tap.mjs rc-button-bluebox docs/dev/evidence/remote-control-button-bluebox.png
herdr pane read wX:p1 | tail -20 | sed 's/session_[A-Za-z0-9_-]*/session_<masked>/g'
sessionhub show <scratch ID prefix> | head -30 | sed 's/session_[A-Za-z0-9_-]*/session_<masked>/g'
```

Expected: `before:` shows **Remote Control**; the script prints `tapped: Remote Control`; `after:` shows **Open in Claude** and the masked link; the pane shows `/remote-control is active`; `sessionhub show` lists `remote_control_requested` and `remote_control_result` events. Record every command and its output under "Inject on bluebox".

- [ ] **Step 8: Resume path on `tower`, in a scratch workspace**

```bash
ssh tower 'mkdir -p /tmp/sessionhub-rc-button && PATH=$HOME/.local/bin:$PATH herdr api snapshot | python3 -c "import json,sys; print(json.load(sys.stdin)[\"focused_workspace_id\"])"'
ssh tower 'PATH=$HOME/.local/bin:$PATH herdr workspace create --no-focus --label sessionhub-rc-button --cwd /tmp/sessionhub-rc-button'   # note wY, wY:p1
ssh tower 'PATH=$HOME/.local/bin:$PATH herdr agent start sessionhub-rc-btn-tower --kind claude --pane wY:p1'
ssh tower "PATH=\$HOME/.local/bin:\$PATH herdr agent prompt wY:p1 'Reply with the single word ok.'"
sessionhub ls --machine tower                                                                # note the scratch session's ID
ssh tower 'TOKEN=$(grep "^token" ~/.config/sessionhub/config.toml | cut -d\" -f2); curl -fsS -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d "{\"title\":\"rc-button-tower\"}" http://127.0.0.1:8787/v1/sessions/<full scratch ID>/title >/dev/null'   # a title needs the owning machine's token
ssh tower 'PATH=$HOME/.local/bin:$PATH herdr agent prompt wY:p1 /exit'
sessionhub ls --all --machine tower                                                          # repeat until the scratch session shows ended (at most about 60 s)
node docs/dev/evidence/remote-control-button-tap.mjs rc-button-tower docs/dev/evidence/remote-control-button-tower.png
ssh tower 'PATH=$HOME/.local/bin:$PATH herdr workspace list'                          # a new "sessionhub: rc-button-tower" workspace
ssh tower 'PATH=$HOME/.local/bin:$PATH herdr api snapshot | python3 -c "import json,sys; print(json.load(sys.stdin)[\"focused_workspace_id\"])"'
```

If `herdr api snapshot` prints the snapshot inside another object, adjust the Python to that shape and note it. Expected: `before:` shows **Resume with Remote Control**; `after:` shows **Open in Claude**; `herdr workspace list` has a workspace labeled `sessionhub: rc-button-tower`; the focused workspace ID is the same before and after (no focus change). If `after:` says the start failed because the new pane's shell wasn't ready for `agent.start`, record it, and stop: that needs a fix in `resumeInWorkspace` (retry `agent.start` once after a short wait), not a note.

- [ ] **Step 9: The CLI through the sessionhub, from `bluebox`**

```bash
./bin/sessionhub remote-control <tower scratch ID prefix>
echo "exit=$?"
```

Expected: `Asked machine "tower" to turn on Remote Control for session <short ID>. Waiting for it...`, then `Remote Control is on for session <short ID>:`, the link, and `(Remote Control was already on)`, because Step 8 already turned it on and the tower watcher took the link from Claude's dialog; then `exit=0`. Check that the tower pane shows no open dialog afterwards (`ssh tower 'PATH=$HOME/.local/bin:$PATH herdr pane read <new pane ID>'`). Mask the link when you record it.

- [ ] **Step 10: the user's check on the phone**

Ask the user to open `https://sessionhub.example.com/` on his phone and, for the cards **rc-button-bluebox** and **rc-button-tower**:

1. Tap **Open in Claude**, and confirm that the Claude app (or claude.ai) opens that session and shows its last reply ("ok").
2. On **rc-button-bluebox**, tap **Remote Control** again, and confirm the card shows **Open in Claude** again (the already-on dialog path) and that the `bluebox` scratch pane shows no open dialog afterwards (`herdr pane read wX:p1 | tail -15`).

Wait for his answer, and record it in the evidence in his words. Don't mark the task done without it.

- [ ] **Step 11: Clean up the scratch workspaces**

```bash
herdr workspace close wX
ssh tower 'PATH=$HOME/.local/bin:$PATH herdr workspace close wY'
ssh tower 'PATH=$HOME/.local/bin:$PATH herdr workspace list'   # close the "sessionhub: rc-button-tower" workspace by its ID too
ssh tower 'PATH=$HOME/.local/bin:$PATH herdr workspace close <its ID>'
rm -rf /tmp/sessionhub-rc-button /tmp/sessionhub-rc-fixture; ssh tower 'rm -rf /tmp/sessionhub-rc-button'
```

Expected: every close succeeds, and `herdr workspace list` on both machines shows none of the scratch workspaces.

- [ ] **Step 12: Write the evidence and commit**

Create `docs/dev/evidence/remote-control-button.md` with these sections, each holding the commands and outputs recorded above (links and session IDs masked, as in `docs/dev/evidence/remote-control.md`; never the read token):

1. **Summary**: a table of each check (deploy, polling, inject on `bluebox`, resume on `tower`, no focus change, CLI through the sessionhub, phone check, node state table) and its result.
2. **Tests**: `make test` summary (packages `ok`, `go test ./... -count=1 -v 2>&1 | grep -c '^--- PASS'`), `make lint` (empty), and the `auth matrix: 13 routes x 9 credentials, 0 failures` and `dashboard states: 25 cases, 0 failures` log lines from `go test -v -run 'TestAuthMatrix|TestDashboardRemoteControlStates' ./internal/server/`.
3. **Deploy and polling** (Step 6).
4. **Inject on bluebox** (Step 7), with `remote-control-button-bluebox.png`.
5. **Resume on tower** (Step 8), with `remote-control-button-tower.png`.
6. **CLI through the sessionhub** (Step 9).
7. **Phone** (Step 10), in the user's words.
8. **Cleanup** (Step 11).

```bash
git add docs/dev/evidence/remote-control-button.md docs/dev/evidence/remote-control-button-tap.mjs docs/dev/evidence/remote-control-button-*.png
git commit -F - <<'MSG'
Record the Remote Control button end to end

Deployed to tower and bluebox, then tapped the real dashboard in headless
Chrome for a scratch session on bluebox (inject) and on tower (resume in a new
workspace, without focus), ran sessionhub remote-control from bluebox through the
sessionhub, and had the user open both links in the Claude app. The scratch
workspaces are closed. Links and session IDs are masked.

Refs: docs/dev/superpowers/plans/2026-09-30-remote-control-button.md Task 7

Claude-Session: https://claude.ai/code/session_01Pjee46WmLGYzKNnKJBJR6h
MSG
```

---

## Self-review

Checked against the spec with the plan written:

1. **Spec coverage.** Data model and schema v3: Task 1. `api.Session` fields and event kinds: Task 1. The three endpoints, their auth, codes, limits, clamping, in-process wake-up, and lazy expiry: Tasks 1 and 2. Watcher loop, backoff, and every machine-side branch (inject, blocked, resume without focus in the recorded directory with the `sessionhub: <title>` label, missing directory, looks live): Tasks 3 and 4. Shared logic in `internal/resume`: Task 3. CLI remote path and SSH backup: Task 5. Dashboard states, polling, link, copy, messages, last link, CSP, `textContent`, one kind of write: Task 6. Every testing bullet: auth matrix (Task 2), duplicates, expiry, limits (Tasks 1 and 2), long poll with a fake timer (Task 2), URL check (Tasks 1 and 2), watcher decision table against `herdrtest` and the real server plus reconnect and backoff (Task 4), CLI remote and SSH backup (Task 5), dashboard states and no raw HTML (Task 6), live end to end (Task 7). "Server restart drops open polls; watchers reconnect": Task 2 (`StopPolls`) and Task 4 (a fast `204` pauses 1 s, then polls again). "Every request and result is an event, visible in `sessionhub show`": Task 1, checked live in Task 7 Step 7.
2. **Placeholders.** The only deferred content is live output (IDs, workspace names, evidence text), which a step names and says how to get. Task 3 Step 5 names exactly what to copy if the merged CLI code differs, and which unchanged tests prove the copy.
3. **Type consistency.** Checked names across tasks: `CreateControl`/`ClaimControl`/`FinishControl`/`RecordPoll` (store), `CreateControl`/`PollControl`/`PostControlResult` (client), `Control`/`ControlOptions`/`ControlResult`/`Outcome*` (resume), `controlRunner`/`newController`/`resultFor` (plugin), `StopPolls` and `after` (server), JSON field names in Task 6 against Task 1's tags.
4. **Review Focus.** Five lines, each with its test in the owning task. The `agent.start`-right-after-`workspace.create` race is checked live in Task 7 Step 8, with a stop rule if it shows.
5. **Dry run.** The code in Tasks 1–6 was applied to a copy of the CLI branch at `fcc4638`, with a synthetic `workspace-create.ndjson` in place of Task 3's live capture. `make test lint` passed, `go test -race -p 1 ./internal/server/ ./internal/plugin/` passed, the page's script parsed under `node --check`, and Task 6 Step 6's layout check and Task 7's tap script ran against a local server with a scripted watcher (tap, `202`, claim, result, **Open in Claude**). The dry run found and this plan fixes: an ambiguous `sid1[:8]` prefix in the auth matrix (now `sid3`), gofmt alignment in `routes.go` and `TestControlBackoff`, an unused `herdr` import after the move, the SSH fallback printing a line before a refused host (now `remoteControlSSH(ctx, s, why)`), and the move target (the fix round's `fcc4638` visible-screen dialog check and `closeDialog`).
