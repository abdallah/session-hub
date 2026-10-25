package plugin

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"time"

	"github.com/abdallah/session-hub/internal/client"
	"github.com/abdallah/session-hub/internal/move"
)

// Snapshot timing. See docs/plugin.md, "Snapshots".
const (
	// snapshotEvery is the shortest gap between two snapshots of a session.
	snapshotEvery = 10 * time.Minute
	// snapshotTimeout bounds one session's snapshots: build, seal, upload.
	snapshotTimeout = 5 * time.Minute
)

// snapCand is a session the heartbeat saw idle or done in a pane here.
type snapCand struct{ id, cwd string }

// snapper uploads sealed snapshots of this machine's sessions, one per
// other machine with a move key, so a move can take a session while this
// machine is offline. It runs beside the watcher's ticks: a snapshot can
// take seconds.
type snapper struct {
	machine   string // this machine's name, as the server knows it
	claudeDir string
	roots     []string
	claudeBin string
	key       *ecdh.PrivateKey
	newClient func() (*client.Client, error)
	log       *log.Logger
	now       func() time.Time
	every     time.Duration
	// stat reports a session's transcript size and modification time.
	stat func(id string) (int64, time.Time, bool)
	jobs chan []snapCand
	last map[string]snapMark // session ID → its last uploaded snapshot; the loop's goroutine only
}

// snapMark is a session's transcript when its last snapshot was uploaded.
type snapMark struct {
	mark transcriptMark
	at   time.Time
}

func newSnapper(machine, claudeDir string, roots []string, key *ecdh.PrivateKey, newClient func() (*client.Client, error),
	stat func(string) (int64, time.Time, bool), logger *log.Logger) *snapper {
	return &snapper{machine: machine, claudeDir: claudeDir, roots: roots, claudeBin: "claude", key: key, newClient: newClient,
		log: logger, now: time.Now, every: snapshotEvery, stat: stat, jobs: make(chan []snapCand, 1), last: map[string]snapMark{}}
}

// offer hands the loop the sessions to check, unless it is still busy with
// the last ones.
func (sn *snapper) offer(cands []snapCand) {
	select {
	case sn.jobs <- cands:
	default:
	}
}

// loop takes snapshots until ctx ends.
func (sn *snapper) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case cands := <-sn.jobs:
			sn.run(ctx, cands)
		}
	}
}

// run snapshots each candidate whose transcript changed since its last
// snapshot, at most once per sn.every.
func (sn *snapper) run(ctx context.Context, cands []snapCand) {
	seen := map[string]bool{}
	var due []snapCand
	for _, c := range cands {
		seen[c.id] = true
		size, mod, ok := sn.stat(c.id)
		if !ok {
			continue
		}
		last, had := sn.last[c.id]
		if had && (last.mark == transcriptMark{size, mod} || sn.now().Sub(last.at) < sn.every) {
			continue
		}
		due = append(due, c)
	}
	for id := range sn.last {
		if !seen[id] {
			delete(sn.last, id)
		}
	}
	if len(due) == 0 {
		return
	}
	cl, err := sn.newClient()
	if err != nil {
		sn.log.Printf("snapshot: %s", cleanErr(err))
		return
	}
	peers, err := sn.recipients(ctx, cl)
	if err != nil {
		sn.log.Printf("snapshot: list machines: %s", cleanErr(err))
		return
	}
	if len(peers) == 0 {
		return
	}
	version, err := move.ClaudeVersion(ctx, sn.claudeBin)
	if err != nil {
		sn.log.Printf("snapshot: %s", cleanErr(err))
		return
	}
	for _, c := range due {
		// The mark is read before the build, so a turn that ends during it
		// is snapshotted next time.
		size, mod, _ := sn.stat(c.id)
		sctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
		err := sn.snapshot(sctx, cl, c, version, peers)
		cancel()
		if err != nil {
			sn.log.Printf("snapshot %s: %s", c.id, cleanErr(err))
		}
		// A failure waits like a success, so a bundle that can't be built
		// is not rebuilt on every heartbeat.
		sn.last[c.id] = snapMark{transcriptMark{size, mod}, sn.now()}
	}
}

// peer is a machine a snapshot is sealed for.
type peer struct {
	name string
	key  *ecdh.PublicKey
}

// recipients lists the other machines with a move key.
func (sn *snapper) recipients(ctx context.Context, cl *client.Client) ([]peer, error) {
	ms, err := cl.ListMachines(ctx)
	if err != nil {
		return nil, err
	}
	var out []peer
	for _, m := range ms {
		if m.Name == sn.machine || m.MovePublicKey == "" {
			continue
		}
		k, err := move.ParsePublicKey(m.MovePublicKey)
		if err != nil {
			sn.log.Printf("snapshot: machine %s's key: %v", m.Name, err)
			continue
		}
		out = append(out, peer{m.Name, k})
	}
	return out, nil
}

// snapshot builds, seals, and uploads one snapshot of c for each peer. Each
// gets its own ID, since the manifest names the ID it is sealed under.
func (sn *snapper) snapshot(ctx context.Context, cl *client.Client, c snapCand, version string, peers []peer) error {
	repo, err := move.ReadRepo(ctx, c.cwd, sn.roots)
	if err != nil {
		return fmt.Errorf("read repo: %w", err)
	}
	for _, p := range peers {
		id := newSnapshotID()
		data, _, err := move.Build(ctx, move.BuildInput{MoveID: id, SessionID: c.id, Machine: sn.machine, CWD: c.cwd,
			ClaudeDir: sn.claudeDir, ClaudeVersion: version, Repo: repo, Now: sn.now()})
		if err != nil {
			return fmt.Errorf("build bundle: %w", err)
		}
		sealed, err := move.Seal(sn.key, p.key, id, data)
		if err != nil {
			return fmt.Errorf("seal: %w", err)
		}
		if err := cl.PutSnapshot(ctx, c.id, p.name, id, bytes.NewReader(sealed), int64(len(sealed))); err != nil {
			return fmt.Errorf("upload for %s: %w", p.name, err)
		}
	}
	return nil
}

// newSnapshotID is a seal ID in the move ID shape: mv_ and 16 random bytes.
func newSnapshotID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return "mv_" + base64.RawURLEncoding.EncodeToString(b[:])
}

// snapCands lists the sessions in the pane cache that are idle or done, each
// once.
func (w *watcher) snapCands() []snapCand {
	seen := map[string]bool{}
	var out []snapCand
	for _, e := range w.cache {
		u := e.u
		if u.ID == "" || u.CWD == "" || seen[u.ID] || (u.AgentState != "idle" && u.AgentState != "done") {
			continue
		}
		seen[u.ID] = true
		out = append(out, snapCand{u.ID, u.CWD})
	}
	return out
}
