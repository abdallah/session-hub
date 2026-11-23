package main

import (
	"strings"
	"testing"
)

func TestFirstNameserver(t *testing.T) {
	for in, want := range map[string]string{
		"# termux\nsearch lan\nnameserver 8.8.8.8\nnameserver 8.8.4.4\n": "8.8.8.8",
		"nameserver   2001:4860:4860::8888":                              "2001:4860:4860::8888",
		"search lan\n":                                                   "",
		"":                                                               "",
	} {
		if got := firstNameserver(strings.NewReader(in)); got != want {
			t.Errorf("firstNameserver(%q) = %q, want %q", in, got, want)
		}
	}
}
