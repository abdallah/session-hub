package knowledge

import (
	"reflect"
	"testing"
)

func TestNormalizeRepo(t *testing.T) {
	for in, want := range map[string]string{
		"git@gitlab.com:Group/Repo.git":            "gitlab.com/group/repo",
		"https://user@gitlab.com/group/repo":       "gitlab.com/group/repo",
		"ssh://git@gitlab.com:2222/group/repo.git": "gitlab.com/group/repo",
		"": "",
	} {
		if got := NormalizeRepo(in); got != want {
			t.Errorf("NormalizeRepo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeRef(t *testing.T) {
	for in, want := range map[string]string{
		"SystemsDev-17908": "systemsdev-17908",
		"Group/Project!34": "group/project!34",
		"!34":              "",
		"nonsense":         "",
		"":                 "",
	} {
		if got := NormalizeRef(in); got != want {
			t.Errorf("NormalizeRef(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseScope(t *testing.T) {
	ok := map[string][2]string{
		"global":                             {"global", ""},
		"repo:git@gitlab.com:Group/Repo.git": {"repo", "gitlab.com/group/repo"},
		"ticket:SystemsDev-17908":            {"ticket", "systemsdev-17908"},
		"machine:blu":                        {"machine", "blu"},
	}
	for in, want := range ok {
		k, v, err := ParseScope(in)
		if err != nil || k != want[0] || v != want[1] {
			t.Errorf("ParseScope(%q) = %q, %q, %v; want %v", in, k, v, err, want)
		}
	}
	for _, in := range []string{"repo:", "team:x", "", "ticket:!34", "machine:", "global:x"} {
		if _, _, err := ParseScope(in); err == nil {
			t.Errorf("ParseScope(%q) succeeded, want error", in)
		}
	}
}

func TestSessionRefAndRefInRepo(t *testing.T) {
	for in, want := range map[string]string{" !34 ": "!34", "SystemsDev-1": "systemsdev-1", "G/P!3": "g/p!3", "!x": ""} {
		if got := SessionRef(in); got != want {
			t.Errorf("SessionRef(%q) = %q, want %q", in, got, want)
		}
	}
	for _, c := range [][3]string{
		{"!34", "gitlab.com/group/project", "group/project!34"},
		{"!34", "", ""},
		{"systemsdev-1", "", "systemsdev-1"},
		{"g/p!3", "gitlab.com/other/repo", "g/p!3"},
	} {
		if got := RefInRepo(c[0], c[1]); got != c[2] {
			t.Errorf("RefInRepo(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestRefsInBranch(t *testing.T) {
	got := RefsInBranch("abdallah/systemsdev-17908-dr-plan")
	if !reflect.DeepEqual(got, []string{"systemsdev-17908"}) {
		t.Errorf("got %v", got)
	}
	if got := RefsInBranch("main"); got != nil {
		t.Errorf("main: got %v, want nil", got)
	}
	if got := RefsInBranch("SYSTEMSDEV-1/x-SystemsDev-1"); len(got) != 1 {
		t.Errorf("dedupe: got %v", got)
	}
}
