package plugin

import (
	"context"
	"strings"
	"sync"
	"unicode"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/herdr"
)

// gitFunc looks up a directory's origin URL and branch (gitinfo.Lookup).
type gitFunc func(ctx context.Context, cwd string) (repo, branch string)

// agentState maps herdr's agent_status to a sessionhub agent state. herdr 0.9.3 uses
// the same five values; anything else becomes "unknown".
func agentState(status string) string {
	switch status {
	case "":
		return ""
	case "idle", "working", "blocked", "done", "unknown":
		return status
	}
	return "unknown"
}

// paneUpsert builds the upsert for one snapshot pane, or ok=false when the
// pane runs no live Claude session. A pane qualifies only when herdr has
// detected the agent (pane.agent is "claude") AND agent_session has kind
// "id" and a value (agent_session.agent, when set, must be claude too).
// Other agents are skipped in v1.
//
// A pane that kept its agent_session without a detected agent is at a shell
// prompt, not in Claude: herdr restores agent_session on a restored pane
// (captured: w8:p1, agent_status unknown, shell title). Counting it made the
// heartbeat revive a session every minute after it ended. On a clean /exit,
// herdr 0.9.3 drops both agent and agent_session (checked live, see
// docs/dev/evidence/final-fix-B.md), so that case is skipped by either test.
func paneUpsert(p herdr.PaneInfo, herdrSession string) (api.SessionUpsert, bool) {
	as := p.AgentSession
	if as == nil || as.Kind != "id" || as.Value == "" {
		return api.SessionUpsert{}, false
	}
	if p.Agent != "claude" || (as.Agent != "" && as.Agent != p.Agent) {
		return api.SessionUpsert{}, false
	}
	agent := p.Agent
	cwd := p.CWD
	if cwd == "" {
		cwd = p.ForegroundCWD
	}
	u := api.SessionUpsert{
		ID:             as.Value,
		Agent:          agent,
		Source:         api.SourcePlugin,
		CWD:            foldControl(cwd),
		HerdrSession:   herdrSession,
		HerdrWorkspace: p.WorkspaceID,
		HerdrPane:      p.PaneID,
		AgentState:     agentState(p.AgentStatus),
		TitleHint:      cleanText(p.TitleClean, maxTitleRunes), // the agent is detected, so the title is Claude's
	}
	return u, true
}

// unidentifiedPanes lists the panes where herdr detects Claude but reports
// no session ID, which herdr does for some resumed sessions. The server keeps
// a session that hooks registered in one of them instead of ending it as
// missing from the snapshot.
func unidentifiedPanes(snap herdr.Snapshot) []string {
	var out []string
	for _, p := range snap.Panes {
		if p.Agent != "claude" {
			continue
		}
		if _, ok := paneUpsert(p, ""); !ok {
			out = append(out, p.PaneID)
		}
	}
	return out
}

// maxTitleRunes is the server's cap on title_hint.
const maxTitleRunes = 200

// foldControl replaces every control character (unicode.IsControl, the set
// the server rejects) with a space.
func foldControl(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// cleanText folds control characters to spaces, collapses runs of whitespace,
// and cuts the result to n runes. The server skips an entry with a control
// character in it, so send clean text as defense in depth.
func cleanText(s string, n int) string {
	s = strings.Join(strings.Fields(foldControl(s)), " ")
	if r := []rune(s); len(r) > n {
		s = strings.TrimSpace(string(r[:n]))
	}
	return s
}

// buildSessions returns the upserts for every qualifying pane, with git info
// looked up in parallel (each lookup has its own 500 ms budget). The result
// is never nil, so an empty set marshals as [] and the server ends every
// herdr session of this machine.
func buildSessions(ctx context.Context, snap herdr.Snapshot, herdrSession string, git gitFunc) []api.SessionUpsert {
	out := []api.SessionUpsert{}
	for _, p := range snap.Panes {
		if u, ok := paneUpsert(p, herdrSession); ok {
			out = append(out, u)
		}
	}
	if git != nil {
		var wg sync.WaitGroup
		for i := range out {
			if out[i].CWD == "" {
				continue
			}
			wg.Add(1)
			go func(u *api.SessionUpsert) {
				defer wg.Done()
				u.GitRepo, u.GitBranch = git(ctx, u.CWD)
			}(&out[i])
		}
		wg.Wait()
	}
	return out
}
