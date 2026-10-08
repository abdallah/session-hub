package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/store"
)

// TestSessionLastNoteJSON checks last_note on GET /v1/sessions and GET
// /v1/sessions/{id}, and that a session without notes has none.
func TestSessionLastNoteJSON(t *testing.T) {
	e := newEnv(t)
	e.register(e.tokA, api.SessionUpsert{ID: sid1, CWD: "/home/user/proj"})
	e.register(e.tokB, api.SessionUpsert{ID: sid2, CWD: "/home/user/proj"})
	var r api.NoteResult
	e.must(http.StatusOK, "POST", "/v1/notes", e.tokA, api.NoteIn{ID: eventA, Text: "start fix X systemsdev-1", SessionID: sid1}, &r)

	var list []api.Session
	e.must(http.StatusOK, "GET", "/v1/sessions", e.tokA, nil, &list)
	var d api.SessionDetail
	e.must(http.StatusOK, "GET", "/v1/sessions/"+sid1, e.tokA, nil, &d)
	for _, s := range append(list, d.Session) {
		n := s.LastNote
		switch s.ID {
		case sid1:
			if n == nil || n.TaskID != r.Task.ID || n.TaskTitle != r.Task.Title || n.TaskRef != "systemsdev-1" ||
				n.TaskState != api.TaskInProgress || n.Text != "start fix X systemsdev-1" || n.At.IsZero() {
				t.Errorf("sid1 last note = %+v, want the note on %+v", n, r.Task)
			}
		case sid2:
			if n != nil {
				t.Errorf("sid2 last note = %+v, want none", n)
			}
		}
	}
	_, b, _ := e.do("GET", "/v1/sessions/"+sid2, e.tokA, nil)
	if strings.Contains(string(b), "last_note") {
		t.Errorf("a session without notes has last_note: %s", b)
	}
}

func TestAddNote(t *testing.T) {
	e := newEnv(t)
	e.server.SetRefURLs(store.RefURLs{Ticket: "https://yt/issue/{ref}"})
	e.register(e.tokA, api.SessionUpsert{ID: sid1, CWD: "/home/user/proj"})
	e.register(e.tokB, api.SessionUpsert{ID: sid2, CWD: "/home/user/proj"})

	var r api.NoteResult
	e.must(http.StatusOK, "POST", "/v1/notes", e.tokA, api.NoteIn{ID: eventA, Text: "start systemsdev-1", SessionID: sid1}, &r)
	if r.Action != api.NoteCreated || r.Task.State != api.TaskInProgress || r.Task.RefURL != "https://yt/issue/systemsdev-1" ||
		r.Task.CreatedBy != "session:"+sid1 {
		t.Errorf("note: %+v", r)
	}
	// Another machine's session, an unknown session, bad input.
	e.must(http.StatusConflict, "POST", "/v1/notes", e.tokA, api.NoteIn{ID: "te_BBBBBBBBBBB", Text: "x", SessionID: sid2}, nil)
	e.must(http.StatusNotFound, "POST", "/v1/notes", e.tokA, api.NoteIn{ID: "te_CCCCCCCCCCC", Text: "x", SessionID: "nosuch"}, nil)
	e.must(http.StatusBadRequest, "POST", "/v1/notes", e.tokA, api.NoteIn{ID: "bad", Text: "x"}, nil)
	e.must(http.StatusBadRequest, "POST", "/v1/notes", e.tokA, api.NoteIn{ID: "te_DDDDDDDDDDD", Text: ""}, nil)
	// No token.
	e.must(http.StatusUnauthorized, "POST", "/v1/notes", "", api.NoteIn{ID: "te_EEEEEEEEEEE", Text: "x"}, nil)
	// The dashboard needs the tasks action header.
	if code, b := e.webTask("POST", "/v1/notes", api.NoteIn{ID: "te_FFFFFFFFFFF", Text: "from the web"}); code != http.StatusOK {
		t.Errorf("web note: %d %s", code, b)
	}
}
