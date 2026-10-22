package ratelimit

import (
	"sync"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTest(rate float64, burst int) (*Limiter, *clock) {
	c := &clock{t: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	return New(rate, burst, c.now), c
}

// A full bucket allows a burst, then refuses, then refills at the rate.
func TestBurstThenRate(t *testing.T) {
	l, c := newTest(10, 5)
	for i := range 5 {
		d := l.Allow("a")
		if !d.Allowed || d.Remaining != 4-i || d.Limit != 5 {
			t.Fatalf("request %d of the burst: %+v", i+1, d)
		}
	}
	d := l.Allow("a")
	if d.Allowed {
		t.Fatal("the 6th request in the same instant was allowed")
	}
	if d.RetryAfter != 100*time.Millisecond { // one token at 10/s
		t.Errorf("RetryAfter = %s, want 100ms", d.RetryAfter)
	}

	c.advance(99 * time.Millisecond)
	if l.Allow("a").Allowed {
		t.Error("allowed before a whole token had come back")
	}
	c.advance(time.Millisecond)
	if !l.Allow("a").Allowed {
		t.Error("refused after a token had come back")
	}
}

// Over a long run the average is the rate, whatever the burst.
func TestSustainedRate(t *testing.T) {
	l, c := newTest(20, 5)
	allowed := 0
	for range 10_000 { // 10 s in 1 ms steps, asking every millisecond
		if l.Allow("a").Allowed {
			allowed++
		}
		c.advance(time.Millisecond)
	}
	// 5 from the initial burst + 20/s for 10 s.
	if allowed < 200 || allowed > 206 {
		t.Errorf("allowed %d requests in 10 s at 20/s (burst 5), want about 205", allowed)
	}
}

// Idle time refills the bucket only up to its size.
func TestRefillIsCappedAtTheBurst(t *testing.T) {
	l, c := newTest(10, 3)
	l.Allow("a")
	c.advance(time.Hour)
	n := 0
	for n < 1000 && l.Allow("a").Allowed { // bounded: a broken limiter must fail, not hang
		n++
	}
	if n != 3 {
		t.Errorf("after an hour idle, %d requests passed at once, want the burst (3)", n)
	}
}

// Keys do not share a bucket.
func TestKeysAreIndependent(t *testing.T) {
	l, _ := newTest(1, 1)
	if !l.Allow("a").Allowed || l.Allow("a").Allowed {
		t.Fatal("setup: a's single token")
	}
	if !l.Allow("b").Allowed {
		t.Error("b was refused because a had used its token")
	}
}

// A clock that goes backwards (it should not, but) does not mint tokens.
func TestClockGoingBackwardsAddsNothing(t *testing.T) {
	l, c := newTest(10, 1)
	l.Allow("a")
	c.advance(-time.Hour)
	if l.Allow("a").Allowed {
		t.Error("a clock moving backwards produced a token")
	}
	// Nor does it take tokens away: from the new "now" the bucket refills at
	// the normal rate, instead of first paying back the hour.
	c.advance(100 * time.Millisecond)
	if !l.Allow("a").Allowed {
		t.Error("after the clock went back, the bucket did not refill normally")
	}
}

func TestConcurrentUseNeverOverspends(t *testing.T) {
	l, _ := newTest(0.000001, 50) // effectively no refill during the test
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				if l.Allow("a").Allowed {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if allowed != 50 {
		t.Errorf("%d of 200 concurrent requests passed a bucket of 50", allowed)
	}
}
