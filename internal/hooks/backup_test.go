package hooks

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBackupsDoNotCollide(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte("{}\n"), 0o600)
	o := installOpts{settings: p, binary: testBinary}
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	_, b1, err := installFile(o, now)
	if err != nil {
		t.Fatal(err)
	}
	_, b2, err := uninstallFile(o, now)
	if err != nil {
		t.Fatal(err)
	}
	if b1 == b2 {
		t.Fatalf("both backups are %s", b1)
	}
	if b, _ := os.ReadFile(b1); string(b) != "{}\n" {
		t.Errorf("first backup was overwritten: %q", b)
	}
}
