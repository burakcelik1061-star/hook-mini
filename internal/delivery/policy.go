// Package delivery is the worker side of Hook Mini: it claims queued
// deliveries, sends them to their endpoints and records the outcome, retrying
// failures with exponential backoff until they succeed or run out of attempts.
//
// Guarantees, and how they are kept:
//
//   - A delivery is never handed to two workers at once: claiming is a single
//     UPDATE over rows selected FOR UPDATE SKIP LOCKED.
//   - A worker that crashes mid-attempt loses nothing: its lease expires and
//     another worker reclaims the delivery. The lost attempt is counted, so a
//     delivery that crashes every worker still ends after MaxAttempts.
//   - A late result never overwrites a newer one: every write is fenced by the
//     lease token handed out with the claim.
//
// Delivery is at least once. A request that reached the endpoint but whose
// result was lost (crash, timeout after the endpoint processed it) is sent
// again; receivers deduplicate on the webhook-id header.
package delivery

import (
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// backoffMultiplier grows the wait between attempts: 30s, 2m, 8m, 32m, 2h8m...
const backoffMultiplier = 4

// RetryPolicy decides how often and how far apart a delivery is retried.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts, the first one included.
	MaxAttempts int
	// InitialBackoff is the base wait after the first failure.
	InitialBackoff time.Duration
	// MaxBackoff caps any single wait, Retry-After included.
	MaxBackoff time.Duration
	// Rand returns a number in [0, 1) for jitter. Nil means math/rand/v2.
	Rand func() float64
}

// DefaultRetryPolicy gives 8 attempts spread over roughly 15 hours, enough to
// ride out a typical outage on the receiving side.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 8, InitialBackoff: 30 * time.Second, MaxBackoff: 6 * time.Hour}
}

// Validate reports a policy that cannot work.
func (p RetryPolicy) Validate() error {
	switch {
	case p.MaxAttempts < 1:
		return errors.New("max attempts must be at least 1")
	case p.InitialBackoff <= 0:
		return errors.New("initial backoff must be positive")
	case p.MaxBackoff < p.InitialBackoff:
		return errors.New("max backoff must not be smaller than the initial backoff")
	}
	return nil
}

// Backoff returns the wait after the n-th consecutive failed attempt (n >= 1).
//
// The base doubles twice per attempt up to MaxBackoff, and the result is drawn
// uniformly from [base/2, base]. Jitter spreads retries of many deliveries
// that failed together (an endpoint outage) so they do not all hit the
// endpoint again in the same instant; keeping the lower half avoids retrying
// much sooner than intended.
func (p RetryPolicy) Backoff(n int) time.Duration {
	base := float64(p.InitialBackoff)
	for i := 1; i < n && base < float64(p.MaxBackoff); i++ {
		base *= backoffMultiplier
	}
	base = min(base, float64(p.MaxBackoff))

	r := rand.Float64
	if p.Rand != nil {
		r = p.Rand
	}
	return time.Duration(base/2 + r()*base/2)
}

// nextDelay is the wait before retrying after the n-th failed attempt. A
// Retry-After from the endpoint can only lengthen it, never shorten it, and
// MaxBackoff bounds both.
func (p RetryPolicy) nextDelay(n int, retryAfter time.Duration) time.Duration {
	return min(max(p.Backoff(n), retryAfter), p.MaxBackoff)
}

// parseRetryAfter reads a Retry-After header: either delay-seconds or an
// HTTP date. It returns 0 when the header is absent, invalid or in the past.
func parseRetryAfter(h string, now time.Time) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(h, 10, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		// Clamp absurd values before converting, so they cannot overflow.
		return time.Duration(min(secs, int64(366*24*time.Hour/time.Second))) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
