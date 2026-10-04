package plugin

import (
	"strconv"
	"strings"
)

// ManifestFile is the file name herdr reads in a plugin directory.
const ManifestFile = "herdr-plugin.toml"

// PluginID is the herdr plugin id.
const PluginID = "sessionhub"

// Manifest returns the herdr plugin manifest from docs/dev/PLAN.md "herdr plugin",
// with every command calling bin by absolute path: herdr runs plugin commands
// without a shell and with its server's PATH.
func Manifest(bin string) string {
	q := strconv.Quote(bin)
	cmd := func(args ...string) string {
		parts := []string{q}
		for _, a := range args {
			parts = append(parts, strconv.Quote(a))
		}
		return "command = [" + strings.Join(parts, ", ") + "]\n"
	}
	var b strings.Builder
	b.WriteString(`id = "sessionhub"
name = "sessionhub"
version = "0.1.0"
min_herdr_version = "0.9.3"
description = "Register the Claude Code sessions in herdr panes with sessionhub."
platforms = ["linux", "macos"]

[[startup]]
`)
	b.WriteString(cmd("plugin", "startup"))
	for _, on := range []string{"pane.agent_detected", "pane.agent_status_changed", "pane.closed", "pane.exited"} {
		b.WriteString("\n[[events]]\non = " + strconv.Quote(on) + "\n")
		b.WriteString(cmd("plugin", "event"))
	}
	b.WriteString(`
[[actions]]
id = "status"
title = "sessionhub: sessions on this machine"
contexts = ["global"]
`)
	b.WriteString(cmd("status"))
	b.WriteString(`
[[actions]]
id = "resume"
title = "sessionhub: resume a session"
contexts = ["global"]
`)
	b.WriteString(cmd("plugin", "open-picker"))
	b.WriteString(`
[[actions]]
id = "inbox"
title = "sessionhub: inbox"
contexts = ["global"]
`)
	b.WriteString(cmd("plugin", "open-inbox"))
	b.WriteString(`
[[panes]]
id = "resume-picker"
title = "sessionhub: resume"
placement = "overlay"
`)
	b.WriteString(cmd("resume", "--pick"))
	b.WriteString(`
[[panes]]
id = "inbox"
title = "sessionhub: inbox"
placement = "split"
`)
	b.WriteString(cmd("inbox", "--watch"))
	return b.String()
}
