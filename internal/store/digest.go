package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"time"

	"github.com/abdallah/session-hub/internal/api"
)

// Digest limits. See docs/server.md, "Session digests".
const (
	MaxRecapRunes    = 600
	MaxSubjectRunes  = 120
	MaxDigestLinks   = 20
	MaxDigestCommits = 5
	MaxLinkBytes     = 300

	maxDigestCount   = 1_000_000_000_000
	maxCostUSD       = 100000
	digestFutureSkew = 5 * time.Minute
)

var (
	shaRE      = regexp.MustCompile(`^[0-9a-f]{4,40}$`)
	prNumberRE = regexp.MustCompile(`^[0-9]{1,10}$`)
)

func checkCount(name string, v, lo int64) error {
	if v < lo || v > maxDigestCount {
		return invalidf("%s is %d, want %d to %d", name, v, lo, int64(maxDigestCount))
	}
	return nil
}

func checkLink(name string, l api.DigestLink) error {
	if len(l.URL) > MaxLinkBytes {
		return invalidf("%s.url is %d bytes, at most %d allowed", name, len(l.URL), MaxLinkBytes)
	}
	if err := checkNoControl(name+".url", l.URL); err != nil {
		return err
	}
	u, err := url.Parse(l.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return invalidf("%s.url %q: want an https:// URL without user info", name, l.URL)
	}
	if l.Number != "" && !prNumberRE.MatchString(l.Number) {
		return invalidf("%s.number %q: want 1-10 digits", name, l.Number)
	}
	return checkText(name+".repo", l.Repo, MaxTitleRunes)
}

func validateDigest(d api.DigestIn, now time.Time) error {
	if d.AsOf.IsZero() {
		return invalidf("as_of is required")
	}
	if d.AsOf.After(now.Add(digestFutureSkew)) {
		return invalidf("as_of %s is more than %s ahead of the server clock", d.AsOf.UTC().Format(time.RFC3339), digestFutureSkew)
	}
	for _, f := range []struct {
		name, v string
		max     int
	}{{"recap", d.Recap, MaxRecapRunes}, {"ai_title", d.AITitle, MaxTitleRunes}, {"custom_title", d.CustomTitle, MaxTitleRunes}} {
		if err := checkText(f.name, f.v, f.max); err != nil {
			return err
		}
	}
	if len(d.Links) > MaxDigestLinks {
		return invalidf("links has %d items, at most %d allowed", len(d.Links), MaxDigestLinks)
	}
	for i, l := range d.Links {
		if err := checkLink(fmt.Sprintf("links[%d]", i), l); err != nil {
			return err
		}
	}
	for _, c := range []struct {
		name string
		v    int64
	}{{"tokens.input", d.Tokens.Input}, {"tokens.output", d.Tokens.Output},
		{"tokens.cache_read", d.Tokens.CacheRead}, {"tokens.cache_write", d.Tokens.CacheWrite},
		{"bad_lines", int64(d.BadLines)}} {
		if err := checkCount(c.name, c.v, 0); err != nil {
			return err
		}
	}
	if d.CostUSD != nil && (*d.CostUSD < 0 || *d.CostUSD > maxCostUSD) {
		return invalidf("cost_usd is %v, want 0 to %d", *d.CostUSD, maxCostUSD)
	}
	if g := d.Git; g != nil {
		if len(g.Commits) > MaxDigestCommits {
			return invalidf("git.commits has %d items, at most %d allowed", len(g.Commits), MaxDigestCommits)
		}
		for _, c := range []struct {
			name  string
			v, lo int64
		}{{"git.commit_count", int64(g.CommitCount), int64(len(g.Commits))},
			{"git.uncommitted", int64(g.Uncommitted), 0}, {"git.unpushed", int64(g.Unpushed), -1}} {
			if err := checkCount(c.name, c.v, c.lo); err != nil {
				return err
			}
		}
		for i, c := range g.Commits {
			if !shaRE.MatchString(c.SHA) {
				return invalidf("git.commits[%d].sha %q: want 4-40 lowercase hex digits", i, c.SHA)
			}
			if err := checkText(fmt.Sprintf("git.commits[%d].subject", i), c.Subject, MaxSubjectRunes); err != nil {
				return err
			}
		}
	}
	return nil
}

func sameLinks(a, b []api.DigestLink) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// PutDigest stores session id's digest unless the stored one has a newer
// as_of; then it returns stored=false and changes nothing. An equal as_of is
// stored again, so the hook child and the watcher can both send one.
//
// It never touches last_seen_at or ended_at: a backfilled digest for an
// ended session must not revive it.
func (s *Store) PutDigest(ctx context.Context, machineID int64, id string, d api.DigestIn) (stored bool, err error) {
	now := s.Now()
	if err := validateDigest(d, now); err != nil {
		return false, err
	}
	d.AsOf = d.AsOf.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := checkOwnerTx(ctx, tx, machineID, id); err != nil {
		return false, err
	}
	var prevAsOf, prevBody string
	var prev api.DigestIn
	hadPrev := false
	err = tx.QueryRowContext(ctx, `SELECT as_of, body FROM session_digests WHERE session_id = ?`, id).Scan(&prevAsOf, &prevBody)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, err
	default:
		if formatTS(d.AsOf) < prevAsOf {
			return false, nil
		}
		if err := json.Unmarshal([]byte(prevBody), &prev); err != nil {
			return false, fmt.Errorf("stored digest of %s: %w", id, err)
		}
		hadPrev = true
	}
	// A digest without git means git is unknown, not zero: keep the stored one.
	if d.Git == nil && prev.Git != nil {
		d.Git = prev.Git
	}
	body, err := json.Marshal(d)
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_digests (session_id, as_of, received_at, body)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET as_of = excluded.as_of, received_at = excluded.received_at, body = excluded.body`,
		id, formatTS(d.AsOf), formatTS(now), string(body)); err != nil {
		return false, err
	}
	if err := applyDigestTitle(ctx, tx, id, d, prev, hadPrev); err != nil {
		return false, err
	}
	if d.Recap != prev.Recap || !sameLinks(d.Links, prev.Links) {
		payload, _ := json.Marshal(struct {
			RecapAt *time.Time `json:"recap_at,omitempty"`
			Links   int        `json:"links"`
		}{d.RecapAt, len(d.Links)})
		if err := insertEventTx(ctx, tx, id, now, api.SourceServer, api.KindDigest, payload); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// applyDigestTitle applies the digest's titles. A custom title (from
// /rename) is applied only when it differs from the previous digest's, so a
// later set_title keeps winning. The automatic title fills in below herdr and
// user titles. Without a stored digest there is nothing to compare with, so a
// custom title doesn't replace a title the user already set.
func applyDigestTitle(ctx context.Context, tx *sql.Tx, id string, d, prev api.DigestIn, hadPrev bool) error {
	var cur string
	if err := tx.QueryRowContext(ctx, `SELECT title_source FROM sessions WHERE id = ?`, id).Scan(&cur); err != nil {
		return err
	}
	var title, src string
	switch {
	case d.CustomTitle != "" && d.CustomTitle != prev.CustomTitle && (hadPrev || cur != TitleSourceUser):
		title, src = d.CustomTitle, TitleSourceUser
	case d.AITitle != "" && titleRank(cur) <= titleRank(TitleSourceClaude):
		title, src = d.AITitle, TitleSourceClaude
	default:
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE sessions SET title = ?, title_source = ? WHERE id = ?`, title, src, id)
	return err
}

// summarize is the part of a digest the session list carries.
func summarize(d api.DigestIn) *api.SessionSummary {
	sum := &api.SessionSummary{AsOf: d.AsOf, CostUSD: d.CostUSD, OutputTokens: d.Tokens.Output}
	if d.Git != nil {
		g := d.Git.GitCounts
		sum.Git = &g
	}
	if n := len(d.Links); n > 0 {
		l := d.Links[n-1]
		sum.LatestLink = &l
	}
	return sum
}
