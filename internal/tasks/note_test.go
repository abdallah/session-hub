package tasks

import "testing"

// TestParseRealNotes runs the worklog notes of 2026-10-07 through Parse.
func TestParseRealNotes(t *testing.T) {
	for _, c := range []struct {
		text   string
		want   Note
		finish bool
	}{
		{"admit SSO role to reporting secrets", Note{Title: "admit SSO role to reporting secrets"}, false},
		{"investigate Bobby VPN 403 report", Note{Title: "investigate Bobby VPN 403 report"}, false},
		{"done: Bobby VPN 403 — client-side network drops", Note{Title: "Bobby VPN 403 — client-side network drops"}, true},
		{"design daily tasks feature", Note{Title: "design daily tasks feature"}, false},
		{"tasks schema v13 store", Note{Title: "tasks schema v13 store"}, false},
		{"close systemsdev-17845: rebase content!430", Note{Title: "rebase content!430", Ref: "systemsdev-17845", Kind: RefTicket}, true},
		{"investigate ATE E2E 1h CI timeout (systemsdev-17987)", Note{Title: "investigate ATE E2E 1h CI timeout", Ref: "systemsdev-17987", Kind: RefTicket}, false},
		{"done: ATE E2E timeout root cause found (ate-large runner 1h cap)", Note{Title: "ATE E2E timeout root cause found (ate-large runner 1h cap)"}, true},
		{"task 4 client task calls", Note{Title: "task 4 client task calls"}, false},
		{"review Amir's CI process proposal (systemsdev-17991)", Note{Title: "review Amir's CI process proposal", Ref: "systemsdev-17991", Kind: RefTicket}, false},
		{"sessionhub tasks dashboard tab", Note{Title: "sessionhub tasks dashboard tab"}, false},
		{"finished sessionhub tasks dashboard tab", Note{Title: "sessionhub tasks dashboard tab"}, true},
		{"fix tasks final review findings", Note{Title: "fix tasks final review findings"}, false},
		{"posted Systems review on systemsdev-17991", Note{Title: "posted Systems review on", Ref: "systemsdev-17991", Kind: RefTicket}, false},
		{"closed systemsdev-17845 (P81 to NetBird)", Note{Title: "P81 to NetBird", Ref: "systemsdev-17845", Kind: RefTicket}, true},
		{"fixed tasks final review findings", Note{Title: "fixed tasks final review findings"}, false},
		{"built daily tasks feature (feat/tasks)", Note{Title: "built daily tasks feature (feat/tasks)"}, false},
		{"drop retired QA/staging WP Aurora from tofu", Note{Title: "drop retired QA/staging WP Aurora from tofu"}, false},
		{"tasks review: collapse untasked, ignore all", Note{Title: "tasks review: collapse untasked, ignore all"}, false},
		{"done: tasks review collapse + ignore all", Note{Title: "tasks review collapse + ignore all"}, true},
		{"done: ATE runners moved to ate/gitlab-runner (zero-diff)", Note{Title: "ATE runners moved to ate/gitlab-runner (zero-diff)"}, true},
		{"MR !2218 up: QA/staging WP Aurora dropped from tofu", Note{Title: "up: QA/staging WP Aurora dropped from tofu", Ref: "!2218", Kind: RefMR}, false},
		{"18032 planned; paused for Raouf's reply", Note{Title: "planned; paused for Raouf's reply", Ref: "systemsdev-18032", Kind: RefTicket}, false},
		{"fix checkout-payments-silent false positive (16975)", Note{Title: "fix checkout-payments-silent false positive", Ref: "systemsdev-16975", Kind: RefTicket}, false},
		{"18032 built: draft MR !2225, dry runs pass", Note{Title: "built: draft MR !2225, dry runs pass", Ref: "systemsdev-18032", Kind: RefTicket}, false},
		{"done: checkout-payments-silent fix MR !2223", Note{Title: "checkout-payments-silent fix", Ref: "!2223", Kind: RefMR}, true},
		// Not from the day, but the edges of the rules.
		{"close look at the logs", Note{Title: "close look at the logs"}, false},
		{"closed-loop control tweaks", Note{Title: "closed-loop control tweaks"}, false},
		{"Done. Shipped it", Note{Title: "Shipped it"}, true},
		{"done", Note{}, true},
		{"bump port 18787 for scratch", Note{Title: "bump port 18787 for scratch"}, false},
		{"(123456) too long", Note{Title: "(123456) too long"}, false},
		{"Systemsdev-17993", Note{Title: "systemsdev-17993", Ref: "systemsdev-17993", Kind: RefTicket}, false},
		{"done:", Note{}, true},
		{"DONE: Ship it", Note{Title: "Ship it"}, true},
		{"donezo party", Note{Title: "donezo party"}, false},
		{"review !12 and systemsdev-5", Note{Title: "review !12 and", Ref: "systemsdev-5", Kind: RefTicket}, false},
	} {
		c.want.Finish = c.finish
		if got := Parse(c.text); got != c.want {
			t.Errorf("Parse(%q) = %+v, want %+v", c.text, got, c.want)
		}
	}
}

func TestParseCutsTitle(t *testing.T) {
	long := ""
	for range 250 {
		long += "é"
	}
	if got := []rune(Parse(long).Title); len(got) != 200 {
		t.Errorf("title is %d runes, want 200", len(got))
	}
}
