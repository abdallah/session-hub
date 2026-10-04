package digest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/abdallah/session-hub/internal/api"
	"github.com/abdallah/session-hub/internal/client"
)

// ErrEmpty: the transcript has no timed entry yet.
var ErrEmpty = errors.New("transcript has no timed entry yet")

// sendTimeout bounds one PUT; a slower server gets the digest from the queue.
const sendTimeout = 5 * time.Second

// Limits the server enforces. Build clamps to them so one corrupt transcript
// entry can't make the server reject every later digest.
const (
	maxFuture   = 5 * time.Minute
	maxCostUSD  = 100000
	maxTokenVal = int64(1e12)
)

// Builder reads a session's transcript and runs git.
type Builder struct {
	Reader Reader
	Git    func(ctx context.Context, cwd string, since, until time.Time) *api.DigestGit
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time
}

func (b Builder) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// Build returns the session's current digest, clamped to the server's
// limits. Git runs in the transcript's last working directory over the
// session's activity window.
func (b Builder) Build(ctx context.Context, id string) (api.DigestIn, error) {
	st, err := b.Reader.Read(id)
	if err != nil {
		return api.DigestIn{}, err
	}
	d, ok := st.DigestIn()
	if !ok {
		return d, ErrEmpty
	}
	d = b.clamp(d)
	if b.Git != nil && d.FirstAt != nil && st.CWD != "" {
		d.Git = b.Git(ctx, st.CWD, *d.FirstAt, d.AsOf)
	}
	return bound(d), nil
}

// maxBodyBytes keeps the marshalled digest under the server's 16 KiB cap.
const maxBodyBytes = 15 << 10

// bound shrinks d until its JSON body fits maxBodyBytes: it drops the oldest
// links first, then cuts the recap. Long repo values or escaped text in 20
// links could otherwise make the server answer 413 for good.
func bound(d api.DigestIn) api.DigestIn {
	size := func() int {
		b, _ := json.Marshal(d)
		return len(b)
	}
	for size() > maxBodyBytes && len(d.Links) > 0 {
		d.Links = d.Links[1:]
	}
	for r := []rune(d.Recap); size() > maxBodyBytes && len(r) > 0; {
		r = r[:len(r)/2]
		d.Recap = string(r)
	}
	return d
}

// clamp brings d inside the server's limits.
func (b Builder) clamp(d api.DigestIn) api.DigestIn {
	now := b.now()
	if d.AsOf.After(now.Add(maxFuture)) {
		d.AsOf = now.UTC()
	}
	limit := d.AsOf.Add(maxFuture)
	keep := func(t *time.Time) *time.Time {
		if t != nil && t.After(limit) {
			return nil
		}
		return t
	}
	d.FirstAt, d.RecapAt, d.CostAt = keep(d.FirstAt), keep(d.RecapAt), keep(d.CostAt)
	if d.CostUSD != nil && (*d.CostUSD < 0 || *d.CostUSD > maxCostUSD) {
		d.CostUSD = nil
	}
	for _, p := range []*int64{&d.Tokens.Input, &d.Tokens.Output, &d.Tokens.CacheRead, &d.Tokens.CacheWrite} {
		*p = min(max(*p, 0), maxTokenVal)
	}
	return d
}

// Send puts the digest, or queues it when the server can't take it now
// (network error, timeout, 408, 429, 5xx, or no usable client). A 404 or
// other 4xx is returned as an error and not queued.
func Send(ctx context.Context, c *client.Client, q *client.Queue, id string, d api.DigestIn) (queued bool, err error) {
	if c != nil {
		sctx, cancel := context.WithTimeout(ctx, sendTimeout)
		err = c.PutDigest(sctx, id, d)
		cancel()
		if err == nil {
			return false, nil
		}
		if !client.IsRetryable(err) {
			return false, err
		}
	}
	body, merr := json.Marshal(d)
	if merr != nil {
		return false, merr
	}
	if qerr := q.Append(client.Item{Op: client.OpDigest, SessionID: id, Body: body}); qerr != nil {
		return false, fmt.Errorf("send failed (%v) and queueing failed: %w", err, qerr)
	}
	return true, nil
}

// skipReason names the build errors that are not failures.
func skipReason(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrNoTranscript):
		return "no transcript", true
	case errors.Is(err, ErrLocked):
		return "another run is in progress", true
	case errors.Is(err, ErrEmpty):
		return "nothing to report yet", true
	}
	return "", false
}

type cmdEnv struct {
	out   io.Writer
	queue *client.Queue
	cfg   client.Config
	build Builder
}

// Run is `sessionhub digest <session-id>` and `sessionhub digest --all`.
func Run(ctx context.Context, args []string) error {
	cfg, err := client.LoadConfig()
	if err != nil {
		return err
	}
	e := &cmdEnv{out: os.Stdout, queue: client.DefaultQueue(), cfg: cfg, build: Builder{Git: Git}}
	return e.run(ctx, args)
}

const usageMsg = "usage: sessionhub digest <session-id> | sessionhub digest --all"

func (e *cmdEnv) run(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New(usageMsg)
	}
	c, cerr := client.New(e.cfg)
	if args[0] == "--all" {
		if cerr != nil {
			return cerr
		}
		return e.all(ctx, c)
	}
	if strings.HasPrefix(args[0], "-") {
		return errors.New(usageMsg)
	}
	if cerr != nil {
		c = nil // no usable client: the digest is queued
	}
	id := args[0]
	res, err := e.one(ctx, c, id)
	if res == "skipped: no transcript" && c != nil && len(id) < fullIDLen {
		// sessionhub ls prints 8-character IDs: resolve a prefix to the full ID.
		d, gerr := c.GetSession(ctx, id)
		if gerr != nil {
			fmt.Fprintf(e.out, "%s  failed: %v\n", short(id), gerr)
			return gerr
		}
		id = d.ID
		res, err = e.one(ctx, c, id)
	}
	fmt.Fprintf(e.out, "%s  %s\n", short(id), res)
	return err
}

// one builds and sends one digest and describes the result.
func (e *cmdEnv) one(ctx context.Context, c *client.Client, id string) (string, error) {
	d, err := e.build.Build(ctx, id)
	if reason, ok := skipReason(err); ok {
		return "skipped: " + reason, nil
	}
	if err != nil {
		return "failed: " + err.Error(), err
	}
	queued, err := Send(ctx, c, e.queue, id, d)
	switch {
	case err != nil && client.IsNotFound(err):
		return "failed: sessionhub doesn't know this session", err
	case err != nil:
		return "failed: " + err.Error(), err
	case queued:
		return "queued", nil
	}
	return "sent", nil
}

// all sends a digest for every session the server knows on this machine.
func (e *cmdEnv) all(ctx context.Context, c *client.Client) error {
	if e.cfg.Machine == "" {
		return errors.New("sessionhub digest --all: the client config has no machine name; run sessionhub join first")
	}
	list, err := c.ListSessions(ctx, false, e.cfg.Machine)
	if err != nil {
		return err
	}
	var sent, skipped, failed int
	for _, s := range list {
		res, err := e.one(ctx, c, s.ID)
		fmt.Fprintf(e.out, "%s  %s\n", short(s.ID), res)
		switch {
		case err != nil:
			failed++
		case strings.HasPrefix(res, "skipped"):
			skipped++
		default:
			sent++ // queued counts as sent
		}
	}
	fmt.Fprintf(e.out, "%d sent, %d skipped, %d failed\n", sent, skipped, failed)
	if failed > 0 {
		return fmt.Errorf("%d digests failed", failed)
	}
	return nil
}

// fullIDLen is the length of a full session ID (a UUID).
const fullIDLen = 36

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
