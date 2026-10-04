package client

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestConfigSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deep", "dir", "config.toml")
	t.Setenv("SESSIONHUB_CONFIG", path)
	for _, k := range []string{"SESSIONHUB_SERVER_URL", "SESSIONHUB_TOKEN", "SESSIONHUB_MACHINE"} {
		t.Setenv(k, "")
	}
	// Missing file: empty config, no error.
	if c, err := LoadConfig(); err != nil || !reflect.DeepEqual(c, Config{}) {
		t.Fatalf("missing file: %+v, %v", c, err)
	}
	want := Config{ServerURL: "https://sessionhub.example", Token: "hub_m_abc", Machine: "bluebox"}
	if err := want.Save(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, err %v", st.Mode().Perm(), err)
	}
	got, err := LoadConfig()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
	// Overwrite keeps 0600 and leaves no temp files.
	want.Machine = "tower"
	if err := want.Save(); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode after overwrite = %v", st.Mode().Perm())
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".config-*")); len(m) != 0 {
		t.Errorf("temp files left: %v", m)
	}
}

func TestConfigEnvOverrides(t *testing.T) {
	t.Setenv("SESSIONHUB_CONFIG", filepath.Join(t.TempDir(), "c.toml"))
	(Config{ServerURL: "http://file", Token: "file-token", Machine: "file-m"}).Save()
	t.Setenv("SESSIONHUB_SERVER_URL", "http://env")
	t.Setenv("SESSIONHUB_TOKEN", "")
	t.Setenv("SESSIONHUB_MACHINE", "env-m")
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	want := Config{ServerURL: "http://env", Token: "file-token", Machine: "env-m"}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got %+v, want %+v", c, want)
	}
}

func TestConfigMalformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	os.WriteFile(path, []byte("server_url = \n[[["), 0o600)
	t.Setenv("SESSIONHUB_CONFIG", path)
	if _, err := LoadConfig(); err == nil {
		t.Fatal("want parse error")
	}
}
