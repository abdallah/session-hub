package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/abdallah/session-hub/internal/cli"
)

var commands = []string{"server", "machine", "plugin", "install-plugin", "uninstall-plugin", "hook", "install-hooks",
	"uninstall-hooks", "install-mod", "uninstall-mod", "mod", "mcp", "install-mcp", "uninstall-mcp", "ls", "show", "status", "login", "inbox", "rules", "send",
	"approve", "deny", "move", "move-key", "resume", "remote-control", "join"}

// pluginSubs are the plugin subcommands with their own route.
var pluginSubs = []string{"plugin open-picker", "plugin open-inbox"}

// recorders returns a route table whose handlers record their key and args.
func recorders(calls *[]string, argv *[][]string) map[string]handler {
	m := map[string]handler{}
	for _, k := range append(commands, pluginSubs...) {
		k := k
		m[k] = func(_ context.Context, a []string) error {
			*calls = append(*calls, k)
			*argv = append(*argv, a)
			return nil
		}
	}
	return m
}

func runQuiet(args []string, r map[string]handler) (code int, out, errOut string) {
	var o, e bytes.Buffer
	code = run(context.Background(), args, r, &o, &e)
	return code, o.String(), e.String()
}

func TestRoutesCoverEveryCommand(t *testing.T) {
	for _, c := range append(commands, pluginSubs...) {
		if routes[c] == nil {
			t.Errorf("real route table missing %q", c)
		}
	}
}

func TestDispatchRoutes(t *testing.T) {
	tests := []struct {
		args     []string
		wantCall string
		wantArgs []string
	}{
		{[]string{"server"}, "server", []string{}},
		{[]string{"machine", "add", "x"}, "machine", []string{"add", "x"}},
		{[]string{"plugin", "startup"}, "plugin", []string{"startup"}},
		{[]string{"plugin", "open-picker", "a"}, "plugin open-picker", []string{"a"}},
		{[]string{"plugin", "open-inbox"}, "plugin open-inbox", []string{}},
		{[]string{"install-plugin"}, "install-plugin", []string{}},
		{[]string{"uninstall-plugin"}, "uninstall-plugin", []string{}},
		{[]string{"hook", "stop"}, "hook", []string{"stop"}},
		{[]string{"install-hooks"}, "install-hooks", []string{}},
		{[]string{"uninstall-hooks"}, "uninstall-hooks", []string{}},
		{[]string{"install-mod"}, "install-mod", []string{}},
		{[]string{"uninstall-mod"}, "uninstall-mod", []string{}},
		{[]string{"mod", "poll", "--session", "s1"}, "mod", []string{"poll", "--session", "s1"}},
		{[]string{"mcp"}, "mcp", []string{}},
		{[]string{"install-mcp"}, "install-mcp", []string{}},
		{[]string{"uninstall-mcp"}, "uninstall-mcp", []string{}},
		{[]string{"ls"}, "ls", []string{}},
		{[]string{"show", "id"}, "show", []string{"id"}},
		{[]string{"status"}, "status", []string{}},
		{[]string{"inbox", "snooze", "abcd", "1h"}, "inbox", []string{"snooze", "abcd", "1h"}},
		{[]string{"resume", "id"}, "resume", []string{"id"}},
		{[]string{"resume", "--remote-control", "id"}, "resume", []string{"--remote-control", "id"}},
		{[]string{"remote-control", "id"}, "remote-control", []string{"id"}},
		{[]string{"join"}, "join", []string{}},
		{[]string{"move", "abcd", "tower"}, "move", []string{"abcd", "tower"}},
		{[]string{"move-key"}, "move-key", []string{}},
	}
	for _, tt := range tests {
		var calls []string
		var argv [][]string
		code, out, errOut := runQuiet(tt.args, recorders(&calls, &argv))
		if code != 0 || out != "" || errOut != "" {
			t.Errorf("%v: code=%d out=%q err=%q", tt.args, code, out, errOut)
		}
		if len(calls) != 1 || calls[0] != tt.wantCall || !reflect.DeepEqual(argv[0], tt.wantArgs) {
			t.Errorf("%v: calls=%v args=%v, want %s %v", tt.args, calls, argv, tt.wantCall, tt.wantArgs)
		}
	}
}

func TestHandlerErrorExitsOne(t *testing.T) {
	r := map[string]handler{"ls": func(context.Context, []string) error { return errors.New("boom") }}
	code, out, errOut := runQuiet([]string{"ls"}, r)
	if code != 1 || out != "" || !strings.Contains(errOut, "boom") {
		t.Errorf("code=%d out=%q err=%q", code, out, errOut)
	}
}

// TestExitErrorIsQuiet: a command that returns cli.ExitError sets the exit
// status and prints nothing (sessionhub inbox --watch after a signal).
func TestExitErrorIsQuiet(t *testing.T) {
	r := map[string]handler{"inbox": func(context.Context, []string) error { return fmt.Errorf("wrapped: %w", &cli.ExitError{Code: 130}) }}
	code, out, errOut := runQuiet([]string{"inbox"}, r)
	if code != 130 || out != "" || errOut != "" {
		t.Errorf("code=%d out=%q err=%q, want 130 and no output", code, out, errOut)
	}
}

func TestUnknownCommand(t *testing.T) {
	var calls []string
	var argv [][]string
	code, out, errOut := runQuiet([]string{"bogus"}, recorders(&calls, &argv))
	if code != 2 || out != "" || !strings.Contains(errOut, "usage: sessionhub") || len(calls) != 0 {
		t.Errorf("code=%d out=%q err=%q calls=%v", code, out, errOut, calls)
	}
}

func TestUsageListsEveryCommand(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}} {
		code, out, errOut := runQuiet(args, nil)
		if code != 0 || errOut != "" {
			t.Errorf("%v: code=%d err=%q", args, code, errOut)
		}
		for _, c := range append(commands, "version") {
			if !strings.Contains(out, "  "+c) {
				t.Errorf("%v: usage missing %q", args, c)
			}
		}
	}
}

func TestHookAlwaysExitsZero(t *testing.T) {
	tests := []struct {
		name    string
		h       handler
		wantErr string
	}{
		{"error", func(context.Context, []string) error { return errors.New("boom") }, "boom"},
		{"panic", func(context.Context, []string) error { panic("kaboom") }, "kaboom"},
		{"ok", func(context.Context, []string) error { return nil }, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, out, errOut := runQuiet([]string{"hook", "stop"}, map[string]handler{"hook": tt.h})
			if code != 0 || out != "" || !strings.Contains(errOut, tt.wantErr) {
				t.Errorf("code=%d out=%q err=%q", code, out, errOut)
			}
		})
	}
}
