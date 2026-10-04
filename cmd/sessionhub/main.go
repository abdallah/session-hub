// Command sessionhub is the single binary for the sessionhub server and its clients.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/abdallah/session-hub/internal/cli"
	"github.com/abdallah/session-hub/internal/digest"
	"github.com/abdallah/session-hub/internal/hooks"
	"github.com/abdallah/session-hub/internal/join"
	"github.com/abdallah/session-hub/internal/mcp"
	"github.com/abdallah/session-hub/internal/plugin"
	"github.com/abdallah/session-hub/internal/resume"
	"github.com/abdallah/session-hub/internal/server"
)

// version is set with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: sessionhub <command> [args]

commands:
  server                        run the sessionhub server
  machine add|rm|ls             manage machines (server side)
  plugin startup|event|watch    herdr plugin entry points
  plugin open-picker            open the session picker
  plugin open-inbox             open the inbox pane
  install-plugin                install the herdr plugin
  uninstall-plugin              remove the herdr plugin
  hook <event>                  Claude Code hook handler (always exits 0)
  install-hooks                 add sessionhub hooks to Claude Code settings
  uninstall-hooks               remove sessionhub hooks
  mcp                           run the MCP server
  install-mcp                   register the MCP server with Claude Code
  uninstall-mcp                 remove the MCP registration
  ls                            list sessions
  show <id>                     show one session
  status                        show sessionhub connection status
  login --name <device>         sign a browser in to the dashboard
  login ls | rm <name|id>       list or sign out browsers
  inbox [--json|--watch]        list the sessions that need you
  inbox dismiss|snooze <id>     dismiss or snooze an inbox item
  rules [ls|add <text>|rm <id>] list or edit the standing rules
  send <id>... -m <text>        send a message to sessions (or --machine M)
  approve [--yes] <id>          allow a pending permission prompt once
  deny [--yes] <id> [reason]    deny a pending permission prompt
  move <id> <machine|cloud>     move a session to another machine or the cloud
  move --status <move-id>       show a move
  move-key                      print this machine's move key fingerprint
  start <machine> [--dir D] [-m T]  start a new session with Remote Control on
  digest <id> | --all           send session digests to the sessionhub
  resume [--remote-control] <id>  resume a session on this machine
  remote-control <id>           turn on Remote Control for a running session
  join                          enroll this machine
  version                       print the version
  help                          print this help
`

type handler func(ctx context.Context, args []string) error

// routes maps a command (or "plugin open-picker" and "plugin open-inbox") to its handler. Tests
// substitute their own table.
var routes = map[string]handler{
	"server":             server.Run,
	"machine":            server.RunMachine,
	"plugin":             plugin.Run,
	"plugin open-picker": resume.RunOpenPicker,
	"plugin open-inbox":  resume.RunOpenInbox,
	"install-plugin":     plugin.RunInstall,
	"uninstall-plugin":   plugin.RunUninstall,
	"hook":               hooks.Run,
	"install-hooks":      hooks.RunInstall,
	"uninstall-hooks":    hooks.RunUninstall,
	"mcp":                mcp.Run,
	"install-mcp":        mcp.RunInstall,
	"uninstall-mcp":      mcp.RunUninstall,
	"ls":                 cli.RunLs,
	"show":               cli.RunShow,
	"status":             cli.RunStatus,
	"login":              cli.RunLogin,
	"inbox":              cli.RunInbox,
	"rules":              cli.RunRules,
	"send":               cli.RunSend,
	"approve":            cli.RunApprove,
	"deny":               cli.RunDeny,
	"move":               cli.RunMove,
	"move-key":           cli.RunMoveKey,
	"start":              cli.RunStart,
	"digest":             digest.Run,
	"resume":             resume.Run,
	"remote-control":     resume.RunRemoteControl,
	"join":               join.Run,
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], routes, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, routes map[string]handler, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return 0
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version":
		fmt.Fprintln(stdout, "sessionhub "+version)
		return 0
	}
	h, ok := routes[cmd]
	if cmd == "plugin" && len(rest) > 0 {
		if sub, found := routes["plugin "+rest[0]]; found {
			h, ok, rest = sub, true, rest[1:]
		}
	}
	if !ok {
		fmt.Fprintf(stderr, "sessionhub: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if cmd == "hook" {
		// Hooks must never fail Claude Code: report errors and panics, exit 0.
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(stderr, "hook: panic: %v\n", r)
			}
		}()
		if err := h(ctx, rest); err != nil {
			fmt.Fprintln(stderr, err)
		}
		return 0
	}
	if err := h(ctx, rest); err != nil {
		var ee *cli.ExitError
		if errors.As(err, &ee) {
			return ee.Code // the command already said what it needed to
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
