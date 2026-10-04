// Package join implements `sessionhub join`: get a per-machine token from the server
// host, write the client config, and install every client that fits.
package join

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/cli"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/hooks"
	"github.com/abdallah/session-hub/internal/mcp"
	"github.com/abdallah/session-hub/internal/paths"
	"github.com/abdallah/session-hub/internal/plugin"
	"github.com/abdallah/session-hub/internal/server"
	"github.com/abdallah/session-hub/internal/store"
)

const usage = `usage: sessionhub join <server-ssh-target> [--name N] [--ssh-host H] [--herdr-host H]`

// MinHerdr is the oldest herdr release that supports the sessionhub plugin.
var MinHerdr = [3]int{0, 9, 3}

// Deps holds everything Run calls out to, so tests can replace it.
type Deps struct {
	Stdout        io.Writer
	InstallPlugin func(ctx context.Context, args []string) error
	InstallHooks  func(ctx context.Context, args []string) error
	InstallMCP    func(ctx context.Context, args []string) error
	Status        func(ctx context.Context, args []string) error
	// MachineAddLocal runs `sessionhub machine add` in-process and returns its stdout.
	MachineAddLocal func(ctx context.Context, args []string) ([]byte, error)
	// Hostname returns the local host name.
	Hostname func() (string, error)
}

// DefaultDeps wires Run to the real installers.
func DefaultDeps() Deps {
	return Deps{
		Stdout:          os.Stdout,
		InstallPlugin:   plugin.RunInstall,
		InstallHooks:    hooks.RunInstall,
		InstallMCP:      mcp.RunInstall,
		Status:          cli.RunStatus,
		MachineAddLocal: machineAddLocal,
		Hostname:        os.Hostname,
	}
}

// claudeSnippet is the text to paste into ~/.claude/CLAUDE.md. It is a copy
// of docs/CLAUDE-snippet.md, because go:embed cannot reach outside this
// package; TestSnippetMatchesDocs keeps the two identical.
//
//go:embed CLAUDE-snippet.md
var claudeSnippet string

// Run is `sessionhub join`.
func Run(ctx context.Context, args []string) error {
	return RunWith(ctx, args, DefaultDeps())
}

type options struct {
	target, name, sshHost, herdrHost string
}

func parseArgs(args []string) (options, error) {
	fs := flag.NewFlagSet("sessionhub join", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o options
	fs.StringVar(&o.name, "name", "", "machine name (default: short hostname)")
	fs.StringVar(&o.sshHost, "ssh-host", "", "host others use to ssh to this machine")
	fs.StringVar(&o.herdrHost, "herdr-host", "", "host others use for herdr remote attach")
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return o, fmt.Errorf("join: %v\n%s", err, usage)
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) != 1 {
		return o, fmt.Errorf("join: want one server ssh target, got %d\n%s", len(pos), usage)
	}
	o.target = pos[0]
	return o, nil
}

func validHost(h string) bool { return store.ValidHost(h) && !strings.HasPrefix(h, "-") }

// RunWith is Run with explicit dependencies.
func RunWith(ctx context.Context, args []string, d Deps) error {
	o, err := parseArgs(args)
	if err != nil {
		return err
	}
	if o.name == "" {
		h, err := d.Hostname()
		if err != nil {
			return fmt.Errorf("join: read hostname: %w (pass --name)", err)
		}
		o.name = strings.ToLower(strings.SplitN(h, ".", 2)[0])
	}
	// The remote side is a shell, so validate everything before sending it.
	if !store.ValidMachineName(o.name) {
		return fmt.Errorf("join: invalid machine name %q: use letters, digits, '.', '_', '-' (max 64)", o.name)
	}
	if !validHost(o.target) {
		return fmt.Errorf("join: invalid ssh target %q", o.target)
	}
	if o.sshHost != "" && !validHost(o.sshHost) {
		return fmt.Errorf("join: invalid --ssh-host %q", o.sshHost)
	}
	if o.herdrHost != "" && !validHost(o.herdrHost) {
		return fmt.Errorf("join: invalid --herdr-host %q", o.herdrHost)
	}

	out := d.Stdout
	addArgs := []string{"add", o.name}
	if o.sshHost != "" {
		addArgs = append(addArgs, "--ssh-host", o.sshHost)
	}
	if o.herdrHost != "" {
		addArgs = append(addArgs, "--herdr-host", o.herdrHost)
	}
	addArgs = append(addArgs, "--json")

	var raw []byte
	if localServer() {
		fmt.Fprintf(out, "Server config and database found here: adding machine %q locally.\n", o.name)
		fmt.Fprintf(out, "The ssh target %q is ignored.\n", o.target)
		raw, err = d.MachineAddLocal(ctx, addArgs)
		if err != nil {
			return fmt.Errorf("join: local machine add: %w", err)
		}
	} else {
		fmt.Fprintf(out, "Adding machine %q on %s over ssh.\n", o.name, o.target)
		raw, err = sshMachineAdd(ctx, o.target, addArgs)
		if err != nil {
			return err
		}
	}
	res, err := parseAdd(raw, o.name)
	if err != nil {
		return err
	}

	cfg := client.Config{ServerURL: res.ServerURL, Token: res.Token, Machine: o.name}
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("join: write client config: %w", err)
	}
	fmt.Fprintf(out, "Wrote %s (mode 0600).\n", paths.ClientConfig())

	var failed []string
	step := func(name string, err error) {
		if err != nil {
			fmt.Fprintf(out, "FAILED  %s: %v\n", name, err)
			failed = append(failed, name)
		}
	}

	// herdr plugin.
	switch ver, err := herdrVersion(ctx); {
	case errors.Is(err, exec.ErrNotFound):
		fmt.Fprintln(out, "Skipped install-plugin: herdr is not on PATH.")
	case err != nil:
		fmt.Fprintf(out, "Skipped install-plugin: cannot read the herdr version: %v\n", err)
	case !atLeast(ver, MinHerdr):
		fmt.Fprintf(out, "Skipped install-plugin: herdr %d.%d.%d is older than %d.%d.%d.\n",
			ver[0], ver[1], ver[2], MinHerdr[0], MinHerdr[1], MinHerdr[2])
	default:
		fmt.Fprintln(out, "Installing the herdr plugin.")
		step("install-plugin", d.InstallPlugin(ctx, nil))
	}

	// Claude Code hooks.
	if _, err := os.Stat(paths.Binary()); err != nil {
		fmt.Fprintf(out, "Warning: %s does not exist yet; hooks and the MCP server call that path. Run `make install`.\n", paths.Binary())
	}
	fmt.Fprintln(out, "Installing the Claude Code hooks.")
	step("install-hooks", d.InstallHooks(ctx, nil))

	// MCP server.
	if _, err := exec.LookPath("claude"); err != nil {
		fmt.Fprintln(out, "Skipped install-mcp: claude is not on PATH.")
	} else {
		fmt.Fprintln(out, "Registering the MCP server with Claude Code.")
		step("install-mcp", d.InstallMCP(ctx, nil))
	}

	// Health.
	if c, err := client.New(cfg); err != nil {
		step("health", err)
	} else {
		hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := c.Health(hctx)
		cancel()
		if err == nil {
			fmt.Fprintf(out, "Health check passed: %s is reachable.\n", cfg.ServerURL)
		}
		step("health", err)
	}

	// Status is informational: a failure here does not fail the join.
	fmt.Fprintln(out, "\nsessionhub status:")
	if err := d.Status(ctx, nil); err != nil {
		fmt.Fprintf(out, "Could not print status: %v\n", err)
	}

	printSnippets(out)
	if len(failed) > 0 {
		return fmt.Errorf("join: enrolled %s, but these steps failed: %s", o.name, strings.Join(failed, ", "))
	}
	fmt.Fprintf(out, "\nJoined as %s.\n", o.name)
	return nil
}

func printSnippets(w io.Writer) {
	fmt.Fprint(w, `
Two manual steps remain. sessionhub does not edit these files.

1. Paste the following into ~/.claude/CLAUDE.md so Claude sessions call
   report_progress and set_title. The same text is in docs/CLAUDE-snippet.md
   in the sessionhub repository.

--- begin CLAUDE.md snippet ---
`)
	fmt.Fprint(w, claudeSnippet)
	if !strings.HasSuffix(claudeSnippet, "\n") {
		fmt.Fprintln(w)
	}
	fmt.Fprint(w, `--- end CLAUDE.md snippet ---

2. To show each session's summary in the herdr sidebar, add a row to
   ~/.config/herdr/config.toml:

   [ui.sidebar.agents]
   rows = [["state_icon", "workspace", "tab"], ["$hub_summary"], ["agent"]]
`)
}

// localServer reports whether this machine hosts the server: a server config
// file and the database both exist.
func localServer() bool {
	for _, p := range []string{paths.ServerConfig(), paths.DB()} {
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			return false
		}
	}
	return true
}

func machineAddLocal(ctx context.Context, args []string) ([]byte, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- b
	}()
	runErr := server.RunMachine(ctx, args)
	os.Stdout = orig
	w.Close()
	b := <-done
	r.Close()
	return b, runErr
}

func sshMachineAdd(ctx context.Context, target string, addArgs []string) ([]byte, error) {
	// Every argument was validated against the server's name and host rules
	// and contains no shell metacharacters, so joining is safe.
	remote := "~/.local/bin/sessionhub machine " + strings.Join(addArgs, " ")
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "--", target, remote)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("join: ssh %s failed: %v: %s (ssh runs with BatchMode=yes, so it needs key-based login, and sessionhub must be installed at ~/.local/bin/sessionhub on the server host)",
			target, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func parseAdd(raw []byte, name string) (server.AddOutput, error) {
	var res server.AddOutput
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	// Login banners can precede the JSON line; take the last JSON object.
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "{") {
			if err := json.Unmarshal([]byte(l), &res); err != nil {
				return res, fmt.Errorf("join: parse machine add output: %w", err)
			}
			if res.Token == "" || res.ServerURL == "" || res.Name != name {
				return res, errors.New("join: machine add output is missing name, token, or server_url")
			}
			return res, nil
		}
	}
	return res, fmt.Errorf("join: machine add printed no JSON line: %q", strings.TrimSpace(string(raw)))
}

var herdrVerRE = regexp.MustCompile(`herdr (\d+)\.(\d+)\.(\d+)`)

func herdrVersion(ctx context.Context) ([3]int, error) {
	var v [3]int
	path, err := exec.LookPath("herdr")
	if err != nil {
		return v, err
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, path, "--version").Output()
	if err != nil {
		return v, err
	}
	m := herdrVerRE.FindStringSubmatch(string(out))
	if m == nil {
		return v, fmt.Errorf("unexpected output %q", strings.TrimSpace(string(out)))
	}
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, nil
}

func atLeast(v, min [3]int) bool {
	for i := range v {
		if v[i] != min[i] {
			return v[i] > min[i]
		}
	}
	return true
}
