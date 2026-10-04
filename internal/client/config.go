// Package client holds what the plugin, hooks, MCP server, and CLI share: the
// client config, the HTTP client for the sessionhub server, and the offline queue.
package client

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/abdallah/session-hub/internal/paths"
)

// Config is the client config. File keys: server_url, token, machine,
// remote_permissions, remote_start, and move_roots. Env overrides: SESSIONHUB_SERVER_URL,
// SESSIONHUB_TOKEN, SESSIONHUB_MACHINE.
type Config struct {
	ServerURL string `toml:"server_url"`
	Token     string `toml:"token"`
	Machine   string `toml:"machine"`
	// RemotePermissions = false turns off remote permission answers on this
	// machine. Unset means on.
	RemotePermissions *bool `toml:"remote_permissions,omitempty"`
	// RemoteStart = false stops the watcher from starting new sessions on
	// this machine. Unset means on.
	RemoteStart *bool `toml:"remote_start,omitempty"`
	// MoveRoots are directories, besides ~/Code, where a moved session's
	// clone is searched for. Absolute or starting with ~/.
	MoveRoots []string `toml:"move_roots,omitempty"`
}

// RemotePermissionsOff reports whether this machine opted out of remote
// permission answers: SESSIONHUB_REMOTE_PERMISSIONS=off (any case) in the
// environment, or remote_permissions = false in the config.
func RemotePermissionsOff(cfg Config, getenv func(string) string) bool {
	if strings.EqualFold(getenv("SESSIONHUB_REMOTE_PERMISSIONS"), "off") {
		return true
	}
	return cfg.RemotePermissions != nil && !*cfg.RemotePermissions
}

// RemoteStartOff reports whether this machine opted out of starting new
// sessions on request: SESSIONHUB_REMOTE_START=off (any case) in the
// environment, or remote_start = false in the config.
func RemoteStartOff(cfg Config, getenv func(string) string) bool {
	if strings.EqualFold(getenv("SESSIONHUB_REMOTE_START"), "off") {
		return true
	}
	return cfg.RemoteStart != nil && !*cfg.RemoteStart
}

// LoadConfig reads the file at paths.ClientConfig() and applies env
// overrides. A missing file is not an error: it yields an empty config, so
// env-only setups work.
func LoadConfig() (Config, error) {
	var c Config
	data, err := os.ReadFile(paths.ClientConfig())
	switch {
	case err == nil:
		if _, err := toml.Decode(string(data), &c); err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", paths.ClientConfig(), err)
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return Config{}, err
	}
	if v := os.Getenv("SESSIONHUB_SERVER_URL"); v != "" {
		c.ServerURL = v
	}
	if v := os.Getenv("SESSIONHUB_TOKEN"); v != "" {
		c.Token = v
	}
	if v := os.Getenv("SESSIONHUB_MACHINE"); v != "" {
		c.Machine = v
	}
	return c, nil
}

// Save writes c to paths.ClientConfig() with mode 0600, creating parent
// directories. It writes a temp file and renames it, so a reader never sees a
// partial file. Env overrides are not saved: pass the file's own values.
func (c Config) Save() error {
	path := paths.ClientConfig()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
