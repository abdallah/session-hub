package main

import (
	"bufio"
	"io"
	"strings"
)

// firstNameserver returns the first nameserver in a resolv.conf, or "".
func firstNameserver(r io.Reader) string {
	s := bufio.NewScanner(r)
	for s.Scan() {
		f := strings.Fields(s.Text())
		if len(f) >= 2 && f[0] == "nameserver" {
			return f[1]
		}
	}
	return ""
}
