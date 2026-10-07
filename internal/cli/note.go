package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/cli/termtext"
	"github.com/abdallah/session-hub/internal/client"
)

// noteAPI is the part of *client.Client that sessionhub note uses.
type noteAPI interface {
	AddNote(ctx context.Context, in api.NoteIn) (api.NoteResult, error)
}

// noteQueue keeps a note that could not be sent; *client.Queue is one.
type noteQueue interface {
	Append(it client.Item) error
}

const noteUsage = `usage: sessionhub note <text>`

// noteTimeout bounds the send, so worklog's 5-second limit leaves time to
// queue the note.
const noteTimeout = 4 * time.Second

// sessionIDRE is the server's rule for a session ID.
var sessionIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// RunNote implements `sessionhub note <text>`.
func RunNote(ctx context.Context, args []string) error {
	e, err := defaultEnv()
	if err != nil {
		return err
	}
	return e.note(ctx, args)
}

// note sends a worklog note for the session in CLAUDE_CODE_SESSION_ID, if
// any. A note the server cannot take now is queued and sent later; one for
// a session the server does not know goes again without the session.
func (e *env) note(ctx context.Context, args []string) error {
	text := strings.Join(strings.Fields(strings.Join(args, " ")), " ")
	if text == "" {
		return errors.New(noteUsage)
	}
	in := api.NoteIn{ID: client.NewTaskEventID(), Text: text}
	if sid := e.getenv("CLAUDE_CODE_SESSION_ID"); sessionIDRE.MatchString(sid) {
		in.SessionID = sid
	}
	send := func() (api.NoteResult, error) {
		ctx, cancel := context.WithTimeout(ctx, noteTimeout)
		defer cancel()
		return e.notes.AddNote(ctx, in)
	}
	r, err := send()
	if client.IsNotFound(err) && in.SessionID != "" {
		in.SessionID = ""
		r, err = send()
	}
	switch {
	case err == nil:
		line := r.Task.Title
		if r.Task.Ref != "" && !strings.EqualFold(r.Task.Ref, r.Task.Title) {
			line = r.Task.Ref + " " + line
		}
		fmt.Fprintf(e.out, "noted → %s (%s)\n", clean(line, termtext.TitleWidth), r.Action)
		return nil
	case client.IsRetryable(err):
		body, _ := json.Marshal(in)
		if qerr := e.queue.Append(client.Item{Op: client.OpNote, SessionID: in.SessionID, Body: body, QueuedAt: time.Now().UTC()}); qerr != nil {
			return fmt.Errorf("note: %v; queue: %w", err, qerr)
		}
		fmt.Fprintf(e.out, "queued: %s\n", clean(text, termtext.TitleWidth))
		return nil
	default:
		return fmt.Errorf("note: %w", err)
	}
}
