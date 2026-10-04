package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
)

// actionHub answers the rule and message routes the way the server does.
type actionHub struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []string // "METHOD path body"
}

func newActionHub(t *testing.T) *actionHub {
	h := &actionHub{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.bodies = append(h.bodies, r.Method+" "+r.URL.Path+" "+string(b))
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/instructions":
			var in api.InstructionIn
			json.Unmarshal(b, &in)
			if strings.Contains(in.Text, "FULL") {
				w.WriteHeader(409)
				io.WriteString(w, `{"error":"full: the rules use 1900 of 2000 characters and this one has 150; remove a rule first"}`)
				return
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(api.Instruction{ID: 7, Text: in.Text, CreatedBy: "tower"})
		case r.Method == "GET" && r.URL.Path == "/v1/instructions":
			io.WriteString(w, `{"instructions":[{"id":3,"text":"Never push to main.","created_by":"tower"},`+
				`{"id":7,"text":"Write in British English.","created_by":"web:phone"}],"version":"abc"}`)
		case r.Method == "DELETE" && r.URL.Path == "/v1/instructions/7":
			w.WriteHeader(204)
		case r.Method == "DELETE":
			w.WriteHeader(404)
			io.WriteString(w, `{"error":"not found: rule 8"}`)
		case r.Method == "POST" && r.URL.Path == "/v1/messages":
			var in api.MessagesIn
			json.Unmarshal(b, &in)
			out := api.MessagesOut{}
			for _, id := range in.SessionIDs {
				if id == sid2 {
					out.Results = append(out.Results, api.MessageResult{SessionID: id, State: api.MessageRefused, Detail: "the session is not in herdr"})
					continue
				}
				out.Results = append(out.Results, api.MessageResult{SessionID: id, ID: "msg_x", State: api.MessageQueued})
			}
			json.NewEncoder(w).Encode(out)
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"error":"no such endpoint"}`)
		}
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *actionHub) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.bodies...)
}

func callTool(t *testing.T, s *Server, name, args string) (string, bool) {
	t.Helper()
	got := transcript(t, s, callLine(1, name, args))
	if len(got) != 1 {
		t.Fatalf("%s: %d responses", name, len(got))
	}
	return toolText(t, decode(t, got[0]))
}

func TestRememberAndForget(t *testing.T) {
	h := newActionHub(t)
	s, _, _ := newTestServer(t, h.URL, 1)
	msg, isErr := callTool(t, s, "remember", `{"text":"  Write in\nBritish English.  "}`)
	if isErr || !strings.Contains(msg, "rule 7 saved") || !strings.Contains(msg, "next prompt") {
		t.Errorf("remember: %q %v", msg, isErr)
	}
	if b := h.seen(); len(b) != 1 || b[0] != `POST /v1/instructions {"text":"Write in British English."}` {
		t.Errorf("request %q", b)
	}
	if msg, isErr := callTool(t, s, "remember", `{"text":"FULL rule"}`); !isErr || !strings.Contains(msg, "full") ||
		!strings.Contains(msg, "forget") {
		t.Errorf("full: %q %v", msg, isErr)
	}
	if msg, isErr := callTool(t, s, "remember", `{"text":" \n "}`); !isErr || !strings.Contains(msg, "empty") {
		t.Errorf("empty: %q %v", msg, isErr)
	}
	if msg, isErr := callTool(t, s, "remember", `{"text":"`+strings.Repeat("x", 301)+`"}`); !isErr || !strings.Contains(msg, "300") {
		t.Errorf("too long: %q %v", msg, isErr)
	}
	if msg, isErr := callTool(t, s, "forget", `{"id":7}`); isErr || !strings.Contains(msg, "rule 7 removed") {
		t.Errorf("forget: %q %v", msg, isErr)
	}
	if msg, isErr := callTool(t, s, "forget", `{"id":8}`); !isErr || !strings.Contains(msg, "no rule 8") {
		t.Errorf("forget unknown: %q %v", msg, isErr)
	}
	if msg, isErr := callTool(t, s, "forget", `{"id":"seven"}`); !isErr {
		t.Errorf("forget with a string id: %q", msg)
	}
}

func TestInstructionsTool(t *testing.T) {
	h := newActionHub(t)
	s, _, _ := newTestServer(t, h.URL, 1)
	msg, isErr := callTool(t, s, "instructions", `{}`)
	want := "sessionhub standing rules:\n3. Never push to main. (added by tower)\n7. Write in British English. (added by web:phone)"
	if isErr || msg != want {
		t.Errorf("instructions:\n got %q\nwant %q", msg, want)
	}
	down, _, _ := newTestServer(t, "http://127.0.0.1:1", 1)
	if msg, isErr := callTool(t, down, "instructions", `{}`); !isErr || !strings.Contains(msg, "could not") {
		t.Errorf("server down: %q %v", msg, isErr)
	}
}

func TestSendToSessions(t *testing.T) {
	h := newActionHub(t)
	s, _, state := newTestServer(t, h.URL, 4242)
	writeCurrent(t, state, 4242, sid1)
	msg, isErr := callTool(t, s, "send_to_sessions", `{"session_ids":["`+sid1+`","`+sid2+`"],"text":"Please rebase.\nThanks."}`)
	if isErr {
		t.Fatalf("send: %q", msg)
	}
	for _, want := range []string{"11111111: queued", "66666666: refused: the session is not in herdr"} {
		if !strings.Contains(msg, want) {
			t.Errorf("result %q lacks %q", msg, want)
		}
	}
	b := h.seen()
	var in api.MessagesIn
	json.Unmarshal([]byte(strings.TrimPrefix(b[0], "POST /v1/messages ")), &in)
	if in.Text != "Please rebase.\nThanks." || in.FromSession != sid1 || len(in.SessionIDs) != 2 {
		t.Errorf("request %+v", in)
	}
	if msg, isErr := callTool(t, s, "send_to_sessions", `{"session_ids":[],"text":"x"}`); !isErr {
		t.Errorf("no targets: %q", msg)
	}
	if msg, isErr := callTool(t, s, "send_to_sessions", `{"session_ids":["`+sid1+`"],"text":"  "}`); !isErr {
		t.Errorf("blank text: %q", msg)
	}
}

func TestSendToSessionsErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{404, "unknown"},
		{409, "another machine"},
		{429, "too many"},
		{500, "boom"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			io.WriteString(w, `{"error":"boom"}`)
		}))
		s, _, state := newTestServer(t, srv.URL, 4242)
		writeCurrent(t, state, 4242, sid1)
		msg, isErr := callTool(t, s, "send_to_sessions", `{"session_ids":["`+sid2+`"],"text":"hi"}`)
		srv.Close()
		if !isErr || !strings.Contains(msg, tc.want) {
			t.Errorf("status %d: %q %v", tc.status, msg, isErr)
		}
	}
	h := newActionHub(t)
	s, _, _ := newTestServer(t, h.URL, 4243)
	t.Setenv("HERDR_PANE_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	if msg, isErr := callTool(t, s, "send_to_sessions", `{"session_ids":["`+sid2+`"],"text":"hi"}`); !isErr || !strings.Contains(msg, "session's ID") {
		t.Errorf("no own session: %q %v", msg, isErr)
	}
	if len(h.seen()) != 0 {
		t.Errorf("request sent without a session: %q", h.seen())
	}
}

// TestServerErrorsAreCleaned: every tool quotes a server error with its
// control characters replaced and cut to maxReasonLen runes.
func TestServerErrorsAreCleaned(t *testing.T) {
	hostile := "bad \x1b[31mred\nline " + strings.Repeat("x", 1000)
	body, _ := json.Marshal(map[string]string{"error": hostile})
	for _, code := range []int{400, 404, 409, 429, 500} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			w.Write(body)
		}))
		s, _, state := newTestServer(t, srv.URL, 4300+code)
		writeCurrent(t, state, 4300+code, sid1)
		for name, args := range map[string]string{
			"remember":         `{"text":"Be brief."}`,
			"forget":           `{"id":8}`,
			"instructions":     `{}`,
			"send_to_sessions": `{"session_ids":["` + sid2 + `"],"text":"hi"}`,
			"set_title":        `{"title":"T"}`,
		} {
			msg, _ := callTool(t, s, name, args)
			if strings.ContainsAny(msg, "\x1b\n") {
				t.Errorf("%d %s: control character in %q", code, name, msg)
			}
			if n := strings.Count(msg, "x"); n > maxReasonLen {
				t.Errorf("%d %s: quotes %d characters of the error, want at most %d", code, name, n, maxReasonLen)
			}
		}
		srv.Close()
	}
	if got := reason(hostile); got != clean(hostile, maxReasonLen) || len([]rune(got)) > maxReasonLen {
		t.Errorf("reason = %q", got)
	}
}
