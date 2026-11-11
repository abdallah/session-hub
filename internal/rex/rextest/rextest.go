// Package rextest puts a fake rex command on PATH for tests.
package rextest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// script answers the rex commands sessionhub runs, from the files in
// $FAKE_REX_STATE:
//
//   - calls: every command line, one per line.
//   - gone: block calls fail the way rex does for a missing block. broken:
//     they fail another way.
//   - noclaude: process reports a shell instead of claude.
//   - frame: the text format returns; frame_after replaces it once
//     /remote-control is typed.
//   - writes: the JSON argument of every write. Typing /exit makes noclaude,
//     unless exit_ignored exists.
//   - block ls lists no block once gone exists; ls lists session:new, which
//     sessionhub labelled, and session:1, which it did not.
//   - nostart: new and run fail. norun: run fails, as with no session open. noclaude_after_start: Claude does not run in
//     what they create; otherwise new and run remove gone and noclaude.
const script = `#!/bin/sh
state="$FAKE_REX_STATE"
printf '%s\n' "$*" >> "$state/calls"
case "$1 $2" in
"block call")
  [ -e "$state/gone" ] && { echo "Error: block target \"$6\" not found: session scope: session \"$4\" not found" >&2; exit 3; }
  [ -e "$state/broken" ] && { echo "Error: connection refused" >&2; exit 1; }
  case "$7" in
  format) printf '{"content":%s}' "$(cat "$state/frame")" ;;
  process) if [ -e "$state/noclaude" ]; then echo '{"foreground":{"pid":2,"name":"zsh"}}'; else echo '{"child":{"pid":2,"name":"claude"}}'; fi ;;
  write)
    printf '%s\n' "$8" >> "$state/writes"
    case "$8" in
    *"/exit"*) [ -e "$state/exit_ignored" ] || : > "$state/noclaude" ;;
    *"/remote-control"*) [ -e "$state/frame_after" ] && cp "$state/frame_after" "$state/frame" ;;
    esac
    echo '{"bytes_written":1}' ;;
  *) echo "unknown method $7" >&2; exit 2 ;;
  esac ;;
"new "*|"run "*)
  [ -e "$state/nostart" ] && { echo "Error: boom" >&2; exit 1; }
  [ "$1" = run ] && [ -e "$state/norun" ] && { echo "Error: session target default not found: no sessions" >&2; exit 1; }
  rm -f "$state/gone" "$state/noclaude"
  [ -e "$state/noclaude_after_start" ] && : > "$state/noclaude"
  if [ "$1" = new ]; then
    echo '{"session_id":"session:new","initial_windows":[{"window_id":"window:new","block_ids":["block:new"]}]}'
  else
    echo '{"window_id":"window:run","block_ids":["block:run"]}'
  fi ;;
"block ls") if [ -e "$state/gone" ]; then echo '{"blocks":[]}'; else echo '{"blocks":[{"block_id":"block:1"}]}'; fi ;;
"ls --json") echo '{"sessions":[{"session_id":"session:new","label":"sessionhub: new in app"},{"session_id":"session:1","label":"mine"}]}' ;;
"block focus") [ -e "$state/gone" ] && { echo "Error: block not found" >&2; exit 3; }; exit 0 ;;
"session kill") : > "$state/gone" ;;
*) echo "bad args: $*" >&2; exit 2 ;;
esac
`

// Fake puts the fake rex first on PATH and returns its state directory. The
// first frame is empty.
func Fake(t *testing.T) string {
	t.Helper()
	state, bin := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "rex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	Set(t, state, "frame", `""`)
	t.Setenv("FAKE_REX_STATE", state)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return state
}

// HideReal drops from PATH every directory that holds a rex command, so no
// test runs the machine's own Rex. Call it from TestMain.
func HideReal() {
	var keep []string
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if _, err := os.Stat(filepath.Join(d, "rex")); err != nil {
			keep = append(keep, d)
		}
	}
	os.Setenv("PATH", strings.Join(keep, string(os.PathListSeparator)))
}

// Set writes one state file.
func Set(t *testing.T, state, name, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(state, name), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Read returns one state file, or "" when it does not exist.
func Read(t *testing.T, state, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(state, name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// Called reports whether a command line that contains want ran.
func Called(t *testing.T, state, want string) bool {
	t.Helper()
	return strings.Contains(Read(t, state, "calls"), want)
}
