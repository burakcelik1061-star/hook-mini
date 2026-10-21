package delivery

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func FuzzClean(f *testing.F) {
	for _, s := range []string{"", "ok", "a\x00b", "\xff\xfe", strings.Repeat("é", 3000), " ", "🙂🙂🙂"} {
		f.Add(s, 5)
	}
	f.Fuzz(func(t *testing.T, s string, max int) {
		if max < 1 || max > 4096 {
			return
		}
		got := clean(s, max)
		// Exactly what a Postgres text column requires.
		if !utf8.ValidString(got) {
			t.Fatalf("clean(%q, %d) = %q is not valid UTF-8", s, max, got)
		}
		if strings.ContainsRune(got, 0) {
			t.Fatalf("clean(%q, %d) still contains a NUL byte", s, max)
		}
		if n := utf8.RuneCountInString(got); n > max {
			t.Fatalf("clean(%q, %d) has %d characters", s, max, n)
		}
		// Cleaning twice changes nothing.
		if again := clean(got, max); again != got {
			t.Fatalf("clean is not idempotent: %q then %q", got, again)
		}
	})
}

func FuzzParseRetryAfter(f *testing.F) {
	for _, s := range []string{"", "0", "120", "-1", "99999999999999999999", "Wed, 21 Oct 2026 07:28:00 GMT", "1e3", " 5 ", "\x00"} {
		f.Add(s)
	}
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, s string) {
		d := parseRetryAfter(s, now)
		if d < 0 || d > maxRetryAfter { // both the seconds and the date form are clamped
			t.Fatalf("parseRetryAfter(%q) = %s is out of range", s, d)
		}
		// Whatever the endpoint sends, the policy bounds the wait.
		p := RetryPolicy{MaxAttempts: 5, InitialBackoff: time.Second, MaxBackoff: time.Hour, Rand: func() float64 { return 1 }}
		if w := p.nextDelay(3, d); w < 0 || w > time.Hour {
			t.Fatalf("nextDelay with Retry-After %q = %s, want within the cap", s, w)
		}
	})
}

func FuzzBackoff(f *testing.F) {
	f.Add(1, 0.0)
	f.Add(1<<30, 0.999)
	f.Add(-5, 0.5)
	f.Fuzz(func(t *testing.T, n int, r float64) {
		if r < 0 || r >= 1 || r != r {
			return
		}
		p := RetryPolicy{MaxAttempts: 8, InitialBackoff: 30 * time.Second, MaxBackoff: 6 * time.Hour, Rand: func() float64 { return r }}
		d := p.Backoff(n)
		if d < p.InitialBackoff/2 || d > p.MaxBackoff {
			t.Fatalf("Backoff(%d) with jitter %v = %s, outside [%s, %s]", n, r, d, p.InitialBackoff/2, p.MaxBackoff)
		}
	})
}
