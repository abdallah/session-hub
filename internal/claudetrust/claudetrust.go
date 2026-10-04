// Package claudetrust reads and records Claude Code's folder trust.
//
// Claude asks "Is this a project you created or one you trust?" the first
// time it runs in a folder, and records the answer in its global config
// (~/.claude.json, or $CLAUDE_CONFIG_DIR/.claude.json) as
// projects["<absolute path>"].hasTrustDialogAccepted. A folder counts as
// trusted when it or any folder above it has that flag.
package claudetrust

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// trustKey is the per-project flag Claude sets when its trust prompt is
// accepted.
const trustKey = "hasTrustDialogAccepted"

// writeAttempts bounds how often Trust retries when Claude rewrites the
// config between Trust's read and its rename.
const writeAttempts = 5

// Path is the config file Claude reads trust from: $CLAUDE_CONFIG_DIR/.claude.json
// when that is set, else home/.claude.json.
func Path(getenv func(string) string, home string) string {
	if d := getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json")
	}
	return filepath.Join(home, ".claude.json")
}

// Trusted reports whether Claude trusts dir: dir or a folder above it has
// hasTrustDialogAccepted set in the config at path. A missing config trusts
// nothing. dir must be absolute.
func Trusted(path, dir string) (bool, error) {
	if !filepath.IsAbs(dir) {
		return false, fmt.Errorf("%q is not an absolute path", dir)
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var cfg struct {
		Projects map[string]map[string]json.RawMessage `json:"projects"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if accepted(cfg.Projects[d]) {
			return true, nil
		}
		if d == filepath.Dir(d) {
			return false, nil
		}
	}
}

func accepted(p map[string]json.RawMessage) bool {
	var v bool
	return p != nil && json.Unmarshal(p[trustKey], &v) == nil && v
}

// Trust marks exactly dir as trusted in the config at path, and nothing
// above it. dir must be absolute and clean, and must not be home or a
// folder above home: trusting those trusts every folder under them.
//
// Trust rewrites the file atomically and keeps every other key as it was.
// Claude rewrites the file while it runs; when the file changes between the
// read and the rename, Trust starts again. A missing file is created.
func Trust(path, dir, home string) error {
	if err := checkDir(dir, home); err != nil {
		return err
	}
	// Write through a symlinked config (dotfiles) rather than replacing
	// the link.
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	for range writeAttempts {
		done, err := trustOnce(path, dir)
		if done || err != nil {
			return err
		}
	}
	return fmt.Errorf("%s kept changing while sessionhub tried to update it; try again", path)
}

// checkDir refuses a relative or unclean dir, and home or anything above it.
func checkDir(dir, home string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return fmt.Errorf("%q: want an absolute path", dir)
	}
	if home == "" {
		return errors.New("no home directory")
	}
	// home is dir or below it when the way from dir to home doesn't go up.
	rel, err := filepath.Rel(dir, filepath.Clean(home))
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s is the home directory or above it; trusting it would trust every folder under it", dir)
	}
	return nil
}

// beforeRename runs in trustOnce after the temp file is written. Tests use
// it to change the config under Trust.
var beforeRename = func(path string) {}

// fileState is what Trust compares to tell whether the file changed.
type fileState struct {
	exists bool
	size   int64
	mod    time.Time
	mode   fs.FileMode
}

func stat(path string) (fileState, error) {
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fileState{mode: 0o600}, nil
	}
	if err != nil {
		return fileState{}, err
	}
	return fileState{exists: true, size: st.Size(), mod: st.ModTime(), mode: st.Mode().Perm()}, nil
}

// trustOnce does one read-modify-write. done is false when the file changed
// before the rename and nothing was written.
func trustOnce(path, dir string) (done bool, err error) {
	before, err := stat(path)
	if err != nil {
		return false, err
	}
	cfg := map[string]json.RawMessage{}
	if before.exists {
		b, err := os.ReadFile(path)
		if err != nil {
			return false, err
		}
		if len(bytes.TrimSpace(b)) > 0 {
			if err := json.Unmarshal(b, &cfg); err != nil {
				return false, fmt.Errorf("read %s: %w", path, err)
			}
		}
	}
	projects := map[string]json.RawMessage{}
	if raw, ok := cfg["projects"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &projects); err != nil {
			return false, fmt.Errorf("read %s: projects: %w", path, err)
		}
	}
	project := map[string]json.RawMessage{}
	if raw, ok := projects[dir]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &project); err != nil {
			return false, fmt.Errorf("read %s: projects[%q]: %w", path, dir, err)
		}
	}
	project[trustKey] = json.RawMessage("true")
	if projects[dir], err = json.Marshal(project); err != nil {
		return false, err
	}
	if cfg["projects"], err = json.Marshal(projects); err != nil {
		return false, err
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".claude.json.sessionhub-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(append(out, '\n'))
	if werr == nil {
		werr = tmp.Chmod(before.mode)
	}
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return false, werr
	}
	beforeRename(path)
	if now, err := stat(path); err != nil || now != before {
		return false, err
	}
	return true, os.Rename(tmp.Name(), path)
}
