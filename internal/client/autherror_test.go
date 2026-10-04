package client

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsAuthError(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"401", &StatusError{Status: 401}, true},
		{"403", &StatusError{Status: 403}, true},
		{"wrapped 401", fmt.Errorf("wrap: %w", &StatusError{Status: 401}), true},
		{"400", &StatusError{Status: 400}, false},
		{"404", &StatusError{Status: 404}, false},
		{"409", &StatusError{Status: 409}, false},
		{"500", &StatusError{Status: 500}, false},
		{"network", errors.New("dial tcp: refused"), false},
	} {
		if got := IsAuthError(c.err); got != c.want {
			t.Errorf("%s: IsAuthError = %v, want %v", c.name, got, c.want)
		}
	}
}
