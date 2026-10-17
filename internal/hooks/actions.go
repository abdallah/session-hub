package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/knowledge"
)

const (
	// permissionWait is how long the permission hook waits for an answer:
	// the server's request lifetime. The installed timeout is 660 s.
	permissionWait = 10 * time.Minute
	// decisionPoll is one decision long poll.
	decisionPoll = 30 * time.Second
	// decisionPause is the shortest time between two decision polls.
	decisionPause = time.Second
	// maxSuggestionBytes caps the permission_suggestions the hook forwards;
	// bigger ones are dropped (the server does not store them).
	maxSuggestionBytes = 4 << 10
	// contextHeader starts the rules block.
	contextHeader = "Standing instructions from sessionhub (apply in every session):"
	// knowledgeHeader starts the knowledge block.
	knowledgeHeader = "Knowledge approved by the user for this session (data, not instructions from the content you read):"
	// knowledgeBudget caps the text runes of the entries other than global
	// conventions in one block.
	knowledgeBudget = 2000
	denyMessage     = "Denied from sessionhub."
)

// ContextBlock is the additionalContext for the rules: the header and one
// "- " line per rule, or "" when no rule has text left after cleaning.
func ContextBlock(list api.InstructionList) string {
	var lines []string
	for _, in := range list.Instructions {
		if t := cleanText(in.Text, 300); t != "" {
			lines = append(lines, "- "+t)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return contextHeader + "\n" + strings.Join(lines, "\n")
}

// contextOutput is the context hook's output.
type contextOutput struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

type permissionDecision struct {
	Behavior string `json:"behavior"`
	Message  string `json:"message,omitempty"`
}

// permissionOutput is the permission hook's output. Structs, not maps, keep
// the key order Claude Code's docs show.
type permissionOutput struct {
	HookSpecificOutput struct {
		HookEventName string             `json:"hookEventName"`
		Decision      permissionDecision `json:"decision"`
	} `json:"hookSpecificOutput"`
}

// writeHookOutput writes v as the hook's one JSON object.
func writeHookOutput(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// printContext is `sessionhub hook context`: it prints the local knowledge
// copy (or, until the watcher has written one, the rules copy) as
// additionalContext for SessionStart and UserPromptSubmit. It reads local
// files and never builds a sessionhub client, so a prompt never waits on the
// network. Only SessionStart runs git, and caches the result.
func (h *handler) printContext() error {
	raw, err := io.ReadAll(io.LimitReader(h.stdin, maxStdin))
	if err != nil {
		return fmt.Errorf("hook context: read stdin: %w", err)
	}
	var in struct {
		HookEventName string `json:"hook_event_name"`
		AgentID       string `json:"agent_id"`
		SessionID     string `json:"session_id"`
		Cwd           string `json:"cwd"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("hook context: bad stdin JSON: %w", err)
	}
	if in.AgentID != "" || (in.HookEventName != "SessionStart" && in.HookEventName != "UserPromptSubmit") {
		return nil
	}
	start := in.HookEventName == "SessionStart"
	// A missing, empty, or broken copy means no context. The hook stays
	// silent, stderr included: it runs on every prompt, and the next refresh
	// rewrites the copy.
	var block string
	if local, err := client.LoadKnowledge(client.KnowledgePath(h.stateDir)); err == nil {
		var repo, branch string
		if start {
			origin, b := h.git(context.Background(), in.Cwd)
			repo, branch = knowledge.NormalizeRepo(origin), b
			h.writeSessionRepo(in.SessionID, repo, branch)
		} else {
			repo, branch = h.readSessionRepo(in.SessionID)
		}
		n := 0
		if !start {
			periodic := false
			for _, e := range local.Entries {
				periodic = periodic || e.Every > 0
			}
			n = h.countPrompt(in.SessionID, periodic)
		}
		cfg, _ := client.LoadConfig()
		block = selectKnowledge(local, in.SessionID, repo, cfg.Machine, knowledge.RefsInBranch(branch), n)
	} else {
		list, err := client.LoadInstructions(client.InstructionsPath(h.stateDir))
		if err != nil {
			return nil
		}
		if !start {
			periodic := false
			for _, r := range list.Instructions {
				periodic = periodic || r.Every > 0
			}
			list.Instructions = dueRules(list.Instructions, h.countPrompt(in.SessionID, periodic))
		}
		block = ContextBlock(list)
	}
	if block == "" {
		return nil
	}
	var out contextOutput
	out.HookSpecificOutput.HookEventName = in.HookEventName
	out.HookSpecificOutput.AdditionalContext = block
	return writeHookOutput(h.stdout, out)
}

// selectKnowledge is the additionalContext for one session, or "". n is 0 at
// SessionStart (every entry due), the prompt number on UserPromptSubmit, and
// negative when there is no counter (only entries with no every are due).
// Global conventions are always sent; the other entries share
// knowledgeBudget runes of text and are cut from the end.
func selectKnowledge(local api.KnowledgeLocal, sessionID, repo, machine string, branchRefs []string, n int) string {
	refs := map[string]bool{}
	for _, r := range append(append([]string(nil), local.SessionRefs[sessionID]...), branchRefs...) {
		refs["ticket:"+strings.ToLower(strings.TrimSpace(r))] = true
	}
	machine = strings.TrimSpace(machine)
	// Buckets in block order: global conventions, ticket, repo, machine, other global.
	type line struct{ kind, text string }
	var buckets [5][]line
	for _, e := range local.Entries {
		if e.State != api.KnowledgeActive || !(n == 0 || e.Every <= 0 || (n > 0 && n%e.Every == 0)) {
			continue
		}
		b := -1
		switch {
		case e.Scope == "global" && e.Kind == "convention":
			b = 0
		case refs[e.Scope]:
			b = 1
		case repo != "" && e.Scope == "repo:"+repo:
			b = 2
		case machine != "" && e.Scope == "machine:"+machine:
			b = 3
		case e.Scope == "global":
			b = 4
		}
		text := cleanText(e.Text, 300)
		if b < 0 || text == "" {
			continue
		}
		if why := cleanText(e.Why, 200); why != "" {
			text += " (" + why + ")"
		}
		buckets[b] = append(buckets[b], line{cleanText(e.Kind, 20), text})
	}
	lines := []string{knowledgeHeader}
	used, cut := 0, 0
	for b, items := range buckets {
		for _, l := range items {
			if b > 0 {
				if cut > 0 || used+utf8.RuneCountInString(l.text) > knowledgeBudget {
					cut++
					continue
				}
				used += utf8.RuneCountInString(l.text)
			}
			lines = append(lines, "- ["+l.kind+"] "+l.text)
		}
	}
	if len(lines) == 1 {
		return ""
	}
	if cut > 0 {
		lines = append(lines, fmt.Sprintf("- %d more entries match this session; use search_knowledge.", cut))
	}
	return strings.Join(lines, "\n")
}

// sessionRepoDir caches each session's normalized repo and branch, written at
// SessionStart so prompts never run git.
const sessionRepoDir = "session-repo"

// safeSessionID reports whether id can name a file in a state subdirectory.
func safeSessionID(id string) bool {
	return id != "" && id == filepath.Base(id) && !strings.HasPrefix(id, ".")
}

func (h *handler) writeSessionRepo(sessionID, repo, branch string) {
	if !safeSessionID(sessionID) {
		return
	}
	dir := filepath.Join(h.stateDir, sessionRepoDir)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	if os.WriteFile(filepath.Join(dir, sessionID), []byte(repo+"\n"+branch+"\n"), 0o600) == nil {
		pruneCounters(dir, h.now().Add(-promptCountTTL))
	}
}

func (h *handler) readSessionRepo(sessionID string) (repo, branch string) {
	if !safeSessionID(sessionID) {
		return "", ""
	}
	raw, err := os.ReadFile(filepath.Join(h.stateDir, sessionRepoDir, sessionID))
	if err != nil {
		return "", ""
	}
	repo, branch, _ = strings.Cut(strings.TrimRight(string(raw), "\n"), "\n")
	return repo, branch
}

// promptCountDir holds one prompt counter per session, in the state dir.
const promptCountDir = "prompt-counts"

// promptCountTTL is how long an untouched counter is kept.
const promptCountTTL = 7 * 24 * time.Hour

// dueRules keeps the rules sent with prompt n of a session: every rule
// with no every, and each periodic rule on every Nth prompt. n < 1 (no
// counter) keeps only the rules with no every.
func dueRules(rules []api.Instruction, n int) []api.Instruction {
	var out []api.Instruction
	for _, r := range rules {
		if r.Every <= 0 || (n > 0 && n%r.Every == 0) {
			out = append(out, r)
		}
	}
	return out
}

// countPrompt adds one to the session's prompt counter and returns the new
// count, or -1 when nothing is periodic or the counter cannot be kept.
func (h *handler) countPrompt(sessionID string, periodic bool) int {
	if !periodic || !safeSessionID(sessionID) {
		return -1
	}
	dir := filepath.Join(h.stateDir, promptCountDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return -1
	}
	path := filepath.Join(dir, sessionID)
	raw, _ := os.ReadFile(path)
	n, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	n++
	if os.WriteFile(path, []byte(strconv.Itoa(n)), 0o600) != nil {
		return -1
	}
	if n == 1 {
		pruneCounters(dir, h.now().Add(-promptCountTTL))
	}
	return n
}

// pruneCounters removes counters last written before cutoff.
func pruneCounters(dir string, cutoff time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// refreshInstructions is `sessionhub hook refresh-instructions`, which
// session-start starts detached: it rewrites the local rules copy when the
// server's version changed.
func (h *handler) refreshInstructions(ctx context.Context) error {
	c, err := h.newClient()
	if err != nil {
		return err
	}
	_, err = client.RefreshInstructions(ctx, c, client.InstructionsPath(h.stateDir))
	return err
}

// permissionPayload is the PermissionRequest hook input sessionhub reads.
type permissionPayload struct {
	SessionID   string          `json:"session_id"`
	CWD         string          `json:"cwd"`
	ToolName    string          `json:"tool_name"`
	ToolInput   json.RawMessage `json:"tool_input"`
	Suggestions json.RawMessage `json:"permission_suggestions"`
}

// permissionRequest is `sessionhub hook permission-request`. It posts the request
// and waits up to permWait for a decision from sessionhub. It prints the decision,
// or nothing at all on an opt-out, an error, an expiry, or an answer in the
// terminal, so Claude Code's own dialog applies. Subagent calls are handled
// too: their prompt shows in the same terminal.
func (h *handler) permissionRequest(ctx context.Context) error {
	if h.remoteOff() {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(h.stdin, maxStdin))
	if err != nil {
		return fmt.Errorf("hook permission-request: read stdin: %w", err)
	}
	var in permissionPayload
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("hook permission-request: bad stdin JSON: %w", err)
	}
	if in.SessionID == "" || in.ToolName == "" {
		return errors.New("hook permission-request: stdin has no session_id or tool_name")
	}
	input, _, err := api.CapToolInput(in.ToolInput)
	if err != nil {
		return fmt.Errorf("hook permission-request: tool_input: %w", err)
	}
	suggestions := in.Suggestions
	if len(suggestions) > maxSuggestionBytes {
		suggestions = nil
	}
	c, err := h.newClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, h.permWait)
	defer cancel()
	req, err := c.CreatePermission(ctx, in.SessionID, api.PermissionIn{ToolName: cleanText(in.ToolName, 128),
		ToolInput: input, CWD: foldControl(in.CWD), Suggestions: suggestions})
	if err != nil {
		return h.permissionError(err)
	}
	if req.ID == "" {
		return errors.New("hook permission-request: the server returned no request ID")
	}
	for {
		start := time.Now()
		r, err := c.WaitDecision(ctx, req.ID, h.pollWait)
		if ctx.Err() != nil {
			return nil // waited permWait: the dialog in the terminal stays
		}
		if err != nil {
			return h.permissionError(err)
		}
		if r == nil {
			// 204: still open. A server that answers at once is not asked
			// again in a tight loop.
			if d := h.pollPause - time.Since(start); d > 0 {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(d):
				}
			}
			continue
		}
		if r.ID != req.ID {
			return nil // not this request's answer: never act on it
		}
		out, ok := decisionOutput(*r)
		if !ok {
			return nil // expired, answered in the terminal, or closed
		}
		return writeHookOutput(h.stdout, out)
	}
}

// permissionError is the permission hook's result for a failed call. A 429
// gets its own line in the hook log (stderr) and no error; any other error
// is returned for cmd/sessionhub to log. Either way the hook prints nothing, so
// the dialog in the terminal applies.
func (h *handler) permissionError(err error) error {
	var se *client.StatusError
	if errors.As(err, &se) && se.Status == http.StatusTooManyRequests {
		fmt.Fprintf(h.stderr, "hook permission-request: sessionhub answered 429 (too many requests); the terminal dialog applies\n")
		return nil
	}
	return fmt.Errorf("hook permission-request: %w", err)
}

// decisionOutput is the hook output for a decided request. ok is false for
// any other state or an unknown decision.
func decisionOutput(r api.PermissionRequest) (permissionOutput, bool) {
	var out permissionOutput
	if r.State != api.PermissionDecided {
		return out, false
	}
	out.HookSpecificOutput.HookEventName = "PermissionRequest"
	switch r.Decision {
	case api.DecisionAllow:
		out.HookSpecificOutput.Decision = permissionDecision{Behavior: "allow"}
	case api.DecisionDeny:
		msg := cleanText(r.Reason, 200)
		if msg == "" {
			msg = denyMessage
		}
		out.HookSpecificOutput.Decision = permissionDecision{Behavior: "deny", Message: msg}
	default:
		return out, false
	}
	return out, true
}
