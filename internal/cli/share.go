package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"

	"github.com/abdallah/session-hub/internal/client"
)

const shareUsage = `usage:
  sessionhub share [<id>] [--no-qr]   a one-time guest link to one session (default: this Claude session)`

// RunShare implements `sessionhub share`: `sessionhub login` with the session
// scope set and a generated name, printed as the attach command to run.
func RunShare(ctx context.Context, args []string) error {
	e, err := defaultEnv()
	if err != nil {
		return err
	}
	return e.shareCmd(ctx, args)
}

func (e *env) shareCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessionhub share", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	noQR := fs.Bool("no-qr", false, "omit the QR code")
	// Flags may follow the ID: parse twice around it.
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("share: %v\n%s", err, shareUsage)
	}
	id := fs.Arg(0)
	if fs.NArg() > 0 {
		if err := fs.Parse(fs.Args()[1:]); err != nil || fs.NArg() > 0 {
			return fmt.Errorf("share: unexpected arguments\n%s", shareUsage)
		}
	}
	if id == "" {
		if id = e.getenv("CLAUDE_CODE_SESSION_ID"); !sessionIDRE.MatchString(id) {
			return fmt.Errorf("share: not inside a Claude session; pass a session ID from `sessionhub ls`\n%s", shareUsage)
		}
	}
	name := "share-" + e.now().Format("0102-150405")
	l, err := e.login.CreateLogin(ctx, name, id)
	var se *client.StatusError
	if errors.As(err, &se) && se.Status == http.StatusNotFound {
		return fmt.Errorf("share: no session %s; see `sessionhub ls`", id)
	}
	if err != nil {
		return fmt.Errorf("share: %w", err)
	}
	fmt.Fprintf(e.out, "On the other device, run:\n\n  sessionhub attach %s\n\n", clean(l.URL, 0))
	fmt.Fprintf(e.out, "Or open the link in a browser. It gives access to session %s only, works once, and expires at %s.\n",
		clean(l.Session, 0), l.ExpiresAt.Local().Format("15:04:05"))
	if *noQR {
		return nil
	}
	fmt.Fprintln(e.out)
	return renderQR(e.out, l.URL)
}
