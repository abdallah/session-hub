package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Escape sequences for the alternate screen.
const (
	enterScreen = "\x1b[?1049h\x1b[?25l" // alternate screen, cursor hidden
	leaveScreen = "\x1b[?25h\x1b[?1049l" // cursor shown, main screen back
	clearScreen = "\x1b[H\x1b[2J"
)

// sttyFunc runs stty on the terminal with args and returns its output.
type sttyFunc func(args ...string) (string, error)

// realStty runs stty with stdin on the terminal, which is how stty finds it.
func realStty(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	return string(out), err
}

// withRawTerminal saves the terminal settings, sets raw mode without echo,
// switches to the alternate screen, and runs fn. It restores the screen and
// the settings on every way out: a return, an error, or a panic (the
// deferred restore runs while the panic unwinds). It registers the restore
// before changing anything, so a failing raw call restores too. If the
// settings can't be read or changed, fn does not run.
func withRawTerminal(stty sttyFunc, out io.Writer, fn func() error) error {
	saved, err := stty("-g")
	if err != nil {
		return fmt.Errorf("read the terminal settings: %w", err)
	}
	entered := false
	restore := sync.OnceFunc(func() {
		if entered {
			fmt.Fprint(out, leaveScreen)
		}
		stty(strings.TrimSpace(saved))
	})
	defer restore()
	if _, err := stty("raw", "-echo"); err != nil {
		return fmt.Errorf("set raw mode: %w", err)
	}
	entered = true
	fmt.Fprint(out, enterScreen)
	return fn()
}

// runWatch is sessionhub inbox --watch. SIGINT, SIGTERM, and SIGHUP (the pane
// closing) end the loop, so the terminal is restored. In raw mode Ctrl+C is
// a key, not a signal.
func (e *env) runWatch(ctx context.Context) error {
	if e.isTerminal == nil || !e.isTerminal() {
		return errors.New("inbox --watch: stdin and stdout must be a terminal")
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	err := withRawTerminal(realStty, os.Stdout, func() error {
		keys := make(chan []byte)
		go readKeys(os.Stdin, keys)
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		t := time.NewTicker(watchRefresh)
		defer t.Stop()
		return e.watchLoop(ctx, watchIO{
			keys:   keys,
			ticks:  t.C,
			resize: winch,
			// Raw mode turns off the newline translation: lines end in \r\n.
			draw: func(lines []string) { fmt.Fprint(os.Stdout, clearScreen+strings.Join(lines, "\r\n")) },
			size: termSize,
		})
	})
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		// A signal ended the loop: exit 130 without an error message. The
		// terminal is already restored.
		return &ExitError{Code: 130}
	}
	return err
}

// ExitError asks main to exit with Code and print nothing. A command returns
// it when it has already said what it needs to, such as after a signal.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// readKeys sends each read from r on keys and closes keys at the end of
// input or on an error.
func readKeys(r io.Reader, keys chan<- []byte) {
	defer close(keys)
	buf := make([]byte, 64)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			keys <- append([]byte(nil), buf[:n]...)
		}
		if err != nil {
			return
		}
	}
}
