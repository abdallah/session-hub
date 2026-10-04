package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/paths"
)

// hookSpec is one row of the docs/dev/PLAN.md table.
type hookSpec struct {
	Event   string
	Matcher string // empty: the group has no matcher key
	Arg     string // `sessionhub hook <Arg>`
	Async   bool
	Timeout int // seconds, 0 for none
	// Context adds a second, synchronous entry to the same group:
	// `sessionhub hook context`, which prints the rules. An async hook's output
	// does not reach the prompt, and one group per event keeps
	// installEvents' rule of one sessionhub group.
	Context bool
}

// contextTimeout is the context entry's timeout, in seconds. It reads one
// local file.
const contextTimeout = 5

var hookTable = []hookSpec{
	{"SessionStart", "*", "session-start", true, 0, true},
	{"UserPromptSubmit", "", "prompt", true, 0, true},
	{"Stop", "", "stop", true, 0, false},
	{"Notification", "*", "notification", true, 0, false},
	{"SessionEnd", "*", "session-end", false, 2, false},
	// 660 s: the hook waits up to 10 minutes for an answer from sessionhub.
	{"PermissionRequest", "*", "permission-request", false, 660, false},
}

// settingsPath is $CLAUDE_CONFIG_DIR/settings.json, else ~/.claude/settings.json.
func settingsPath() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "settings.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".claude", "settings.json")
}

type installOpts struct {
	settings string
	binary   string
}

func parseOpts(name string, args []string) (installOpts, error) {
	o := installOpts{settings: settingsPath(), binary: paths.Binary()}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.settings, "settings", o.settings, "settings file to edit")
	fs.StringVar(&o.binary, "binary", o.binary, "absolute path of the sessionhub binary")
	if err := fs.Parse(args); err != nil {
		return o, fmt.Errorf("%s: %w", name, err)
	}
	if !filepath.IsAbs(o.binary) {
		return o, fmt.Errorf("%s: --binary must be an absolute path", name)
	}
	return o, nil
}

// RunInstall implements `sessionhub install-hooks [--settings FILE] [--binary PATH]`.
func RunInstall(ctx context.Context, args []string) error {
	o, err := parseOpts("install-hooks", args)
	if err != nil {
		return err
	}
	changed, backup, err := installFile(o, time.Now())
	if err != nil {
		return fmt.Errorf("install-hooks: %w", err)
	}
	switch {
	case !changed:
		fmt.Printf("sessionhub hooks are already installed in %s\n", o.settings)
	case backup != "":
		fmt.Printf("Installed sessionhub hooks in %s (backup: %s)\n", o.settings, backup)
	default:
		fmt.Printf("Installed sessionhub hooks in %s\n", o.settings)
	}
	return nil
}

// RunUninstall implements `sessionhub uninstall-hooks [--settings FILE] [--binary PATH]`.
func RunUninstall(ctx context.Context, args []string) error {
	o, err := parseOpts("uninstall-hooks", args)
	if err != nil {
		return err
	}
	changed, backup, err := uninstallFile(o, time.Now())
	if err != nil {
		return fmt.Errorf("uninstall-hooks: %w", err)
	}
	switch {
	case !changed:
		fmt.Printf("No sessionhub hooks found in %s\n", o.settings)
	default:
		fmt.Printf("Removed sessionhub hooks from %s (backup: %s)\n", o.settings, backup)
	}
	return nil
}

func installFile(o installOpts, now time.Time) (changed bool, backup string, err error) {
	return editFile(o, now, func(src []byte) ([]byte, error) { return installJSON(src, o.binary) })
}

func uninstallFile(o installOpts, now time.Time) (changed bool, backup string, err error) {
	return editFile(o, now, func(src []byte) ([]byte, error) { return uninstallJSON(src, o.binary) })
}

// editFile applies edit to the settings file. It writes only when the bytes
// change, and backs up an existing file first.
func editFile(o installOpts, now time.Time, edit func([]byte) ([]byte, error)) (bool, string, error) {
	path := o.settings
	if r, err := filepath.EvalSymlinks(path); err == nil {
		path = r // keep a symlinked dotfile a symlink
	}
	src, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, "", err
	}
	out, err := edit(src)
	if err != nil {
		return false, "", fmt.Errorf("%s: %w", path, err)
	}
	if bytes.Equal(out, src) {
		return false, "", nil
	}
	mode := os.FileMode(0o600)
	var backup string
	if exists {
		if st, err := os.Stat(path); err == nil {
			mode = st.Mode().Perm()
		}
		backup = path + ".sessionhub-backup-" + now.Format("20060102T150405")
		// Never overwrite an earlier backup made in the same second.
		for n := 1; ; n++ {
			f, err := os.OpenFile(backup, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err == nil {
				_, werr := f.Write(src)
				if cerr := f.Close(); werr == nil {
					werr = cerr
				}
				if werr != nil {
					return false, "", werr
				}
				break
			}
			if !errors.Is(err, os.ErrExist) {
				return false, "", err
			}
			backup = fmt.Sprintf("%s.sessionhub-backup-%s-%d", path, now.Format("20060102T150405"), n)
		}
	} else if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings-*.tmp")
	if err != nil {
		return false, "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return false, "", err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return false, "", err
	}
	if err := tmp.Close(); err != nil {
		return false, "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return false, "", err
	}
	return true, backup, nil
}

var safeWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// commandFor is the hook command: the quoted absolute path plus `hook <arg>`.
func commandFor(binary, arg string) string {
	return shellQuote(binary) + " hook " + arg
}

func shellQuote(s string) string {
	if safeWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// isHubCommand reports whether cmd starts with the sessionhub path + " hook ".
func isHubCommand(cmd, binary string) bool {
	return strings.HasPrefix(cmd, binary+" hook ") || strings.HasPrefix(cmd, shellQuote(binary)+" hook ")
}

type hookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
	Async   bool   `json:"async,omitempty"`
}

type matcherGroup struct {
	Matcher *string     `json:"matcher,omitempty"`
	Hooks   []hookEntry `json:"hooks"`
}

func (s hookSpec) group(binary string) matcherGroup {
	g := matcherGroup{Hooks: []hookEntry{{Type: "command", Command: commandFor(binary, s.Arg), Timeout: s.Timeout, Async: s.Async}}}
	if s.Context {
		g.Hooks = append(g.Hooks, hookEntry{Type: "command", Command: commandFor(binary, "context"), Timeout: contextTimeout})
	}
	if s.Matcher != "" {
		m := s.Matcher
		g.Matcher = &m
	}
	return g
}

// --- byte-preserving JSON object editing -------------------------------

// member is one key of a JSON object with the byte spans of its key and value.
type member struct {
	key              string
	keyStart         int // offset of the opening quote
	valStart, valEnd int
}

// parseObject lists the members of the object in src[start:end]. It returns
// the offset just after '{' and of the closing '}'.
func parseObject(src []byte, start int) (ms []member, open, closeAt int, err error) {
	open = bytes.IndexByte(src[start:], '{')
	if open < 0 {
		return nil, 0, 0, errors.New("not a JSON object")
	}
	open += start
	if len(bytes.TrimSpace(src[start:open])) != 0 {
		return nil, 0, 0, errors.New("not a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(src[open:]))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, 0, 0, errors.New("not a JSON object")
	}
	for dec.More() {
		before := int(dec.InputOffset())
		t, err := dec.Token()
		if err != nil {
			return nil, 0, 0, err
		}
		key, _ := t.(string)
		keyEnd := int(dec.InputOffset())
		// The key's opening quote is the first '"' after `before`.
		q := bytes.IndexByte(src[open+before:open+keyEnd], '"')
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, 0, 0, err
		}
		end := int(dec.InputOffset())
		ms = append(ms, member{key: key, keyStart: open + before + q, valStart: open + end - len(raw), valEnd: open + end})
	}
	if _, err := dec.Token(); err != nil {
		return nil, 0, 0, err
	}
	closeAt = open + int(dec.InputOffset()) - 1
	return ms, open, closeAt, nil
}

func find(ms []member, key string) (member, bool) {
	for _, m := range ms {
		if m.key == key {
			return m, true
		}
	}
	return member{}, false
}

// indentOf returns the whitespace unit of the document: the indentation of
// its first member line, else two spaces.
func indentOf(src []byte, ms []member) string {
	if len(ms) > 0 {
		i := ms[0].keyStart
		j := bytes.LastIndexByte(src[:i], '\n')
		if j >= 0 && len(bytes.TrimLeft(src[j+1:i], " \t")) == 0 && i > j+1 {
			return string(src[j+1 : i])
		}
	}
	return "  "
}

func installJSON(src []byte, binary string) ([]byte, error) {
	if len(bytes.TrimSpace(src)) == 0 {
		src = []byte("{}\n")
	}
	ms, open, closeAt, err := parseObject(src, 0)
	if err != nil {
		return nil, err
	}
	unit := indentOf(src, ms)
	hooksM, hasHooks := find(ms, "hooks")

	var hooksObj []byte
	if hasHooks {
		hooksObj, err = editHooksObject(src, hooksM, unit, func(events []evt) ([]evt, error) {
			return installEvents(events, binary, unit)
		})
		if err != nil {
			return nil, err
		}
		return splice(src, hooksM.valStart, hooksM.valEnd, hooksObj), nil
	}
	// No hooks key: build one and insert it after the last member.
	events, err := installEvents(nil, binary, unit)
	if err != nil {
		return nil, err
	}
	hooksObj = renderEvents(events, unit, unit)
	member := []byte(unit + `"hooks": ` + string(hooksObj))
	if len(ms) == 0 {
		out := append([]byte{}, src[:open+1]...)
		out = append(out, '\n')
		out = append(out, member...)
		out = append(out, '\n')
		return append(out, src[closeAt:]...), nil
	}
	last := ms[len(ms)-1].valEnd
	out := append([]byte{}, src[:last]...)
	out = append(out, ',', '\n')
	out = append(out, member...)
	return append(out, src[last:]...), nil
}

func uninstallJSON(src []byte, binary string) ([]byte, error) {
	if len(bytes.TrimSpace(src)) == 0 {
		return src, nil
	}
	ms, _, _, err := parseObject(src, 0)
	if err != nil {
		return nil, err
	}
	hooksM, ok := find(ms, "hooks")
	if !ok {
		return src, nil
	}
	unit := indentOf(src, ms)
	var removed bool
	hooksObj, err := editHooksObject(src, hooksM, unit, func(events []evt) ([]evt, error) {
		var out []evt
		for _, e := range events {
			groups, ch, err := stripHub(e.groups, binary)
			if err != nil {
				return nil, err
			}
			removed = removed || ch
			if len(groups) == 0 && ch {
				continue // the event held only sessionhub entries
			}
			if ch {
				e.groups, e.dirty = groups, true
			}
			out = append(out, e)
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	if !removed {
		return src, nil
	}
	return splice(src, hooksM.valStart, hooksM.valEnd, hooksObj), nil
}

func splice(src []byte, from, to int, repl []byte) []byte {
	out := make([]byte, 0, len(src)-(to-from)+len(repl))
	out = append(out, src[:from]...)
	out = append(out, repl...)
	return append(out, src[to:]...)
}

// evt is one event key inside "hooks", with its groups as raw JSON.
type evt struct {
	name   string
	raw    []byte // verbatim value, used unless dirty
	groups []json.RawMessage
	dirty  bool
}

// editHooksObject parses the "hooks" value, lets edit change the events, and
// renders it again. Untouched events keep their exact bytes.
func editHooksObject(src []byte, m member, unit string, edit func([]evt) ([]evt, error)) ([]byte, error) {
	val := src[m.valStart:m.valEnd]
	if len(val) == 0 || val[0] != '{' {
		return nil, errors.New(`"hooks" is not an object`)
	}
	ems, _, _, err := parseObject(val, 0)
	if err != nil {
		return nil, err
	}
	var events []evt
	for _, e := range ems {
		ev := evt{name: e.key, raw: val[e.valStart:e.valEnd]}
		if err := json.Unmarshal(ev.raw, &ev.groups); err != nil {
			return nil, fmt.Errorf("hooks.%s is not an array: %w", e.key, err)
		}
		events = append(events, ev)
	}
	events, err = edit(events)
	if err != nil {
		return nil, err
	}
	// If nothing is dirty, keep the exact original bytes.
	same := len(events) == len(ems)
	for _, e := range events {
		same = same && !e.dirty
	}
	if same {
		return val, nil
	}
	// The object sits at one indent level in, its members at two.
	return renderEvents(events, unit, unit), nil
}

// renderEvents writes the hooks object. base is the indent of the "hooks" key.
func renderEvents(events []evt, base, unit string) []byte {
	if len(events) == 0 {
		return []byte("{}")
	}
	var b bytes.Buffer
	b.WriteString("{\n")
	for i, e := range events {
		b.WriteString(base + unit)
		k, _ := json.Marshal(e.name)
		b.Write(k)
		b.WriteString(": ")
		if !e.dirty {
			b.Write(e.raw)
		} else {
			b.WriteString("[\n")
			for j, g := range e.groups {
				b.WriteString(base + unit + unit)
				b.Write(g)
				if j < len(e.groups)-1 {
					b.WriteByte(',')
				}
				b.WriteByte('\n')
			}
			b.WriteString(base + unit + "]")
		}
		if i < len(events)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString(base + "}")
	return b.Bytes()
}

// installEvents makes each sessionhub event carry exactly the wanted group. Groups
// that already match are kept in place, byte for byte.
func installEvents(events []evt, binary, unit string) ([]evt, error) {
	for _, spec := range hookTable {
		want := spec.group(binary)
		wantJSON, _ := json.Marshal(want)
		idx := -1
		for i := range events {
			if events[i].name == spec.Event {
				idx = i
			}
		}
		if idx < 0 {
			events = append(events, evt{name: spec.Event})
			idx = len(events) - 1
			events[idx].dirty = true
			events[idx].groups = []json.RawMessage{wantJSON}
			continue
		}
		e := &events[idx]
		groups, changed, err := stripHubExcept(e.groups, binary, wantJSON)
		if err != nil {
			return nil, err
		}
		kept := false
		for _, g := range groups {
			if isHubOnly(g, binary) {
				kept = true
			}
		}
		if !kept {
			groups = append(groups, wantJSON)
			changed = true
		}
		if changed {
			e.groups, e.dirty = groups, true
		}
	}
	return events, nil
}

// semEqual reports whether two JSON values are equal after decoding.
func semEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return bytes.Equal(xa, ya)
}

func decodeGroup(g json.RawMessage) (matcherGroup, map[string]json.RawMessage, bool) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(g, &raw) != nil {
		return matcherGroup{}, nil, false
	}
	var mg matcherGroup
	if json.Unmarshal(g, &mg) != nil {
		return matcherGroup{}, nil, false
	}
	return mg, raw, true
}

func isHubOnly(g json.RawMessage, binary string) bool {
	mg, _, ok := decodeGroup(g)
	if !ok || len(mg.Hooks) == 0 {
		return false
	}
	for _, h := range mg.Hooks {
		if !isHubCommand(h.Command, binary) {
			return false
		}
	}
	return true
}

// stripHubExcept removes sessionhub entries from groups, except the first sessionhub-only
// group that semantically equals keep (it stays byte-identical).
func stripHubExcept(groups []json.RawMessage, binary string, keep []byte) ([]json.RawMessage, bool, error) {
	var out []json.RawMessage
	changed, kept := false, false
	for _, g := range groups {
		if !kept && isHubOnly(g, binary) && semEqual(g, keep) {
			kept = true
			out = append(out, g)
			continue
		}
		ng, ch, drop, err := stripGroup(g, binary)
		if err != nil {
			return nil, false, err
		}
		if ch {
			changed = true
		}
		if !drop {
			out = append(out, ng)
		}
	}
	return out, changed, nil
}

func stripHub(groups []json.RawMessage, binary string) ([]json.RawMessage, bool, error) {
	return stripHubExcept(groups, binary, nil)
}

// stripGroup removes sessionhub entries from one matcher group. drop is true when
// the group is left with no hooks.
func stripGroup(g json.RawMessage, binary string) (out json.RawMessage, changed, drop bool, err error) {
	mg, raw, ok := decodeGroup(g)
	if !ok {
		return g, false, false, nil // not ours to judge; keep as is
	}
	var keep []json.RawMessage
	var rawHooks []json.RawMessage
	if err := json.Unmarshal(raw["hooks"], &rawHooks); err != nil {
		return g, false, false, nil
	}
	for i, h := range mg.Hooks {
		if i < len(rawHooks) && isHubCommand(h.Command, binary) {
			changed = true
			continue
		}
		if i < len(rawHooks) {
			keep = append(keep, rawHooks[i])
		}
	}
	if !changed {
		return g, false, false, nil
	}
	if len(keep) == 0 {
		return nil, true, true, nil
	}
	hb, _ := json.Marshal(keep)
	raw["hooks"] = hb
	nb, err := json.Marshal(raw)
	return nb, true, false, err
}
