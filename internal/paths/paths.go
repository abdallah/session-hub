// Package paths resolves every file location sessionhub uses, applying env overrides.
package paths

import (
	"os"
	"path/filepath"
)

func home() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "."
}

func override(env, def string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return def
}

// ClientConfig is the client config file. Override: SESSIONHUB_CONFIG.
func ClientConfig() string {
	return override("SESSIONHUB_CONFIG", filepath.Join(home(), ".config", "sessionhub", "config.toml"))
}

// ServerConfig is the server config file. Override: SESSIONHUB_SERVER_CONFIG.
func ServerConfig() string {
	return override("SESSIONHUB_SERVER_CONFIG", filepath.Join(home(), ".config", "sessionhub", "server.toml"))
}

// StateDir holds the queue, locks, and current/. Override: SESSIONHUB_STATE_DIR.
func StateDir() string {
	return override("SESSIONHUB_STATE_DIR", filepath.Join(home(), ".local", "state", "sessionhub"))
}

// DB is the server SQLite database. Override: SESSIONHUB_DB.
func DB() string {
	return override("SESSIONHUB_DB", filepath.Join(home(), ".local", "share", "sessionhub", "sessionhub.db"))
}

// PluginDir is where the herdr plugin is installed.
func PluginDir() string {
	return filepath.Join(home(), ".local", "share", "sessionhub", "herdr-plugin")
}

// Binary is the installed sessionhub binary used in manifests and hook entries.
func Binary() string {
	return filepath.Join(home(), ".local", "bin", "sessionhub")
}

// HerdrSocket is the herdr Unix socket. Override: HERDR_SOCKET_PATH.
func HerdrSocket() string {
	return override("HERDR_SOCKET_PATH", filepath.Join(home(), ".config", "herdr", "herdr.sock"))
}
