package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/paths"
)

const attachUsage = `usage:
  sessionhub attach <login-url>   sign in with a guest link and attach to its session
  sessionhub attach               attach again with the saved guest sign-in
  sessionhub attach <id>          attach to one of your sessions (ID or unique prefix)`

// detachKey is Ctrl-], which ends the attach and leaves the session running.
const detachKey = 0x1d

// attachAPI is the part of *client.Client that sessionhub attach uses.
type attachAPI interface {
	CreateAttach(ctx context.Context, id string) (api.Attach, error)
	AttachOutput(ctx context.Context, id string) (io.ReadCloser, error)
	SendAttachInput(ctx context.Context, id string, b []byte) error
}

// guestLogin is the saved guest sign-in, guest.json in the state directory.
type guestLogin struct {
	ServerURL string    `json:"server_url"`
	Token     string    `json:"token"`
	Session   string    `json:"session"`
	ExpiresAt time.Time `json:"expires_at"`
}

// RunAttach implements `sessionhub attach [<login-url> | <id>]`.
func RunAttach(ctx context.Context, args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("attach: too many arguments\n%s", attachUsage)
	}
	arg := ""
	if len(args) == 1 {
		arg = args[0]
	}
	c, id, err := resolveAttach(ctx, arg, filepath.Join(paths.StateDir(), "guest.json"), time.Now(), os.Stdout, ownerClient)
	if err != nil {
		return err
	}
	if !isTerminal(os.Stdin.Fd()) || !isTerminal(os.Stdout.Fd()) {
		return errors.New("attach: stdin and stdout must be a terminal")
	}
	a, err := c.CreateAttach(ctx, id)
	var se *client.StatusError
	if errors.As(err, &se) && se.Status == http.StatusConflict {
		return fmt.Errorf("attach: %s", clean(se.Message, 0))
	}
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	if w, h := termSize(); w < 80 || h < 24 {
		fmt.Fprintf(os.Stdout, "Your terminal is %dx%d; the mirror keeps the host pane's size.\n", w, h)
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	var detached bool
	err = withRawTerminal(realStty, os.Stdout, func() error {
		var err error
		detached, err = mirrorAttach(ctx, c, a.ID, os.Stdin, os.Stdout)
		return err
	})
	switch {
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		return &ExitError{Code: 130} // a signal ended it; the terminal is restored
	case err != nil:
		return fmt.Errorf("attach: %w", err)
	case detached:
		fmt.Fprintln(os.Stdout, "\nDetached.")
	default:
		fmt.Fprintln(os.Stdout, "\nSession ended.")
	}
	return nil
}

// ownerClient is the machine's own client, from the client config.
func ownerClient() (*client.Client, error) {
	cfg, err := client.LoadConfig()
	if err != nil {
		return nil, err
	}
	c, err := client.New(cfg)
	if err != nil {
		return nil, err
	}
	c.SetTimeout(client.InteractiveTimeout)
	return c, nil
}

// resolveAttach picks the client and session for arg: a login URL signs in
// as a guest and saves the sign-in to guestPath, no argument reuses it, and
// anything else is a session ID or prefix for the machine's own client.
func resolveAttach(ctx context.Context, arg, guestPath string, now time.Time, out io.Writer, owner func() (*client.Client, error)) (attachAPI, string, error) {
	var g guestLogin
	switch {
	case strings.HasPrefix(arg, "https://") || strings.HasPrefix(arg, "http://"):
		var err error
		if g, err = redeemLogin(ctx, arg, now); err != nil {
			return nil, "", err
		}
		if err := saveGuest(guestPath, g); err != nil {
			return nil, "", fmt.Errorf("attach: save the guest sign-in: %w", err)
		}
		u, _ := url.Parse(g.ServerURL)
		fmt.Fprintf(out, "Signed in as a guest for session %s on %s.\n", shortID(g.Session), clean(u.Host, 0))
	case arg == "":
		b, err := os.ReadFile(guestPath)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "", errors.New("attach: no guest sign-in; run sessionhub attach <login-url> first")
		}
		if err != nil {
			return nil, "", fmt.Errorf("attach: %w", err)
		}
		if err := json.Unmarshal(b, &g); err != nil {
			return nil, "", fmt.Errorf("attach: read %s: %w", guestPath, err)
		}
		if !g.ExpiresAt.IsZero() && !now.Before(g.ExpiresAt) {
			return nil, "", errors.New("attach: the guest sign-in expired; run sessionhub attach <login-url> first")
		}
	default:
		c, err := owner()
		if err != nil {
			return nil, "", err
		}
		d, err := c.GetSession(ctx, arg)
		if err != nil {
			return nil, "", fmt.Errorf("attach: %w", err)
		}
		return c, d.ID, nil
	}
	c, err := client.NewGuest(g.ServerURL, g.Token, api.HeaderActionAttach)
	if err != nil {
		return nil, "", err
	}
	c.SetTimeout(client.InteractiveTimeout)
	return c, g.Session, nil
}

// redeemLogin signs in with a login link the way a browser does: POST the
// link with the server's origin, keep the session cookie, and do not follow
// the redirect. It then asks the server for the one session the link allows.
func redeemLogin(ctx context.Context, link string, now time.Time) (guestLogin, error) {
	u, err := url.Parse(link)
	if err != nil || u.Host == "" || !strings.HasPrefix(u.Path, "/login/") {
		return guestLogin{}, fmt.Errorf("attach: %q is not a sessionhub login link", link)
	}
	origin := u.Scheme + "://" + u.Host
	ctx, cancel := context.WithTimeout(ctx, client.InteractiveTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+u.EscapedPath(), nil)
	if err != nil {
		return guestLogin{}, err
	}
	req.Header.Set("Origin", origin)
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Do(req)
	if err != nil {
		return guestLogin{}, fmt.Errorf("attach: %w", err)
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusSeeOther, http.StatusOK:
	case http.StatusGone:
		return guestLogin{}, errors.New("attach: the login link is used or expired; ask for a new one")
	case http.StatusConflict:
		return guestLogin{}, errors.New("attach: the link's browser name is in use; ask for a new link")
	case http.StatusForbidden:
		return guestLogin{}, fmt.Errorf("attach: the server refused the sign-in from %s; check that the link's address is the server's public URL", origin)
	default:
		return guestLogin{}, fmt.Errorf("attach: sign-in failed: HTTP %d", resp.StatusCode)
	}
	g := guestLogin{ServerURL: origin}
	for _, ck := range resp.Cookies() {
		if ck.Name != client.SessionCookie {
			continue
		}
		g.Token = ck.Value
		switch {
		case ck.MaxAge > 0:
			g.ExpiresAt = now.Add(time.Duration(ck.MaxAge) * time.Second)
		case !ck.Expires.IsZero():
			g.ExpiresAt = ck.Expires
		}
	}
	if g.Token == "" {
		return guestLogin{}, errors.New("attach: the server answered without a session cookie")
	}
	c, err := client.NewGuest(origin, g.Token, api.HeaderActionAttach)
	if err != nil {
		return guestLogin{}, err
	}
	c.SetTimeout(client.InteractiveTimeout)
	list, err := c.ListSessions(ctx, false, "")
	if err != nil {
		return guestLogin{}, fmt.Errorf("attach: %w", err)
	}
	if len(list) != 1 {
		return guestLogin{}, errors.New("attach: the link is not limited to one session; ask for a link made with sessionhub login --session")
	}
	g.Session = list[0].ID
	return g, nil
}

// saveGuest writes g to path, readable by the user only.
func saveGuest(path string, g guestLogin) error {
	b, err := json.Marshal(g)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600) // WriteFile keeps the mode of a file that exists
}

// mirrorAttach copies attach id's output to out and sends what it reads
// from in as input, until the output ends (detached false) or in sends
// Ctrl-] (detached true). Bytes before Ctrl-] in the same read are sent.
func mirrorAttach(ctx context.Context, c attachAPI, id string, in io.Reader, out io.Writer) (detached bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	body, err := c.AttachOutput(ctx, id)
	if err != nil {
		return false, err
	}
	defer body.Close()
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(out, body)
		done <- err
	}()
	// shortcut: the readKeys goroutine stays blocked on in after a return;
	// the process exits right after, so it does not leak for long.
	keys := make(chan []byte)
	go readKeys(in, keys)
	for {
		select {
		case err := <-done:
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, err
		case k, ok := <-keys:
			if !ok {
				keys = nil // end of input: keep showing output
				continue
			}
			i := bytes.IndexByte(k, detachKey)
			if i >= 0 {
				k = k[:i]
			}
			if len(k) > 0 {
				if err := c.SendAttachInput(ctx, id, k); err != nil {
					return false, err
				}
			}
			if i >= 0 {
				cancel()
				body.Close()
				<-done
				return true, nil
			}
		}
	}
}
