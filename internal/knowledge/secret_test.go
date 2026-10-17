package knowledge

import (
	"strings"
	"testing"
)

func TestLooksSecret(t *testing.T) {
	no := []string{"Account 823963319113 is enrolled", "arn:aws:iam::823963319113:role/x", "token is in prod/telegram-bot"}
	yes := []string{"AKIAABCDEFGHIJKLMNOP", "-----BEGIN OPENSSH PRIVATE KEY-----", "ghp_0123456789abcdefghijABCDEFGHIJ012345", "glpat-abcdefghij0123456789", strings.Repeat("aB3+", 11)}
	for _, s := range no {
		if LooksSecret(s) {
			t.Errorf("LooksSecret(%q) = true", s)
		}
	}
	for _, s := range yes {
		if !LooksSecret(s) {
			t.Errorf("LooksSecret(%q) = false", s)
		}
	}
}
