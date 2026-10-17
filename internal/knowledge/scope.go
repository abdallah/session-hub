// Package knowledge holds pure helpers for the knowledge layer: scope
// normalization and the secret check.
package knowledge

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var (
	ticketRe = regexp.MustCompile(`^[a-z][a-z0-9]+-\d+$`)
	mrRe     = regexp.MustCompile(`^[^/!\s]+(/[^/!\s]+)+!\d+$`)
	branchRe = regexp.MustCompile(`(?i)\b[a-z][a-z0-9]+-\d+\b`)
)

// NormalizeRepo turns a git origin URL into lowercase host/path with no
// scheme, user, port or .git suffix. It returns "" for empty or unparsable input.
func NormalizeRepo(originURL string) string {
	s := strings.TrimSpace(originURL)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") { // scp form user@host:path
		s = "ssh://" + strings.Replace(s, ":", "/", 1)
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	p := strings.Trim(strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"), "/")
	if p == "" {
		return ""
	}
	return strings.ToLower(u.Hostname() + "/" + p)
}

// NormalizeRef lowercases a ticket ref (systemsdev-123) or an MR ref
// (group/project!34). It returns "" for anything else, including a bare !34.
func NormalizeRef(ref string) string {
	r := strings.ToLower(strings.TrimSpace(ref))
	if ticketRe.MatchString(r) || mrRe.MatchString(r) {
		return r
	}
	return ""
}

// ParseScope splits a scope string into its kind and normalized value.
// Accepted forms: global, repo:<url>, ticket:<ref>, machine:<name>.
func ParseScope(s string) (kind, value string, err error) {
	if s == "global" {
		return "global", "", nil
	}
	kind, value, _ = strings.Cut(s, ":")
	switch kind {
	case "repo":
		value = NormalizeRepo(value)
	case "ticket":
		value = NormalizeRef(value)
	case "machine":
		value = strings.TrimSpace(value)
	default:
		return "", "", fmt.Errorf("invalid scope %q", s)
	}
	if value == "" {
		return "", "", fmt.Errorf("invalid scope %q", s)
	}
	return kind, value, nil
}

// RefsInBranch returns the normalized, deduplicated ticket refs in a branch name.
func RefsInBranch(branch string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range branchRe.FindAllString(branch, -1) {
		m = strings.ToLower(m)
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}
