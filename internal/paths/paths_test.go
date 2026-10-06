package paths

import (
	"path/filepath"
	"testing"
)

func TestPaths(t *testing.T) {
	const h = "/home/tester"
	tests := []struct {
		name string
		fn   func() string
		env  string
		def  string
	}{
		{"client config", ClientConfig, "SESSIONHUB_CONFIG", ".config/sessionhub/config.toml"},
		{"server config", ServerConfig, "SESSIONHUB_SERVER_CONFIG", ".config/sessionhub/server.toml"},
		{"state dir", StateDir, "SESSIONHUB_STATE_DIR", ".local/state/sessionhub"},
		{"database", DB, "SESSIONHUB_DB", ".local/share/sessionhub/sessionhub.db"},
		{"plugin dir", PluginDir, "", ".local/share/sessionhub/herdr-plugin"},
		{"mod dir", ModDir, "", ".local/share/sessionhub/claude-mod"},
		{"binary", Binary, "", ".local/bin/sessionhub"},
		{"herdr socket", HerdrSocket, "HERDR_SOCKET_PATH", ".config/herdr/herdr.sock"},
	}
	for _, tt := range tests {
		t.Run(tt.name+" default", func(t *testing.T) {
			t.Setenv("HOME", h)
			if tt.env != "" {
				t.Setenv(tt.env, "")
			}
			if got, want := tt.fn(), filepath.Join(h, tt.def); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
		if tt.env == "" {
			continue
		}
		t.Run(tt.name+" override", func(t *testing.T) {
			t.Setenv("HOME", h)
			t.Setenv(tt.env, "/custom/path")
			if got := tt.fn(); got != "/custom/path" {
				t.Errorf("got %q, want /custom/path", got)
			}
		})
	}
}
