//go:build android

package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
)

// Android has no /etc/resolv.conf, so Go's resolver (CGO_ENABLED=0) falls
// back to [::1]:53 and fails. Use Termux's resolv.conf, then a public server.
func init() {
	if _, err := os.Stat("/etc/resolv.conf"); err == nil {
		return
	}
	server := ""
	if f, err := os.Open(filepath.Join(os.Getenv("PREFIX"), "etc", "resolv.conf")); err == nil {
		server = firstNameserver(f)
		f.Close()
	}
	if server == "" {
		server = "1.1.1.1"
	}
	addr := net.JoinHostPort(server, "53")
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
}
