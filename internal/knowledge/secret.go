package knowledge

import "regexp"

var (
	secretRe = regexp.MustCompile(`AKIA[0-9A-Z]{16}|-----BEGIN |gh[pousr]_[A-Za-z0-9]{20,}|glpat-[A-Za-z0-9_-]{20,}`)
	runRe    = regexp.MustCompile(`[A-Za-z0-9+/=_-]{40,}`)
	digitRe  = regexp.MustCompile(`[0-9]`)
	lowerRe  = regexp.MustCompile(`[a-z]`)
	upperRe  = regexp.MustCompile(`[A-Z]`)
)

// LooksSecret reports whether text contains a likely credential: an AWS key
// ID, a PEM header, a GitHub or GitLab token, or a 40+ character base64-like
// run with a digit, a lowercase and an uppercase letter.
func LooksSecret(text string) bool {
	if secretRe.MatchString(text) {
		return true
	}
	for _, r := range runRe.FindAllString(text, -1) {
		if digitRe.MatchString(r) && lowerRe.MatchString(r) && upperRe.MatchString(r) {
			return true
		}
	}
	return false
}
