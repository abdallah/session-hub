package hooks

import (
	"os/exec"
	"strconv"
	"strings"
)

// macOS has no /proc: ask ps. Both lookups return the zero value when ps
// fails or the process is gone, as the Linux versions do.

// exeOf is the path of process pid's executable: ps's comm column, which
// macOS prints as the full path.
func exeOf(pid int) string {
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ppidOf is process pid's parent, from ps.
func ppidOf(pid int) int {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}
