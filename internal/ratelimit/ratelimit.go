// Package ratelimit limits how fast each tenant may call the API, so one
// tenant cannot crowd out the others or flood the delivery queue.
//
// It is a token bucket per key, kept in the memory of the api process: a
// database write per request would lower the very ceiling the load
// measurements found (commit speed), and the service deliberately runs
// without Redis. The price is that each api instance enforces the limit on
// its own: with N instances behind a load balancer a tenant gets up to N
// times the limit.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Limiter holds one bucket per key. The zero value is not usable; use New.
// It is safe for concurrent use.
type Limiter struct {
	rate  float64 // tokens added per second
	burst float64 // bucket size
	now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// Decision is the outcome of one Allow call.
type Decision struct {
	Allowed bool
	// Limit is the bucket size, Remaining the whole tokens left after this
	// request; both go into the RateLimit-* response headers.
	Limit, Remaining int
	// RetryAfter is how long until a request would be allowed again (zero
	// when it was allowed).
	RetryAfter time.Duration
}

// New returns a limiter that allows rate requests per second per key on
// average, and up to burst at once. now may be nil (time.Now).
func New(rate float64, burst int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{rate: rate, burst: float64(burst), now: now, buckets: map[string]*bucket{}}
}

// Allow takes one token from key's bucket if there is one. A key seen for the
// first time starts with a full bucket.
//
// Memory grows with the number of distinct keys. Keys are tenant ids, which
// only an operator creates, so the map stays small and is never pruned.
func (l *Limiter) Allow(key string) Decision {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = math.Min(l.burst, b.tokens+elapsed.Seconds()*l.rate)
	}
	b.last = now

	d := Decision{Limit: int(l.burst)}
	if b.tokens >= 1 {
		b.tokens--
		d.Allowed = true
		d.Remaining = int(b.tokens)
		return d
	}
	missing := 1 - b.tokens
	d.RetryAfter = time.Duration(missing / l.rate * float64(time.Second))
	return d
}
