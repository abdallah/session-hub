package resume

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os/exec"
	"strings"
	"time"
)

// SavedMachine is one profile from `herdr machine list --json`.
type SavedMachine struct {
	ID        string
	Label     string
	SSHTarget string
}

// savedTimeout bounds `herdr machine list`; a hang means "no saved machines".
const savedTimeout = 2 * time.Second

// listSavedMachines runs `herdr machine list --json`. Any failure (herdr
// missing, timeout, unexpected output) returns nil.
func listSavedMachines(ctx context.Context) []SavedMachine {
	ctx, cancel := context.WithTimeout(ctx, savedTimeout)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "herdr", "machine", "list", "--json")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil
	}
	return parseSavedMachines(out.Bytes())
}

// parseSavedMachines decodes a bare array or an object holding a "machines"
// array. It reads the label from "label" (or "name") and the SSH target from
// "ssh_target" (or "target"), and returns nil for anything else. herdr 0.9.3
// printed "[]" on the machine that captured the fixtures, so the field names
// are unverified against a real profile (see testdata/README.md).
func parseSavedMachines(data []byte) []SavedMachine {
	type entry struct {
		ID        string `json:"id"`
		Label     string `json:"label"`
		Name      string `json:"name"`
		SSHTarget string `json:"ssh_target"`
		Target    string `json:"target"`
		Enabled   *bool  `json:"enabled"`
	}
	var list []entry
	if json.Unmarshal(data, &list) != nil {
		var wrap struct {
			Machines []entry `json:"machines"`
		}
		if json.Unmarshal(data, &wrap) != nil {
			return nil
		}
		list = wrap.Machines
	}
	var out []SavedMachine
	for _, m := range list {
		if m.Enabled != nil && !*m.Enabled {
			continue // herdr refuses a disabled profile as a --machine selector
		}
		sm := SavedMachine{ID: m.ID, Label: m.Label, SSHTarget: m.SSHTarget}
		if sm.Label == "" {
			sm.Label = m.Name
		}
		if sm.SSHTarget == "" {
			sm.SSHTarget = m.Target
		}
		out = append(out, sm)
	}
	return out
}

// hostOf strips a scheme, a "user@" prefix, and a port from an SSH target.
func hostOf(target string) string {
	t := strings.TrimSpace(target)
	t = strings.TrimPrefix(t, "ssh://")
	if i := strings.LastIndex(t, "@"); i >= 0 {
		t = t[i+1:]
	}
	if i := strings.IndexByte(t, '/'); i >= 0 {
		t = t[:i]
	}
	if h, _, err := net.SplitHostPort(t); err == nil {
		return h
	}
	return strings.Trim(t, "[]")
}

// matchSaved returns the label of the saved machine whose SSH target host
// equals herdrHost, or "".
func matchSaved(saved []SavedMachine, herdrHost string) string {
	want := strings.ToLower(hostOf(herdrHost))
	if want == "" {
		return ""
	}
	for _, m := range saved {
		if strings.ToLower(hostOf(m.SSHTarget)) == want && m.Label != "" {
			return m.Label
		}
	}
	return ""
}

// openPicker runs `herdr plugin pane open` for the sessionhub plugin's resume-picker
// overlay.
func openPicker(ctx context.Context, bin string, stdout, stderr io.Writer) error {
	return openPane(ctx, bin, stdout, stderr, "resume-picker", "--placement", "overlay")
}

// openInbox opens the inbox pane (sessionhub inbox --watch) to the right of the
// focused pane.
func openInbox(ctx context.Context, bin string, stdout, stderr io.Writer) error {
	return openPane(ctx, bin, stdout, stderr, "inbox", "--placement", "split", "--direction", "right")
}

// openPane runs `herdr plugin pane open` for one of the sessionhub plugin's panes,
// focused. herdr answers with a JSON line; a successful open prints nothing,
// and a failed one writes herdr's output to stderr.
func openPane(ctx context.Context, bin string, stdout, stderr io.Writer, entrypoint string, placement ...string) error {
	args := append([]string{"plugin", "pane", "open", "--plugin", "sessionhub", "--entrypoint", entrypoint}, placement...)
	cmd := exec.CommandContext(ctx, bin, append(args, "--focus")...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		stderr.Write(out)
	}
	return err
}
