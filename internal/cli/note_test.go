package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

type fakeNotes struct {
	sent   []api.NoteIn
	errs   []error // returned in order, then nil
	result api.NoteResult
}

func (f *fakeNotes) AddNote(_ context.Context, in api.NoteIn) (api.NoteResult, error) {
	f.sent = append(f.sent, in)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return api.NoteResult{}, err
		}
	}
	return f.result, nil
}

type fakeQueue struct{ items []client.Item }

func (q *fakeQueue) Append(it client.Item) error { q.items = append(q.items, it); return nil }

func runNote(t *testing.T, f *fakeNotes, q *fakeQueue, vars map[string]string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	e := &env{notes: f, queue: q, out: &out, getenv: func(k string) string { return vars[k] }}
	err := e.note(context.Background(), args)
	return out.String(), err
}

var noteIDRE = regexp.MustCompile(`^te_[A-Za-z0-9_-]{11}$`)

const testSID = "aab78df9-ccb8-440c-a049-82fe54f640f0"

func TestNoteSendsSession(t *testing.T) {
	f := &fakeNotes{result: api.NoteResult{Action: api.NoteCreated, Task: api.Task{Title: "investigate X", Ref: "systemsdev-1"}}}
	out, err := runNote(t, f, &fakeQueue{}, map[string]string{"CLAUDE_CODE_SESSION_ID": testSID}, "investigate", "X", "systemsdev-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || f.sent[0].SessionID != testSID || f.sent[0].Text != "investigate X systemsdev-1" || !noteIDRE.MatchString(f.sent[0].ID) {
		t.Errorf("sent %+v", f.sent)
	}
	if out != "noted → systemsdev-1 investigate X (created)\n" {
		t.Errorf("output %q", out)
	}
}

func TestNoteNoSession(t *testing.T) {
	for _, sid := range []string{"", "../x", "-rf"} {
		f := &fakeNotes{result: api.NoteResult{Action: api.NoteCreated, Task: api.Task{Title: "plain"}}}
		out, err := runNote(t, f, &fakeQueue{}, map[string]string{"CLAUDE_CODE_SESSION_ID": sid}, "plain")
		if err != nil || len(f.sent) != 1 || f.sent[0].SessionID != "" || out != "noted → plain (created)\n" {
			t.Errorf("session %q: %+v %q %v", sid, f.sent, out, err)
		}
	}
}

func TestNoteQueuesWhenOffline(t *testing.T) {
	f := &fakeNotes{errs: []error{errors.New("dial tcp: connection refused")}}
	q := &fakeQueue{}
	out, err := runNote(t, f, q, map[string]string{"CLAUDE_CODE_SESSION_ID": testSID}, "offline", "work")
	if err != nil || out != "queued: offline work\n" {
		t.Fatalf("%q %v", out, err)
	}
	if len(q.items) != 1 || q.items[0].Op != client.OpNote || q.items[0].QueuedAt.IsZero() {
		t.Fatalf("queued %+v", q.items)
	}
	var in api.NoteIn
	if err := json.Unmarshal(q.items[0].Body, &in); err != nil || in != f.sent[0] {
		t.Errorf("queued body %+v, sent %+v", in, f.sent[0])
	}
}

// An unknown session is not worth queuing: the note goes again without it.
func TestNoteUnknownSession(t *testing.T) {
	f := &fakeNotes{errs: []error{&client.StatusError{Status: http.StatusNotFound, Message: "session not found"}},
		result: api.NoteResult{Action: api.NoteCreated, Task: api.Task{Title: "x"}}}
	q := &fakeQueue{}
	out, err := runNote(t, f, q, map[string]string{"CLAUDE_CODE_SESSION_ID": testSID}, "x")
	if err != nil || len(f.sent) != 2 || f.sent[1].SessionID != "" || f.sent[1].ID != f.sent[0].ID || len(q.items) != 0 {
		t.Errorf("%+v %v %q", f.sent, err, out)
	}
}

func TestNoteRejected(t *testing.T) {
	f := &fakeNotes{errs: []error{&client.StatusError{Status: http.StatusBadRequest, Message: "note names no work"}}}
	q := &fakeQueue{}
	if _, err := runNote(t, f, q, nil, "done:"); err == nil || !strings.Contains(err.Error(), "names no work") || len(q.items) != 0 {
		t.Errorf("%v %+v", err, q.items)
	}
}

func TestNoteUsage(t *testing.T) {
	if _, err := runNote(t, &fakeNotes{}, &fakeQueue{}, nil); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("%v", err)
	}
	if _, err := runNote(t, &fakeNotes{}, &fakeQueue{}, nil, "  "); err == nil {
		t.Error("blank note accepted")
	}
}
