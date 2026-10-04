package client

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/abdallah/session-hub/internal/api"
)

// InstructionsFile is the local copy of the standing rules, in the state
// dir. The watcher and `sessionhub hook refresh-instructions` write it; `sessionhub hook
// context` reads it.
const InstructionsFile = "instructions.json"

// InstructionsPath is the local copy's path in stateDir.
func InstructionsPath(stateDir string) string { return filepath.Join(stateDir, InstructionsFile) }

// LoadInstructions reads the local copy. A missing file returns an error
// that matches os.ErrNotExist.
func LoadInstructions(path string) (api.InstructionList, error) {
	var list api.InstructionList
	b, err := os.ReadFile(path)
	if err != nil {
		return list, err
	}
	err = json.Unmarshal(b, &list)
	return list, err
}

// SaveInstructions writes the local copy with mode 0600 through a temp file
// and a rename, so a reader never sees half a file.
func SaveInstructions(path string, list api.InstructionList) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".instructions-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// RefreshInstructions reads the rules from the server and rewrites the
// local copy when its version differs or it cannot be read.
func RefreshInstructions(ctx context.Context, c *Client, path string) (bool, error) {
	list, err := c.Instructions(ctx)
	if err != nil {
		return false, err
	}
	if cur, err := LoadInstructions(path); err == nil && cur.Version == list.Version {
		return false, nil
	}
	return true, SaveInstructions(path, list)
}
