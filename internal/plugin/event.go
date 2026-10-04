package plugin

import (
	"encoding/json"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr"
)

// eventPayload is the payload of the events the plugin queues. herdr events
// carry no session ID, so the watcher resolves pane → session later;
// HerdrSession keeps it from resolving against another herdr server's panes
// (pane IDs repeat across servers).
type eventPayload struct {
	AgentState   string `json:"agent_state,omitempty"`  // state_changed: sessionhub agent state (the server stores it)
	AgentStatus  string `json:"agent_status,omitempty"` // state_changed: herdr's raw value
	PaneID       string `json:"pane_id"`
	WorkspaceID  string `json:"workspace_id,omitempty"`
	HerdrSession string `json:"herdr_session"`
	HerdrEvent   string `json:"herdr_event"`
}

// itemFromEvent turns one herdr event into a queue item. ok is false for
// events sessionhub ignores (another agent, or a kind sessionhub does not hook). It makes
// no network or herdr call.
//
//   - pane_agent_detected → "upsert" for the pane (the watcher fills in the
//     session from its snapshot cache).
//   - pane_agent_status_changed → "event" state_changed with agent_state.
//   - pane_closed, pane_exited → "event" pane_closed.
func itemFromEvent(e herdr.Event, herdrSession string, now time.Time) (client.Item, bool, error) {
	now = now.UTC()
	switch e.Event {
	case herdr.EventAgentDetected:
		d, err := e.AgentDetected()
		if err != nil {
			return client.Item{}, false, err
		}
		if d.Agent != "claude" || d.PaneID == "" {
			return client.Item{}, false, nil
		}
		body, err := json.Marshal(api.SessionUpsert{
			Agent:          d.Agent,
			Source:         api.SourcePlugin,
			HerdrSession:   herdrSession,
			HerdrWorkspace: d.WorkspaceID,
			HerdrPane:      d.PaneID,
		})
		if err != nil {
			return client.Item{}, false, err
		}
		return client.Item{Op: client.OpUpsert, PaneID: d.PaneID, Body: body, QueuedAt: now}, true, nil

	case herdr.EventAgentStatusChange:
		d, err := e.AgentStatusChanged()
		if err != nil {
			return client.Item{}, false, err
		}
		// agent is optional in the schema; when present it must be claude.
		if (d.Agent != "" && d.Agent != "claude") || d.PaneID == "" {
			return client.Item{}, false, nil
		}
		return eventItem(api.KindStateChanged, eventPayload{
			AgentState:   agentState(d.AgentStatus),
			AgentStatus:  d.AgentStatus,
			PaneID:       d.PaneID,
			WorkspaceID:  d.WorkspaceID,
			HerdrSession: herdrSession,
			HerdrEvent:   e.Event,
		}, now)

	case herdr.EventPaneClosed, herdr.EventPaneExited:
		var d herdr.PaneRef
		var err error
		if e.Event == herdr.EventPaneClosed {
			d, err = e.PaneClosed()
		} else {
			d, err = e.PaneExited()
		}
		if err != nil {
			return client.Item{}, false, err
		}
		if d.PaneID == "" {
			return client.Item{}, false, nil
		}
		return eventItem(api.KindPaneClosed, eventPayload{
			PaneID:       d.PaneID,
			WorkspaceID:  d.WorkspaceID,
			HerdrSession: herdrSession,
			HerdrEvent:   e.Event,
		}, now)
	}
	return client.Item{}, false, nil
}

func eventItem(kind string, p eventPayload, now time.Time) (client.Item, bool, error) {
	payload, err := json.Marshal(p)
	if err != nil {
		return client.Item{}, false, err
	}
	body, err := json.Marshal(api.EventIn{Kind: kind, Source: api.SourcePlugin, TS: now, Payload: payload})
	if err != nil {
		return client.Item{}, false, err
	}
	return client.Item{Op: client.OpEvent, PaneID: p.PaneID, Body: body, QueuedAt: now}, true, nil
}
