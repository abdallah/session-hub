package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"text/tabwriter"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

// loginAPI is the part of *client.Client that sessionhub login uses.
type loginAPI interface {
	CreateLogin(ctx context.Context, name string) (api.Login, error)
	ListWebSessions(ctx context.Context) ([]api.WebSession, error)
	RevokeWebSession(ctx context.Context, id string) error
}

const loginUsage = `usage:
  sessionhub login --name <name> [--no-qr] [--json]
  sessionhub login ls [--json]
  sessionhub login rm <name|id>`

// RunLogin implements `sessionhub login`, `sessionhub login ls`, and `sessionhub login rm`.
func RunLogin(ctx context.Context, args []string) error {
	e, err := defaultEnv()
	if err != nil {
		return err
	}
	return e.loginCmd(ctx, args)
}

func (e *env) loginCmd(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "ls":
			return e.loginLs(ctx, args[1:])
		case "rm":
			return e.loginRm(ctx, args[1:])
		}
	}
	return e.loginCreate(ctx, args)
}

func (e *env) loginCreate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessionhub login", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "the browser session's name")
	noQR := fs.Bool("no-qr", false, "omit the QR code")
	asJSON := fs.Bool("json", false, "print only {url, name, expires_at}")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("login: %v\n%s", err, loginUsage)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("login: unexpected argument %q\n%s", fs.Arg(0), loginUsage)
	}
	if *name == "" {
		return fmt.Errorf("login: --name is required\n%s", loginUsage)
	}
	l, err := e.login.CreateLogin(ctx, *name)
	var se *client.StatusError
	if errors.As(err, &se) && se.Status == http.StatusConflict {
		return fmt.Errorf("login: a browser session named %s exists; revoke it with `sessionhub login rm %s` or pick another name", *name, *name)
	}
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if *asJSON {
		b, err := json.Marshal(l)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(e.out, "%s\n", b)
		return err
	}
	fmt.Fprintf(e.out, "Open this link on the device to sign it in as %s:\n\n  %s\n\n", clean(l.Name, 0), clean(l.URL, 0))
	fmt.Fprintf(e.out, "The link works once and expires at %s.\n", l.ExpiresAt.Local().Format("15:04:05"))
	if *noQR {
		return nil
	}
	fmt.Fprintln(e.out)
	return renderQR(e.out, l.URL)
}

func (e *env) loginLs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessionhub login ls", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "print {sessions: [...]} as JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return fmt.Errorf("login ls: unexpected arguments\n%s", loginUsage)
	}
	list, err := e.login.ListWebSessions(ctx)
	if err != nil {
		return fmt.Errorf("login ls: %w", err)
	}
	if *asJSON {
		b, err := json.Marshal(api.WebSessionList{Sessions: list})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(e.out, "%s\n", b)
		return err
	}
	if len(list) == 0 {
		fmt.Fprintln(e.out, "no browser sessions; sign one in with: sessionhub login --name <device>")
		return nil
	}
	const day = "2006-01-02 15:04"
	tw := tabwriter.NewWriter(e.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tID\tMACHINE\tCREATED\tLAST_USED\tEXPIRES")
	for _, s := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", clean(s.Name, 0), clean(s.ID, 0), clean(s.Machine, 0),
			s.CreatedAt.Local().Format(day), age(e.now(), s.LastUsedAt), s.ExpiresAt.Local().Format(day))
	}
	return tw.Flush()
}

// loginRm resolves a name, then an ID, through the list, and deletes by ID.
func (e *env) loginRm(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("login rm: want one name or ID\n%s", loginUsage)
	}
	target := args[0]
	list, err := e.login.ListWebSessions(ctx)
	if err != nil {
		return fmt.Errorf("login rm: %w", err)
	}
	var hit *api.WebSession
	for i := range list {
		if list[i].Name == target {
			hit = &list[i]
			break
		}
	}
	if hit == nil {
		for i := range list {
			if list[i].ID == target {
				hit = &list[i]
				break
			}
		}
	}
	if hit == nil {
		return fmt.Errorf("login rm: no browser session named or with ID %q; see `sessionhub login ls`", target)
	}
	if err := e.login.RevokeWebSession(ctx, hit.ID); err != nil {
		return fmt.Errorf("login rm: %w", err)
	}
	fmt.Fprintf(e.out, "signed out %s (%s)\n", clean(hit.Name, 0), clean(hit.ID, 0))
	return nil
}

// Compile-time check that the real client satisfies loginAPI.
var _ loginAPI = (*client.Client)(nil)
