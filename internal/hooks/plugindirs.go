package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/paths"
)

// The settings edits `sessionhub install-mod` shares with `sessionhub install-hooks`: the
// same byte-preserving JSON edit, backup, and atomic write.

// PluginDirsKey is the env variable in settings.json that lists the extra
// plugin directories Claude Code loads, separated by ':'.
const PluginDirsKey = "CLAUDE_CODE_PLUGIN_DIRS"

// SettingsPath is $CLAUDE_CONFIG_DIR/settings.json, else
// ~/.claude/settings.json.
func SettingsPath() string { return settingsPath() }

// EditSettings applies edit to the settings file at path. It writes only when
// the bytes change, backs up an existing file first, and replaces the file
// with a rename. It returns whether it wrote and the backup's path ("" when
// there was no file before).
func EditSettings(path string, now time.Time, edit func([]byte) ([]byte, error)) (changed bool, backup string, err error) {
	return editFile(installOpts{settings: path}, now, edit)
}

// AddPluginDir adds dir to env.CLAUDE_CODE_PLUGIN_DIRS in the settings JSON
// src, keeping the other ':'-separated entries and every other byte of the
// file. It returns src unchanged when dir is already listed.
func AddPluginDir(src []byte, dir string) ([]byte, error) {
	if len(bytes.TrimSpace(src)) == 0 {
		src = []byte("{}\n")
	}
	ms, open, closeAt, err := parseObject(src, 0)
	if err != nil {
		return nil, err
	}
	unit := indentOf(src, ms)
	envM, ok := find(ms, "env")
	if !ok {
		inner := []byte(unit + unit + jsonKey(PluginDirsKey) + ": " + string(jsonString(dir)))
		val := []byte("{\n" + string(inner) + "\n" + unit + "}")
		return insertMember(src, ms, open, closeAt, unit, "", "env", val), nil
	}
	if src[envM.valStart] != '{' {
		return nil, errors.New(`"env" is not an object`)
	}
	ems, eopen, eclose, err := parseObject(src, envM.valStart)
	if err != nil {
		return nil, fmt.Errorf("env: %w", err)
	}
	keyIndent := lineIndent(src, envM.keyStart)
	dirsM, ok := find(ems, PluginDirsKey)
	if !ok {
		inner := keyIndent + unit
		if len(ems) > 0 {
			inner = indentOf(src, ems)
		}
		return insertMember(src, ems, eopen, eclose, inner, keyIndent, PluginDirsKey, jsonString(dir)), nil
	}
	entries, err := pluginDirs(src, dirsM)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e != "" && filepath.Clean(e) == filepath.Clean(dir) {
			return src, nil
		}
	}
	var keep []string
	for _, e := range entries {
		if e != "" {
			keep = append(keep, e)
		}
	}
	return splice(src, dirsM.valStart, dirsM.valEnd, jsonString(strings.Join(append(keep, dir), ":"))), nil
}

// RemovePluginDir removes dir from env.CLAUDE_CODE_PLUGIN_DIRS in the
// settings JSON src and keeps the other entries. When dir was the only entry
// it removes the key, and then "env" too if nothing else is left in it. It
// returns src unchanged when dir is not listed.
func RemovePluginDir(src []byte, dir string) ([]byte, error) {
	if len(bytes.TrimSpace(src)) == 0 {
		return src, nil
	}
	ms, open, closeAt, err := parseObject(src, 0)
	if err != nil {
		return nil, err
	}
	envM, ok := find(ms, "env")
	if !ok || src[envM.valStart] != '{' {
		return src, nil
	}
	ems, eopen, eclose, err := parseObject(src, envM.valStart)
	if err != nil {
		return nil, fmt.Errorf("env: %w", err)
	}
	dirsM, ok := find(ems, PluginDirsKey)
	if !ok {
		return src, nil
	}
	entries, err := pluginDirs(src, dirsM)
	if err != nil {
		return nil, err
	}
	var keep []string
	removed := false
	for _, e := range entries {
		if e != "" && filepath.Clean(e) == filepath.Clean(dir) {
			removed = true
			continue
		}
		keep = append(keep, e)
	}
	if !removed {
		return src, nil
	}
	if strings.Join(keep, "") != "" {
		return splice(src, dirsM.valStart, dirsM.valEnd, jsonString(strings.Join(keep, ":"))), nil
	}
	if len(ems) > 1 {
		return removeMember(src, ems, eopen, eclose, PluginDirsKey), nil
	}
	return removeMember(src, ms, open, closeAt, "env"), nil
}

// pluginDirs reads the CLAUDE_CODE_PLUGIN_DIRS value as its ':'-separated
// entries.
func pluginDirs(src []byte, m member) ([]string, error) {
	var v string
	if err := json.Unmarshal(src[m.valStart:m.valEnd], &v); err != nil {
		return nil, fmt.Errorf("env.%s is not a string", PluginDirsKey)
	}
	if v == "" {
		return nil, nil
	}
	return strings.Split(v, ":"), nil
}

// insertMember adds `"key": val` as the last member of the object whose
// braces are at open and closeAt. indent is the new member's indent, and
// closeIndent the closing brace's when the object was empty.
func insertMember(src []byte, ms []member, open, closeAt int, indent, closeIndent, key string, val []byte) []byte {
	m := []byte(indent + jsonKey(key) + ": " + string(val))
	if len(ms) == 0 {
		out := append([]byte{}, src[:open+1]...)
		out = append(out, '\n')
		out = append(out, m...)
		out = append(out, '\n')
		out = append(out, closeIndent...)
		return append(out, src[closeAt:]...)
	}
	last := ms[len(ms)-1].valEnd
	out := append([]byte{}, src[:last]...)
	out = append(out, ',', '\n')
	out = append(out, m...)
	return append(out, src[last:]...)
}

// removeMember deletes member key from the object whose braces are at open
// and closeAt, with its comma, and leaves the other members' bytes alone.
func removeMember(src []byte, ms []member, open, closeAt int, key string) []byte {
	i := -1
	for j, m := range ms {
		if m.key == key {
			i = j
		}
	}
	switch {
	case i < 0:
		return src
	case len(ms) == 1:
		return splice(src, open+1, closeAt, nil)
	case i < len(ms)-1:
		// Up to the next key: its indent then sits where this key's was.
		return splice(src, ms[i].keyStart, ms[i+1].keyStart, nil)
	default:
		// The last member: from the end of the one before it.
		return splice(src, ms[i-1].valEnd, ms[i].valEnd, nil)
	}
}

// lineIndent is the whitespace before offset i on its line, or "" when
// something other than whitespace comes first.
func lineIndent(src []byte, i int) string {
	j := bytes.LastIndexByte(src[:i], '\n')
	ws := src[j+1 : i]
	if len(bytes.TrimLeft(ws, " \t")) != 0 {
		return ""
	}
	return string(ws)
}

func jsonKey(k string) string { return string(jsonString(k)) }

// jsonString is the JSON string for s, without HTML escaping.
func jsonString(s string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimRight(b.Bytes(), "\n")
}

// modInstalled reports whether `sessionhub install-mod` has written the mod.
func modInstalled() bool {
	_, err := os.Stat(filepath.Join(paths.ModDir(), ".claude-plugin", "plugin.json"))
	return err == nil
}
