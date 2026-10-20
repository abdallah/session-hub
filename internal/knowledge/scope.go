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
	bareMRRe = regexp.MustCompile(`^!\d+$`)
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

// SessionRef normalizes a task ref for session_refs: what NormalizeRef
// accepts, plus a bare MR ref (!34), which RefInRepo completes in the hook.
func SessionRef(ref string) string {
	if r := strings.TrimSpace(ref); bareMRRe.MatchString(r) {
		return r
	}
	return NormalizeRef(ref)
}

// RefInRepo completes a bare MR ref (!34) with the path of repo, a
// NormalizeRepo result, giving group/project!34. Other refs pass through
// unchanged; a bare MR ref with no repo gives "".
func RefInRepo(ref, repo string) string {
	if !bareMRRe.MatchString(ref) {
		return ref
	}
	if _, path, ok := strings.Cut(repo, "/"); ok {
		return path + ref
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
