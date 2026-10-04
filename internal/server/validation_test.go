package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
)

// fieldCase is one value for one field and whether the server must accept it.
type fieldCase struct {
	name  string
	value string
	ok    bool
}

func identCases() []fieldCase {
	return []fieldCase{
		{"valid", "w1:2.a_b-c", true},
		{"empty means unknown", "", true},
		{"leading dash", "-w1", false},
		{"control char", "w1\x07", false},
		{"newline", "w1\nx", false},
		{"slash", "w/1", false},
		{"over cap", strings.Repeat("a", 65), false},
		{"at cap", strings.Repeat("a", 64), true},
	}
}

func textCases(max int) []fieldCase {
	return []fieldCase{
		{"valid", "fix the login bug", true},
		{"leading dash is prose", "-fix", true},
		{"newline", "line one\nline two", false},
		{"tab", "a\tb", false},
		{"escape", "a\x1b[31mred", false},
		{"DEL", "a\x7fb", false},
		{"C1 control", "a\u0085b", false},
		{"over cap", strings.Repeat("é", max+1), false},
		{"at cap", strings.Repeat("é", max), true},
	}
}

// TestFieldValidation walks every validated field against valid, leading
// dash, control character, and over-cap values. A rejected request must
// answer 400 naming the field and leave the stored state untouched; an
// accepted one must read back through the API.
func TestFieldValidation(t *testing.T) {
	type field struct {
		name  string
		cases []fieldCase
		send  func(e *env, v string) (int, []byte)
		got   func(e *env) string
	}
	register := func(set func(u *api.SessionUpsert, v string)) func(e *env, v string) (int, []byte) {
		return func(e *env, v string) (int, []byte) {
			u := api.SessionUpsert{ID: sid2, Agent: "claude", Source: api.SourceHooks}
			set(&u, v)
			code, b, _ := e.do("POST", "/v1/sessions", e.tokA, u)
			return code, b
		}
	}
	session := func(e *env) api.Session { return e.detail(sid2).Session }
	fields := []field{
		{"agent", identCases(), register(func(u *api.SessionUpsert, v string) { u.Agent = v }),
			func(e *env) string { return session(e).Agent }},
		{"herdr_pane", identCases(), register(func(u *api.SessionUpsert, v string) { u.HerdrPane = v }),
			func(e *env) string { return session(e).HerdrPane }},
		{"herdr_workspace", identCases(), register(func(u *api.SessionUpsert, v string) { u.HerdrWorkspace = v }),
			func(e *env) string { return session(e).HerdrWorkspace }},
		{"herdr_session", identCases(), register(func(u *api.SessionUpsert, v string) { u.HerdrSession = v }),
			func(e *env) string { return session(e).HerdrSession }},
		{"title_hint", textCases(200), register(func(u *api.SessionUpsert, v string) { u.TitleHint = v }),
			func(e *env) string { return session(e).Title }},
		{"first_prompt", textCases(200), register(func(u *api.SessionUpsert, v string) { u.FirstPrompt = v }),
			func(e *env) string { return session(e).Title }},
		{"cwd", []fieldCase{
			{"valid", "/home/user/proj", true},
			{"leading dash", "-x/proj", true},
			{"control char", "/home/user/a\nb", false},
			{"DEL", "/home/user/a\x7f", false},
			{"over cap", "/" + strings.Repeat("a", 4096), false},
			{"at cap", "/" + strings.Repeat("a", 4095), true},
		}, register(func(u *api.SessionUpsert, v string) { u.CWD = v }),
			func(e *env) string { return session(e).CWD }},
		{"title", textCases(200), func(e *env, v string) (int, []byte) {
			code, b, _ := e.do("POST", "/v1/sessions/"+sid2+"/title", e.tokA, api.TitleIn{Title: v})
			return code, b
		}, func(e *env) string { return session(e).Title }},
		{"note", textCases(2000), func(e *env, v string) (int, []byte) {
			code, b, _ := e.do("POST", "/v1/sessions/"+sid2+"/report", e.tokA, api.ReportIn{Note: v})
			return code, b
		}, func(e *env) string { return e.detail(sid2).LatestReport.Note }},
	}
	for _, list := range []string{"done", "in_flight", "waiting_on"} {
		fields = append(fields, field{"report " + list, textCases(200), func(e *env, v string) (int, []byte) {
			r := api.ReportIn{}
			switch list {
			case "done":
				r.Done = []string{v}
			case "in_flight":
				r.InFlight = []string{v}
			default:
				r.WaitingOn = []string{v}
			}
			code, b, _ := e.do("POST", "/v1/sessions/"+sid2+"/report", e.tokA, r)
			return code, b
		}, func(e *env) string {
			r := e.detail(sid2).LatestReport
			return strings.Join(append(append(append([]string{}, r.Done...), r.InFlight...), r.WaitingOn...), "")
		}})
	}

	for _, f := range fields {
		for _, c := range f.cases {
			t.Run(f.name+"/"+c.name, func(t *testing.T) {
				e := newEnv(t)
				// title, note, and report items need an existing session;
				// register creates it for the others.
				e.register(e.tokA, api.SessionUpsert{ID: sid2})
				before := e.snapshot()
				code, body := f.send(e, c.value)
				if c.ok {
					if code != 200 && code != 201 {
						t.Fatalf("status %d, want accepted: %s", code, body)
					}
					if c.value == "" { // empty never overwrites; nothing to read back
						return
					}
					if got := f.got(e); got != c.value {
						t.Errorf("read back %q, want %q", got, c.value)
					}
					return
				}
				if code != 400 {
					t.Fatalf("status %d, want 400: %s", code, body)
				}
				var ae api.Error
				if err := json.Unmarshal(body, &ae); err != nil || ae.Error == "" {
					t.Fatalf("body is not api.Error: %s", body)
				}
				name := strings.TrimPrefix(f.name, "report ")
				if !strings.Contains(ae.Error, name) {
					t.Errorf("error %q does not name field %q", ae.Error, name)
				}
				if e.snapshot() != before {
					t.Error("rejected request changed state")
				}
			})
		}
	}
}

func TestSessionIDValidation(t *testing.T) {
	cases := []struct {
		name string
		id   string
		ok   bool
	}{
		{"uuid", sid1, true},
		{"underscore inside", "a_b-c", true},
		{"at cap", strings.Repeat("a", 128), true},
		{"leading dash", "-rf", false},
		{"leading underscore", "_x", false},
		{"control char", "abc\x07", false},
		{"slash", "a/b", false},
		{"over cap", strings.Repeat("a", 129), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			code, body, _ := e.do("POST", "/v1/sessions", e.tokA,
				api.SessionUpsert{ID: c.id, Agent: "claude", Source: api.SourceHooks})
			if c.ok {
				if code != 201 {
					t.Fatalf("status %d: %s", code, body)
				}
				if got := e.detail(c.id).ID; got != c.id {
					t.Errorf("read back %q", got)
				}
				return
			}
			if code != 400 || !strings.Contains(string(body), "session id") {
				t.Fatalf("status %d, body %s; want 400 naming session id", code, body)
			}
			if n := len(e.list("")); n != 0 {
				t.Errorf("%d sessions stored after a rejected id", n)
			}
		})
	}
}

// TestReconcileSkipsInvalidEntries: one bad entry must not fail the heartbeat.
// The good entries are stored, the bad ones are reported under invalid, and a
// bad snapshot-level herdr_session still returns 400.
func TestReconcileSkipsInvalidEntries(t *testing.T) {
	e := newEnv(t)
	path := "/v1/machines/self/herdr-sessions"
	put := api.HerdrSessionsPut{HerdrSession: "default", Sessions: []api.SessionUpsert{
		{ID: sid1, Agent: "claude", HerdrPane: "w1:1"},
		{ID: sid2, Agent: "claude", HerdrPane: "-w1"},
		{ID: "bad id", Agent: "claude", HerdrPane: "w1:3"},
		{ID: sid3, Agent: "claude", HerdrPane: "w1:4", TitleHint: "two\nlines"},
	}}
	var res struct {
		Upserted int `json:"upserted"`
		Invalid  []struct {
			ID     string `json:"id"`
			Reason string `json:"reason"`
		} `json:"invalid"`
	}
	e.must(200, "PUT", path, e.tokA, put, &res)
	if res.Upserted != 1 || len(res.Invalid) != 3 {
		t.Fatalf("result: %+v", res)
	}
	for i, want := range []struct{ id, reason string }{{sid2, "herdr_pane"}, {"bad id", "session id"}, {sid3, "title_hint"}} {
		if res.Invalid[i].ID != want.id || !strings.Contains(res.Invalid[i].Reason, want.reason) {
			t.Errorf("invalid[%d] = %+v, want id %s and reason about %s", i, res.Invalid[i], want.id, want.reason)
		}
	}
	if got := ids(e.list("")); len(got) != 1 || got[0] != sid1 {
		t.Errorf("stored sessions = %v, want only %s", got, sid1)
	}

	// An invalid entry for a session already stored does not end it.
	put.Sessions = []api.SessionUpsert{{ID: sid1, Agent: "claude", HerdrPane: "-w1"}}
	e.must(200, "PUT", path, e.tokA, put, &res)
	if len(res.Invalid) != 1 || e.detail(sid1).Status == api.StatusEnded {
		t.Errorf("invalid entry ended its session: %+v", res)
	}

	// The snapshot's own herdr_session is still validated.
	put.HerdrSession = "-bad"
	if code, body, _ := e.do("PUT", path, e.tokA, put); code != 400 || !strings.Contains(string(body), "herdr_session") {
		t.Fatalf("bad herdr_session: status %d, body %s", code, body)
	}
}
