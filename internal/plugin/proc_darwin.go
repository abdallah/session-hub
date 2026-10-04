package plugin

import (
	"os/exec"
	"strconv"
	"strings"
)

// commandLine is process pid's arguments joined by spaces, from ps (macOS
// has no /proc).
func commandLine(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
