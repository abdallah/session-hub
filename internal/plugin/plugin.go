// Package plugin is the herdr plugin: manifest install, the startup and
// event hooks, and the long-running watcher that drains the queue.
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/gitinfo"
	"github.com/abdallah/session-hub/internal/herdr"
	"github.com/abdallah/session-hub/internal/paths"
)

// stderr is where the hooks report errors; herdr's plugin log keeps it.
var stderr io.Writer = os.Stderr

func loadClient() (*client.Client, error) {
	cfg, err := client.LoadConfig()
	if err != nil {
		return nil, err
	}
	return client.New(cfg)
}

// Run dispatches `sessionhub plugin startup|event|watch`. startup and event always
// return nil (herdr must never see them fail); they print problems to stderr.
func Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: sessionhub plugin startup|event|watch|open-picker|open-inbox")
	}
	switch args[0] {
	case "startup":
		runStartup(ctx, herdr.SocketPath(), paths.StateDir())
		return nil
	case "event":
		runEvent(paths.StateDir(), time.Now())
		return nil
	case "watch":
		ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		return runWatcher(ctx, paths.StateDir(), herdr.SocketPath(), loadClient, gitinfo.Lookup,
			log.New(os.Stderr, "", log.LstdFlags|log.LUTC))
	}
	return fmt.Errorf("sessionhub plugin: unknown subcommand %q (want startup, event, watch, open-picker, or open-inbox)", args[0])
}

func startWatcher(stateDir string) {
	exe, err := executable()
	if err == nil {
		_, err = ensureWatcher(stateDir, exe)
	}
	if err != nil {
		fmt.Fprintf(stderr, "sessionhub plugin: start watcher: %v\n", err)
	}
}

// runStartup sends the full set of Claude panes, queues it when the server is
// unreachable, and starts the watcher.
func runStartup(ctx context.Context, socketPath, stateDir string) {
	name := herdr.SessionName(socketPath)
	hc, err := herdr.Dial(socketPath)
	if err != nil {
		fmt.Fprintf(stderr, "sessionhub plugin startup: herdr socket: %v\n", err)
		return
	}
	snap, err := hc.Snapshot()
	if err != nil {
		fmt.Fprintf(stderr, "sessionhub plugin startup: snapshot: %v\n", err)
		return
	}
	put := api.HerdrSessionsPut{HerdrSession: name, Sessions: buildSessions(ctx, snap, name, gitinfo.Lookup), UnidentifiedPanes: unidentifiedPanes(snap)}
	c, err := loadClient()
	if err == nil {
		var res client.HerdrSessionsResult
		res, err = c.PutHerdrSessionsResult(ctx, put)
		for _, in := range res.Invalid {
			fmt.Fprintf(stderr, "sessionhub plugin startup: server skipped session %q: %s\n", in.ID, in.Reason)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "sessionhub plugin startup: send %d sessions: %v (queued)\n", len(put.Sessions), err)
		if qerr := queueJSON(client.NewQueue(stateDir), client.Item{Op: client.OpHerdrSessions}, put); qerr != nil {
			fmt.Fprintf(stderr, "sessionhub plugin startup: queue: %v\n", qerr)
		}
	} else {
		fmt.Fprintf(stderr, "sessionhub plugin startup: sent %d sessions for herdr session %q\n", len(put.Sessions), name)
	}
	startWatcher(stateDir)
}

// runEvent queues one item for HERDR_PLUGIN_EVENT_JSON and makes sure the
// watcher runs. It makes no network call. Its only herdr calls are for a
// Claude pane that became blocked: a notification with a sound, on this
// machine, where the session runs.
func runEvent(stateDir string, now time.Time) {
	e, err := herdr.EventFromEnv()
	if err != nil {
		fmt.Fprintf(stderr, "sessionhub plugin event: %v\n", err)
		startWatcher(stateDir)
		return
	}
	if it, ok, err := itemFromEvent(e, herdr.SessionName(herdr.SocketPath()), now); err != nil {
		fmt.Fprintf(stderr, "sessionhub plugin event: %s: %v\n", e.Event, err)
	} else if ok {
		if err := client.NewQueue(stateDir).Append(it); err != nil {
			fmt.Fprintf(stderr, "sessionhub plugin event: queue: %v\n", err)
		}
	}
	startWatcher(stateDir)
	if d, ok := blockedStatus(e); ok {
		alertBlocked(stateDir, herdr.SocketPath(), d)
	}
}

// stopWatcherTimeout is how long install and uninstall wait for the watcher
// to exit: long enough to finish a request and post its result (controlPostFor).
const stopWatcherTimeout = 7 * time.Second

// RunInstall writes the manifest, links it into herdr, and then does what
// herdr's [[startup]] would: herdr runs startup hooks only when its server
// starts, not on link, so without this the plugin would stay idle until the
// next herdr restart. A watcher from an older binary is stopped first. The
// manifest names ~/.local/bin/sessionhub, which must exist (`make install`).
func RunInstall(ctx context.Context, args []string) error {
	if _, err := installManifest(ctx, os.Stdout); err != nil {
		return err
	}
	if stopped, err := stopWatcher(paths.StateDir(), stopWatcherTimeout); err != nil {
		fmt.Fprintf(os.Stderr, "install-plugin: stop old watcher: %v\n", err)
	} else if stopped {
		fmt.Println("stopped the running watcher")
	}
	runStartup(ctx, herdr.SocketPath(), paths.StateDir())
	return nil
}

// RunUninstall unlinks the plugin, removes the plugin dir, and stops the
// watcher. The state dir (queue, log) is kept: hooks may still use the queue.
func RunUninstall(ctx context.Context, args []string) error {
	if err := uninstall(ctx, os.Stdout); err != nil {
		return err
	}
	stopped, err := stopWatcher(paths.StateDir(), stopWatcherTimeout)
	if err != nil {
		return err
	}
	if stopped {
		fmt.Println("stopped the watcher")
	}
	return nil
}

func queueJSON(q *client.Queue, it client.Item, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	it.Body = b
	return q.Append(it)
}
