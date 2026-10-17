package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/abdallah/session-hub/internal/api"
)

func (f *fakeActions) ListKnowledge(_ context.Context, state, scope string) ([]api.Knowledge, error) {
	f.kList = append(f.kList, state+"|"+scope)
	return f.kEntries, nil
}

func (f *fakeActions) ProposeKnowledge(_ context.Context, in api.KnowledgeIn) (api.KnowledgeAdded, error) {
	f.kProposed = append(f.kProposed, in)
	return api.KnowledgeAdded{Entry: api.Knowledge{ID: 7}, Similar: f.kSimilar}, nil
}

func (f *fakeActions) SearchKnowledge(_ context.Context, q, scope string) ([]api.Knowledge, error) {
	f.kList = append(f.kList, "search:"+q+"|"+scope)
	return f.kEntries, nil
}

func TestKnowledgeLs(t *testing.T) {
	f := &fakeActions{}
	e, out, _ := actionsEnv(f)
	ctx := context.Background()
	if err := e.knowledge(ctx, nil); err != nil || out.String() != "no knowledge\n" {
		t.Fatalf("empty: %q %v", out.String(), err)
	}
	if f.kList[0] != "active|" {
		t.Errorf("default state: %q", f.kList[0])
	}
	f.kEntries = []api.Knowledge{{ID: 3, Kind: "fact", Scope: "global", Text: "a\x1b[31m b", State: "proposed", CreatedBy: "blu"}}
	out.Reset()
	if err := e.knowledge(ctx, []string{"ls", "--state", "all", "--scope", "global"}); err != nil {
		t.Fatal(err)
	}
	if f.kList[1] != "|global" {
		t.Errorf("all/scope: %q", f.kList[1])
	}
	if want := "3  [fact] (global) a[31m b  (proposed, blu)\n"; out.String() != want {
		t.Errorf("line: %q want %q", out.String(), want)
	}
	if err := e.knowledge(ctx, []string{"--state", "proposed"}); err != nil || f.kList[2] != "proposed|" {
		t.Errorf("bare flags: %v %v", f.kList, err)
	}
	if err := e.knowledge(ctx, []string{"ls", "--state", "bogus"}); err == nil {
		t.Error("bad state accepted")
	}
}

func TestKnowledgeAdd(t *testing.T) {
	f := &fakeActions{kSimilar: []api.Knowledge{{ID: 2, Kind: "fact", Text: "close"}}}
	e, out, _ := actionsEnv(f)
	e.getenv = func(k string) string {
		if k == "CLAUDE_CODE_SESSION_ID" {
			return testSID
		}
		return ""
	}
	ctx := context.Background()
	if err := e.knowledge(ctx, []string{"add", "--every", "5", "--scope", "global", "--kind", "fact", "--why", "w", "the text"}); err != nil {
		t.Fatal(err)
	}
	want := "proposed knowledge 7; approve it on the dashboard\nsimilar: #2 [fact] close\n"
	if out.String() != want {
		t.Errorf("out %q", out.String())
	}
	in := f.kProposed[0]
	if in.Text != "the text" || in.Kind != "fact" || in.Scope != "global" || in.Why != "w" ||
		in.Every == nil || *in.Every != 5 || in.SourceSession != testSID || !strings.HasPrefix(in.ClientID, "k_") {
		t.Errorf("in %+v", in)
	}
	for _, args := range [][]string{
		{"add", "--scope", "global", "x"},
		{"add", "--kind", "fact", "x"},
		{"add", "--kind", "fact", "--scope", "global"},
		{"add", "--kind", "fact", "--scope", "global", "--every", "z", "x"},
		{"approve", "7"}, {"reject", "7"}, {"rm", "7"},
	} {
		err := e.knowledge(ctx, args)
		if err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Errorf("%v: %v", args, err)
		}
	}
	if len(f.kProposed) != 1 {
		t.Errorf("proposed on bad args: %d", len(f.kProposed))
	}
}

func TestKnowledgeSearch(t *testing.T) {
	f := &fakeActions{}
	e, out, _ := actionsEnv(f)
	ctx := context.Background()
	if err := e.knowledge(ctx, []string{"search", "telegram"}); err != nil || out.String() != "no matching knowledge\n" {
		t.Fatalf("%q %v", out.String(), err)
	}
	f.kEntries = []api.Knowledge{{ID: 4, Kind: "decision", Scope: "global", Text: "use telegram"}}
	out.Reset()
	if err := e.knowledge(ctx, []string{"search", "telegram", "--scope", "global"}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "#4 [decision] (global) use telegram\n" || f.kList[len(f.kList)-1] != "search:telegram|global" {
		t.Errorf("%q %v", out.String(), f.kList)
	}
	if err := e.knowledge(ctx, []string{"search"}); err == nil {
		t.Error("no query accepted")
	}
}
