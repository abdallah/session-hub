package hooks

import (
	"os"
	"strconv"
	"strings"
)

// exeOf is the path of process pid's executable, from /proc/<pid>/exe, or "".
func exeOf(pid int) string {
	p, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return ""
	}
	return p
}

// ppidOf reads field 4 of /proc/<pid>/stat. The command name (field 2) may
// contain spaces and parentheses, so parse after the last ")".
func ppidOf(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(f[1])
	return n
}
