package cli

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// watchRefresh is how often sessionhub inbox --watch reads the inbox.
const watchRefresh = 5 * time.Second

// watchHelp is the first line of the view.
const watchHelp = "sessionhub inbox   j/k move  Enter jump  d dismiss  s snooze  r refresh  q quit"

// key is one key press: a printable character such as "j", or a name below.
type key string

const (
	keyUp    key = "up"
	keyDown  key = "down"
	keyEnter key = "enter"
	keyEsc   key = "esc"
	keyCtrlC key = "ctrl+c"
)

// parseKeys splits raw terminal input into keys. Arrow keys arrive as
// ESC [ A and ESC [ B, or ESC O A and ESC O B in application cursor mode.
// Other control bytes are dropped.
func parseKeys(b []byte) []key {
	var out []key
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c == 0x1b && i+2 < len(b) && (b[i+1] == '[' || b[i+1] == 'O'):
			switch b[i+2] {
			case 'A':
				out = append(out, keyUp)
			case 'B':
				out = append(out, keyDown)
			}
			i += 2
		case c == 0x1b:
			out = append(out, keyEsc)
		case c == '\r' || c == '\n':
			out = append(out, keyEnter)
		case c == 0x03:
			out = append(out, keyCtrlC)
		case c >= 0x20 && c < 0x7f:
			out = append(out, key(string(rune(c))))
		}
	}
	return out
}

type actionKind int

const (
	actNone actionKind = iota
	actQuit
	actRefresh
	actJump
	actDismiss
	actSnooze
)

// watchAction is what a key asks the loop to do.
type watchAction struct {
	kind  actionKind
	item  api.InboxItem // jump, dismiss, snooze
	until time.Time     // snooze
}

// watchState is what the view shows. Keys change it through handleKey,
// reads through withInbox, and actions through watchAct.
type watchState struct {
	items     []api.InboxItem
	selected  string    // session ID under the cursor; "" with no items
	snoozing  bool      // s was pressed: the next key picks the length
	snoozeFor string    // session ID that was under the cursor at s
	status    string    // what the last action did
	errText   string    // the last error
	refreshed time.Time // the last good inbox read
}

// cursor is the index of the selected item, or -1.
func (s watchState) cursor() int {
	for i, it := range s.items {
		if it.Session.ID == s.selected {
			return i
		}
	}
	return -1
}

// withInbox replaces the items after a read. The cursor stays on its
// session while it is listed; when it leaves, the cursor moves to the item
// now at its old position, else to the last item. With nothing selected
// before, it starts on the first item.
func (s watchState) withInbox(items []api.InboxItem, now time.Time) watchState {
	old := s.cursor()
	s.items = items
	s.refreshed = now
	switch {
	case s.cursor() >= 0:
	case len(items) == 0:
		s.selected = ""
	case old < 0:
		s.selected = items[0].Session.ID
	case old < len(items):
		s.selected = items[old].Session.ID
	default:
		s.selected = items[len(items)-1].Session.ID
	}
	return s
}

// snoozeKeys maps the key after s to a parseSnooze argument.
var snoozeKeys = map[key]string{"1": "1h", "4": "4h", "t": "tomorrow"}

// handleKey applies one key. q and Ctrl+C always quit. After s, the next key
// picks the snooze length, and any other key (Esc included) cancels.
func handleKey(s watchState, k key, now time.Time) (watchState, watchAction) {
	if k == keyCtrlC || k == "q" {
		return s, watchAction{kind: actQuit}
	}
	i := s.cursor()
	if s.snoozing {
		s.snoozing = false
		arg, ok := snoozeKeys[k]
		if !ok || i < 0 || s.selected != s.snoozeFor {
			s.status = "snooze cancelled"
			return s, watchAction{}
		}
		until, err := parseSnooze(arg, now)
		if err != nil {
			s.errText = err.Error()
			return s, watchAction{}
		}
		return s, watchAction{kind: actSnooze, item: s.items[i], until: until}
	}
	switch k {
	case "j", keyDown:
		if i >= 0 && i+1 < len(s.items) {
			s.selected = s.items[i+1].Session.ID
		}
	case "k", keyUp:
		if i > 0 {
			s.selected = s.items[i-1].Session.ID
		}
	case "r":
		return s, watchAction{kind: actRefresh}
	case keyEnter:
		if i >= 0 {
			return s, watchAction{kind: actJump, item: s.items[i]}
		}
	case "d":
		if i >= 0 {
			return s, watchAction{kind: actDismiss, item: s.items[i]}
		}
	case "s":
		if i >= 0 {
			s.snoozing, s.snoozeFor = true, s.selected
			s.status = "snooze for: 1 = 1 hour, 4 = 4 hours, t = until 9:00 tomorrow, Esc = cancel"
		}
	}
	return s, watchAction{}
}

// renderWatch draws the view as exactly height lines (at least 3) of at most
// width runes: the help line, the groups as sessionhub inbox prints them with the
// cursor row marked "> ", and the status line last. When the groups don't
// fit, the window scrolls to keep the cursor row in view.
func renderWatch(s watchState, now time.Time, width, height int) []string {
	if height < 3 {
		height = 3
	}
	body, cur := inboxLines(s.items, now, width, s.selected)
	if len(body) == 0 && !s.refreshed.IsZero() {
		body = []string{"nothing needs you"}
	}
	avail := height - 2
	start := 0
	if cur >= avail {
		start = cur - avail + 1
	}
	end := start + avail
	if end > len(body) {
		end = len(body)
	}
	out := []string{cut(watchHelp, width)}
	out = append(out, body[start:end]...)
	for len(out) < height-1 {
		out = append(out, "")
	}
	return append(out, cut(watchStatus(s), width))
}

// watchStatus is the last line: the last refresh time, what the last
// action did, and the last error, each cleaned.
func watchStatus(s watchState) string {
	parts := []string{"loading..."}
	if !s.refreshed.IsZero() {
		parts[0] = "updated " + s.refreshed.Format("15:04:05")
	}
	if s.status != "" {
		parts = append(parts, clean(s.status, 0))
	}
	if s.errText != "" {
		parts = append(parts, "error: "+clean(s.errText, 0))
	}
	return strings.Join(parts, "  ")
}

// jumper takes the inbox pane to a session (*resume.Jumper). It returns one
// line for the status line.
type jumper interface {
	Jump(ctx context.Context, s api.Session) (string, error)
}

// watchIO is the loop's outside world. Tests drive it with channels; a nil
// channel never fires.
type watchIO struct {
	keys   <-chan []byte    // raw terminal input, closed at the end of input
	ticks  <-chan time.Time // the refresh timer
	resize <-chan os.Signal // SIGWINCH
	draw   func(lines []string)
	size   func() (width, height int)
}

// watchLoop runs the view until q, Ctrl+C, the end of input, or ctx ends.
// It reads the inbox at start, on every tick, and after every action. When
// ctx ends it returns ctx's error.
//
// A reader goroutine watches the keys: q or Ctrl+C cancels the requests in
// flight (reads, jumps, dismisses, snoozes), so quitting never waits on a
// slow server. The loop still receives every key in order and quits at q.
func (e *env) watchLoop(ctx context.Context, w watchIO) error {
	reqCtx, cancelReq := context.WithCancel(ctx)
	defer cancelReq()
	var keys chan []byte
	if w.keys != nil {
		keys = make(chan []byte, 256)
		done := make(chan struct{})
		defer close(done)
		go func() {
			defer close(keys)
			for b := range w.keys {
				for _, k := range parseKeys(b) {
					if k == "q" || k == keyCtrlC {
						cancelReq()
					}
				}
				select {
				case keys <- b:
				case <-done:
					return
				}
			}
		}()
	}
	var s watchState
	refresh := func() {
		in, err := e.inbox.Inbox(reqCtx)
		if err != nil {
			s.errText = "refresh: " + err.Error()
			return
		}
		s = s.withInbox(in.Items, e.now())
	}
	draw := func() {
		width, height := w.size()
		w.draw(renderWatch(s, e.now(), width, height))
	}
	refresh()
	draw()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.ticks:
			if s.errText != "" && strings.HasPrefix(s.errText, "refresh: ") {
				s.errText = ""
			}
			refresh()
		case <-w.resize:
		case b, ok := <-keys:
			if !ok {
				return ctx.Err()
			}
			for _, k := range parseKeys(b) {
				var a watchAction
				s, a = handleKey(s, k, e.now())
				switch a.kind {
				case actQuit:
					return nil
				case actNone:
					continue
				case actJump:
					// ssh can take up to 15 s: say what is going on first.
					s.status, s.errText = "jumping to "+shortID(a.item.Session.ID)+"...", ""
					draw()
				}
				s = e.watchAct(reqCtx, s, a)
				refresh()
			}
		}
		draw()
	}
}

// watchAct runs one action and puts its outcome in the status line.
func (e *env) watchAct(ctx context.Context, s watchState, a watchAction) watchState {
	id := shortID(a.item.Session.ID)
	s.status, s.errText = "", ""
	switch a.kind {
	case actDismiss:
		if err := e.inbox.DismissInbox(ctx, a.item.Session.ID, a.item.Since); err != nil {
			s.errText = "dismiss " + id + ": " + err.Error()
		} else {
			s.status = "dismissed " + id
		}
	case actSnooze:
		if err := e.inbox.SnoozeInbox(ctx, a.item.Session.ID, a.item.Since, a.until); err != nil {
			s.errText = "snooze " + id + ": " + err.Error()
		} else {
			s.status = "snoozed " + id + " until " + a.until.Format("Mon 2 Jan 15:04")
		}
	case actJump:
		if e.jump == nil {
			s.status = "run: " + a.item.Session.ResumeCommand
			break
		}
		msg, err := e.jump.Jump(ctx, a.item.Session)
		if err != nil {
			s.errText = "jump " + id + ": " + err.Error()
		} else {
			s.status = msg
		}
	}
	return s
}
