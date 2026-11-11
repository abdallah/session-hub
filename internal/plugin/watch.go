package plugin

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/abdallah/session-hub/internal/gitinfo"
	"github.com/abdallah/session-hub/internal/herdr"
	"github.com/abdallah/session-hub/internal/paths"
)

// RunWatch implements `sessionhub watch`: the watcher as a plain process,
// for a machine without herdr. With a herdr socket it is the plugin's
// watcher. It waits for the lock while another watcher runs.
func RunWatch(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: sessionhub watch")
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return runWatcher(ctx, paths.StateDir(), herdr.SocketPath(), true, loadClient, gitinfo.Lookup,
		log.New(os.Stderr, "", log.LstdFlags|log.LUTC))
}

// watcherUnit is the systemd user unit that runs `sessionhub watch`.
const watcherUnit = "sessionhub-watcher.service"

// watcherUnitText is the unit, with a %s for an Environment= line. It
// restarts the watcher whenever it exits: it exits on
// purpose when a herdr socket appears or goes away, to start in the other
// mode. The binary is ~/.local/bin/sessionhub (`make install`).
const watcherUnitText = `# systemd user unit for the sessionhub watcher on a machine without herdr.
# Written by sessionhub install-watcher.
[Unit]
Description=sessionhub watcher
StartLimitIntervalSec=0

[Service]
ExecStart=%%h/.local/bin/sessionhub watch
%sRestart=always
RestartSec=5s

[Install]
WantedBy=default.target
`

// watcherUnitPath is the unit file under $XDG_CONFIG_HOME, or ~/.config.
func watcherUnitPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "systemd", "user", watcherUnit), nil
}

// systemctl runs `systemctl --user args`. Tests replace it.
var systemctl = func(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RunInstallWatcher implements `sessionhub install-watcher`: it writes the
// unit, stops a watcher from an older binary, and enables and restarts the
// unit.
func RunInstallWatcher(ctx context.Context, args []string) error {
	path, err := watcherUnitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// A herdr socket that is not the default must reach the unit, or its
	// watcher would never see herdr.
	env := ""
	if sock := os.Getenv("HERDR_SOCKET_PATH"); sock != "" && !strings.ContainsAny(sock, "\n\"") {
		env = fmt.Sprintf("Environment=\"HERDR_SOCKET_PATH=%s\"\n", sock)
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf(watcherUnitText, env)), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", path)
	if stopped, err := stopWatcher(paths.StateDir(), stopWatcherTimeout); err != nil {
		fmt.Fprintf(os.Stderr, "install-watcher: stop old watcher: %v\n", err)
	} else if stopped {
		fmt.Println("stopped the running watcher")
	}
	for _, a := range [][]string{{"daemon-reload"}, {"enable", watcherUnit}, {"restart", watcherUnit}} {
		if err := systemctl(ctx, a...); err != nil {
			return err
		}
	}
	fmt.Println("enabled and started " + watcherUnit + "; to keep it running while you are logged out, run: loginctl enable-linger")
	return nil
}

// RunUninstallWatcher implements `sessionhub uninstall-watcher`: it stops
// and disables the unit and removes its file.
func RunUninstallWatcher(ctx context.Context, args []string) error {
	path, err := watcherUnitPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		fmt.Println(watcherUnit + " is not installed")
		return nil
	}
	if err := systemctl(ctx, "disable", "--now", watcherUnit); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	fmt.Println("removed", path)
	return systemctl(ctx, "daemon-reload")
}
