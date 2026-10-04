package plugin

import (
	"fmt"
	"os"
	"strings"
)

// commandLine is process pid's arguments joined by spaces, from
// /proc/<pid>/cmdline.
func commandLine(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "", err
	}
	return strings.Join(strings.Split(strings.TrimRight(string(b), "\x00"), "\x00"), " "), nil
}
