package plugin

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/abdallah/session-hub/internal/cli/termtext"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/herdr"
)

// blockedTitleRunes caps the session title in a Blocked notification.
const blockedTitleRunes = 80

// notifyHerdr is the part of *herdr.Client a Blocked notification uses.
type notifyHerdr interface {
	PaneGet(id string) (herdr.PaneInfo, error)
	NotificationShow(p herdr.NotificationParams) error
}

// blockedStatus returns the payload of a pane.agent_status_changed event
// whose new status is blocked, for a Claude pane or one whose agent herdr
// did not name (the same filter itemFromEvent uses). ok is false for every
// other event.
func blockedStatus(e herdr.Event) (herdr.AgentStatusChanged, bool) {
	if e.Event != herdr.EventAgentStatusChange {
		return herdr.AgentStatusChanged{}, false
	}
	d, err := e.AgentStatusChanged()
	if err != nil || d.PaneID == "" || d.AgentStatus != "blocked" || (d.Agent != "" && d.Agent != "claude") {
		return herdr.AgentStatusChanged{}, false
	}
	return d, true
}

// notifyBlocked shows "Blocked: <title>" with the machine as the body and
// the request sound. The title is the event's, else the pane's terminal
// title (herdr's events carry none so far), cleaned and cut to
// blockedTitleRunes, else "Claude".
func notifyBlocked(h notifyHerdr, d herdr.AgentStatusChanged, machine string) error {
	title := d.Title
	if title == "" {
		if p, err := h.PaneGet(d.PaneID); err == nil {
			title = p.TitleClean
			if title == "" {
				title = p.Title
			}
		}
	}
	title = termtext.Clean(title, blockedTitleRunes)
	if title == "" {
		title = "Claude"
	}
	return h.NotificationShow(herdr.NotificationParams{
		Title: "Blocked: " + title,
		Body:  termtext.Clean(machine, 0),
		Sound: "request",
	})
}

// machineName is the client config's machine, else the host name.
func machineName() string {
	if cfg, err := client.LoadConfig(); err == nil && cfg.Machine != "" {
		return cfg.Machine
	}
	h, _ := os.Hostname()
	return h
}

// alertBlocked shows the Blocked notification in the herdr at socketPath. A
// failure goes to the watcher log and is otherwise ignored: the hook never
// fails.
func alertBlocked(stateDir, socketPath string, d herdr.AgentStatusChanged) {
	h, err := herdr.Dial(socketPath)
	if err == nil {
		err = notifyBlocked(h, d, machineName())
	}
	if err != nil {
		watcherLogf(stateDir, "event: herdr notification for blocked pane %q: %v", d.PaneID, err)
	}
}

// watcherLogf appends one line to <stateDir>/watcher.log in the watcher's
// format. If the file can't be opened, the line goes to stderr (herdr's
// plugin log).
func watcherLogf(stateDir, format string, args ...any) {
	os.MkdirAll(stateDir, 0o700)
	f, err := os.OpenFile(filepath.Join(stateDir, watcherLogFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(stderr, "sessionhub plugin event: "+format+"\n", args...)
		return
	}
	defer f.Close()
	log.New(f, "", log.LstdFlags|log.LUTC).Printf(format, args...)
}
