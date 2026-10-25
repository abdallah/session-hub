package api

import "testing"

func TestStandupText(t *testing.T) {
	prev := &TaskDay{Date: "2026-10-23",
		Done:       []Task{{Title: "Fix CI token rotation", Ref: "systemsdev-1"}},
		InProgress: []Task{{Title: "Runners on private IP", ActiveSeconds: 600}, {Title: "No time that day"}}}
	today := TaskDay{Date: "2026-10-26",
		InProgress: []Task{{Title: "Runners on private IP", WaitingOn: []string{"Ops to open the port"}}},
		Todo:       []Task{{Title: "Netbird upgrade", Ref: "systemsdev-2"}},
		Done:       []Task{{Title: "systemsdev-3 RDS cleanup", Ref: "systemsdev-3"}},
		Ideas:      []Task{{Title: "an idea", Source: SourceIdea}}}
	want := `Yesterday (Fri 23 Oct):
- systemsdev-1: Fix CI token rotation (done)
- Runners on private IP (in progress)

Today:
- Runners on private IP
- systemsdev-2: Netbird upgrade
- systemsdev-3 RDS cleanup (done)

Blockers:
- Runners on private IP: waiting on Ops to open the port
`
	if got := StandupText(prev, today); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := StandupText(nil, TaskDay{}); got != "Yesterday:\n- none\n\nToday:\n- none\n\nBlockers:\n- none\n" {
		t.Errorf("empty standup:\n%s", got)
	}
}
