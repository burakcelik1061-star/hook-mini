package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateKeepsCharactersWhole(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly", 7, "exactly"},
		{"abcdef", 3, "abc..."},
		{"aş", 2, "a..."},    // "ş" is 2 bytes; cutting at 2 would split it
		{"a€b", 3, "a..."},   // "€" is 3 bytes
		{"€", 2, "..."},      // nothing whole fits
		{"ab🙂c", 5, "ab..."}, // a 4-byte character
		{"ab🙂c", 6, "ab🙂..."},
	}
	for _, tt := range tests {
		got := truncate(tt.in, tt.n)
		if got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
	}
	long := strings.Repeat("ş", 300)
	for n := range 20 {
		if got := truncate(long, 480+n); !utf8.ValidString(got) {
			t.Errorf("truncate(..., %d) is not valid UTF-8: %q", 480+n, got[len(got)-8:])
		}
	}
}
