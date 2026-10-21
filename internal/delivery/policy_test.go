package delivery

import (
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func fixedRand(v float64) func() float64 { return func() float64 { return v } }

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 10, InitialBackoff: 30 * time.Second, MaxBackoff: 6 * time.Hour}

	// With jitter at its maximum the wait is the full base: 30s, 2m, 8m, 32m,
	// 2h8m, then the 6h cap.
	p.Rand = fixedRand(1)
	want := []time.Duration{
		30 * time.Second, 2 * time.Minute, 8 * time.Minute, 32 * time.Minute,
		2*time.Hour + 8*time.Minute, 6 * time.Hour, 6 * time.Hour,
	}
	for i, w := range want {
		if got := p.Backoff(i + 1); got != w {
			t.Errorf("Backoff(%d) = %s, want %s", i+1, got, w)
		}
	}

	// With jitter at its minimum the wait is half the base, never less.
	p.Rand = fixedRand(0)
	if got := p.Backoff(1); got != 15*time.Second {
		t.Errorf("minimum jitter: Backoff(1) = %s, want 15s", got)
	}

	// A very large attempt number must neither overflow nor exceed the cap.
	p.Rand = fixedRand(1)
	for _, n := range []int{50, 1000, 1 << 30} {
		if got := p.Backoff(n); got != 6*time.Hour {
			t.Errorf("Backoff(%d) = %s, want the 6h cap", n, got)
		}
	}
}

func TestBackoffJitterStaysInRange(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 5, InitialBackoff: time.Second, MaxBackoff: time.Hour}
	for n := 1; n <= 5; n++ {
		base := time.Second
		for i := 1; i < n; i++ {
			base *= backoffMultiplier
		}
		for range 200 {
			if got := p.Backoff(n); got < base/2 || got > base {
				t.Fatalf("Backoff(%d) = %s, outside [%s, %s]", n, got, base/2, base)
			}
		}
	}
}

func TestNextDelayRespectsRetryAfterWithinTheCap(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 5, InitialBackoff: 10 * time.Second, MaxBackoff: time.Hour, Rand: fixedRand(1)}

	if got := p.nextDelay(1, 0); got != 10*time.Second {
		t.Errorf("no Retry-After: %s, want the backoff 10s", got)
	}
	if got := p.nextDelay(1, 2*time.Minute); got != 2*time.Minute {
		t.Errorf("longer Retry-After: %s, want 2m", got)
	}
	if got := p.nextDelay(1, time.Second); got != 10*time.Second {
		t.Errorf("shorter Retry-After must not speed up retries: %s, want 10s", got)
	}
	if got := p.nextDelay(1, 48*time.Hour); got != time.Hour {
		t.Errorf("Retry-After beyond the cap: %s, want 1h", got)
	}
}

func TestRetryPolicyValidate(t *testing.T) {
	ok := RetryPolicy{MaxAttempts: 1, InitialBackoff: time.Second, MaxBackoff: time.Second}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid policy rejected: %v", err)
	}
	for name, p := range map[string]RetryPolicy{
		"no attempts":       {MaxAttempts: 0, InitialBackoff: time.Second, MaxBackoff: time.Minute},
		"no backoff":        {MaxAttempts: 3, InitialBackoff: 0, MaxBackoff: time.Minute},
		"cap below initial": {MaxAttempts: 3, InitialBackoff: time.Minute, MaxBackoff: time.Second},
	} {
		if p.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tests := map[string]time.Duration{
		"":               0,
		"120":            120 * time.Second,
		" 5 ":            5 * time.Second,
		"0":              0,
		"-5":             0,
		"soon":           0,
		"1.5":            0,
		"99999999999999": 366 * 24 * time.Hour, // clamped, no overflow
		// A far-future date used to come back as ~292 years (found by fuzzing).
		"Wed, 01 Oct 7000 00:00:00 GMT":                   366 * 24 * time.Hour,
		now.Add(90 * time.Second).Format(http.TimeFormat): 90 * time.Second,
		now.Add(-time.Hour).Format(http.TimeFormat):       0, // in the past
	}
	for in, want := range tests {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestCleanMakesTextStorable(t *testing.T) {
	in := "ok\x00 \xff\xfe broken " + strings.Repeat("é", 3000)
	got := clean(in, 1024)
	if !utf8.ValidString(got) {
		t.Error("result is not valid UTF-8")
	}
	if strings.ContainsRune(got, 0) {
		t.Error("result still contains a NUL byte, which Postgres text rejects")
	}
	if n := utf8.RuneCountInString(got); n != 1024 {
		t.Errorf("result has %d characters, want 1024", n)
	}
	if !strings.HasPrefix(got, "ok �") {
		t.Errorf("unexpected start: %q", got[:20])
	}

	if got := clean("short", 1024); got != "short" {
		t.Errorf("short text changed: %q", got)
	}
	// Cutting must not split a multi-byte character.
	if got := clean(strings.Repeat("ğ", 10), 3); got != "ğğğ" {
		t.Errorf("clean(10×ğ, 3) = %q", got)
	}
}
