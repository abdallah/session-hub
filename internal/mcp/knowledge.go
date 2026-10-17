package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

const (
	maxKnowledgeText = 300
	maxKnowledgeWhy  = 200
	maxScopeLen      = 200
	maxSearchResults = 20
)

// proposeKnowledge proposes an entry for the user to approve. The client id
// makes a replay of the queued call a no-op. The server stays authoritative
// on limits and secrets; the local checks only save a round trip.
func (s *Server) proposeKnowledge(ctx context.Context, args json.RawMessage) toolResult {
	var in struct {
		Kind     string `json:"kind"`
		Scope    string `json:"scope"`
		Text     string `json:"text"`
		Why      string `json:"why"`
		Replaces int64  `json:"replaces"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return text("invalid arguments: "+reason(err.Error()), true)
	}
	kind, scope := clean(in.Kind, 20), clean(in.Scope, maxScopeLen)
	txt, why := strings.TrimSpace(in.Text), strings.TrimSpace(in.Why)
	switch {
	case kind != api.KnowledgeConvention && kind != api.KnowledgeFact && kind != api.KnowledgeDecision:
		return text("kind must be convention, fact, or decision", true)
	case scope == "":
		return text("scope must not be empty: use global, repo:<origin>, ticket:<ref>, or machine:<name>", true)
	case txt == "":
		return text("text must not be empty", true)
	case utf8.RuneCountInString(txt) > maxKnowledgeText:
		return text(fmt.Sprintf("text must be at most %d characters", maxKnowledgeText), true)
	case utf8.RuneCountInString(why) > maxKnowledgeWhy:
		return text(fmt.Sprintf("why must be at most %d characters", maxKnowledgeWhy), true)
	case in.Replaces < 0:
		return text("replaces must be an entry number", true)
	}
	sid := s.resolveSession()
	if sid == "" {
		return text(noSession, false)
	}
	ki := api.KnowledgeIn{ClientID: client.NewKnowledgeID(), Kind: kind, Scope: scope, Text: txt, Why: why, Replaces: in.Replaces, SourceSession: sid}
	body, _ := json.Marshal(ki)
	it := client.Item{Op: client.OpKnowledgePropose, SessionID: sid, Body: body}
	return s.sendItem(ctx, it, "knowledge proposal", func(a hubAPI) (string, error) {
		out, err := a.ProposeKnowledge(ctx, ki)
		if err != nil {
			return "", err
		}
		msg := "The user approves it on the dashboard."
		if len(out.Similar) > 0 {
			msg += "\nSimilar existing entries:"
			for _, k := range out.Similar {
				msg += fmt.Sprintf("\n#%d [%s] %s", k.ID, clean(k.Kind, 20), clean(k.Text, maxKnowledgeText))
			}
		}
		return msg, nil
	})
}

// searchKnowledge lists active entries matching the query. Nothing is queued.
func (s *Server) searchKnowledge(ctx context.Context, args json.RawMessage) toolResult {
	var in struct {
		Query string `json:"query"`
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return text("invalid arguments: "+reason(err.Error()), true)
	}
	q := clean(in.Query, 200)
	if q == "" {
		return text("query must not be empty", true)
	}
	a, err := s.API()
	var found []api.Knowledge
	if err == nil {
		found, err = a.SearchKnowledge(ctx, q, clean(in.Scope, maxScopeLen))
	}
	if err != nil {
		return text("could not search knowledge: "+serverMessage(err), true)
	}
	if len(found) == 0 {
		return text("no matching knowledge", false)
	}
	if len(found) > maxSearchResults {
		found = found[:maxSearchResults]
	}
	lines := make([]string, len(found))
	for i, k := range found {
		lines[i] = fmt.Sprintf("#%d [%s] (%s) %s", k.ID, clean(k.Kind, 20), clean(k.Scope, maxScopeLen), clean(k.Text, maxKnowledgeText))
	}
	return text(strings.Join(lines, "\n"), false)
}

// confirmKnowledge marks an entry as still true. Nothing is queued.
func (s *Server) confirmKnowledge(ctx context.Context, args json.RawMessage) toolResult {
	var in struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return text("invalid arguments: "+reason(err.Error()), true)
	}
	if in.ID <= 0 {
		return text("id must be an entry number from search_knowledge", true)
	}
	a, err := s.API()
	if err == nil {
		err = a.ConfirmKnowledge(ctx, in.ID)
	}
	if err != nil {
		return text("could not confirm knowledge: "+serverMessage(err), true)
	}
	return text(fmt.Sprintf("sessionhub: knowledge %d confirmed.", in.ID), false)
}
