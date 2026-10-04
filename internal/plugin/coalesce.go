package plugin

import (
	"encoding/json"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

// itemMeta is what coalescing and resolution read from a queued item.
type itemMeta struct {
	kind         string // event kind for OpEvent, else ""
	herdrSession string // herdr session the pane belongs to, "" when unknown
}

func metaOf(it client.Item) itemMeta {
	switch it.Op {
	case client.OpUpsert:
		var u api.SessionUpsert
		if json.Unmarshal(it.Body, &u) == nil {
			return itemMeta{herdrSession: u.HerdrSession}
		}
	case client.OpHerdrSessions:
		var p api.HerdrSessionsPut
		if json.Unmarshal(it.Body, &p) == nil {
			return itemMeta{herdrSession: p.HerdrSession}
		}
	case client.OpEvent:
		var e api.EventIn
		if json.Unmarshal(it.Body, &e) == nil {
			var p eventPayload
			json.Unmarshal(e.Payload, &p)
			return itemMeta{kind: e.Kind, herdrSession: p.HerdrSession}
		}
	}
	return itemMeta{}
}

// coalesce keeps the queue order and drops items a later item makes
// pointless. Items that name a session (hooks, MCP; they may also carry a
// pane) are keyed by session ID. Plugin items, which name only a pane, are
// keyed by herdr session + pane ID. Per key:
//
//   - state_changed is superseded by any later state_changed or pane_closed,
//     so the latest state wins and pane_closed beats earlier state.
//   - pane_closed is superseded by a later pane_closed (pane.closed and
//     pane.exited for the same pane send one event).
//   - a digest is superseded by a later digest for the same session.
//   - a plugin pane upsert (from pane.agent_detected, no session ID yet) is
//     superseded by a later one.
//
// Only the latest herdr_sessions body per herdr session is kept. Other items
// (prompt, ended, report, title, and every upsert that names a session) are
// never dropped: a hooks upsert can carry first_prompt, which the server
// records once, so dropping the first one would record a later prompt as the
// first.
func coalesce(items []client.Item) (keep, superseded []client.Item) {
	type seen struct{ state, closed, upsert, digest bool }
	byKey := map[string]*seen{}
	drop := make([]bool, len(items))
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		m := metaOf(it)
		var key string
		switch {
		case it.Op == client.OpHerdrSessions:
			key = "hs\x00" + m.herdrSession
		case it.SessionID != "":
			key = "sid\x00" + it.SessionID
		case it.PaneID != "":
			key = "pane\x00" + m.herdrSession + "\x00" + it.PaneID
		default:
			continue
		}
		s := byKey[key]
		if s == nil {
			s = &seen{}
			byKey[key] = s
		}
		switch {
		case it.Op == client.OpHerdrSessions:
			drop[i] = s.upsert
			s.upsert = true
		case it.Op == client.OpUpsert && it.SessionID == "" && it.PaneID != "":
			drop[i] = s.upsert
			s.upsert = true
		case it.Op == client.OpEvent && m.kind == api.KindStateChanged:
			drop[i] = s.state || s.closed
			s.state = true
		case it.Op == client.OpDigest:
			drop[i] = s.digest
			s.digest = true
		case it.Op == client.OpEvent && m.kind == api.KindPaneClosed:
			drop[i] = s.closed
			s.closed = true
		}
	}
	for i, it := range items {
		if drop[i] {
			superseded = append(superseded, it)
		} else {
			keep = append(keep, it)
		}
	}
	return keep, superseded
}
