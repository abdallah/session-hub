package mcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/abdallah/session-hub/internal/paths"
)

func claude(ctx context.Context, args ...string) (string, error) {
	path, err := exec.LookPath("claude")
	if err != nil {
		return "", fmt.Errorf("claude is not on PATH: %w", err)
	}
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// RunInstall registers the MCP server at user scope. If registration fails
// because sessionhub is already registered, it removes the old entry and adds it
// again.
func RunInstall(ctx context.Context, args []string) error {
	add := []string{"mcp", "add", "--scope", "user", "sessionhub", "--", paths.Binary(), "mcp"}
	out, err := claude(ctx, add...)
	if err != nil {
		if _, rerr := claude(ctx, "mcp", "remove", "--scope", "user", "sessionhub"); rerr != nil {
			return fmt.Errorf("install-mcp: claude mcp add failed: %v: %s", err, out)
		}
		if out, err = claude(ctx, add...); err != nil {
			return fmt.Errorf("install-mcp: claude mcp add failed: %v: %s", err, out)
		}
	}
	fmt.Fprintf(os.Stdout, "Registered the sessionhub MCP server: %s mcp\n", paths.Binary())
	return nil
}

// RunUninstall removes the user-scope MCP registration.
func RunUninstall(ctx context.Context, args []string) error {
	if out, err := claude(ctx, "mcp", "remove", "--scope", "user", "sessionhub"); err != nil {
		return fmt.Errorf("uninstall-mcp: claude mcp remove failed: %v: %s", err, out)
	}
	fmt.Fprintln(os.Stdout, "Removed the sessionhub MCP server.")
	return nil
}
