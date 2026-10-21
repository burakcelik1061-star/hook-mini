package delivery

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestClaimUnderHeavyContention hammers the claim query from many goroutines
// at once (more claimers than connections would allow without waiting) and
// checks the property everything else rests on: every due delivery is handed
// out exactly once, none twice, none lost, and each carries its own lease.
func TestClaimUnderHeavyContention(t *testing.T) {
	f := newFixture(t)
	f.endpoint("https://example.invalid/contention")
	const deliveries = 400
	f.events(deliveries)

	cfg := testConfig("w-contention")
	cfg.LeaseDuration, cfg.RequestTimeout = time.Minute, time.Second
	cfg.Concurrency, cfg.BatchSize = 8, 8

	const claimers = 16
	var (
		wg     sync.WaitGroup
		start  = make(chan struct{})
		mu     sync.Mutex
		owner  = map[uuid.UUID]string{} // delivery -> worker that got it
		tokens = map[uuid.UUID]bool{}
		dupes  []string
		errs   []error
	)
	for i := range claimers {
		wg.Add(1)
		id := fmt.Sprintf("claimer-%02d", i)
		w := f.worker(func() Config { c := cfg; c.ID = id; return c }())
		go func() {
			defer wg.Done()
			<-start
			for {
				jobs, err := w.claim(context.Background(), 5)
				mu.Lock()
				if err != nil {
					errs = append(errs, err)
					mu.Unlock()
					return
				}
				for _, j := range jobs {
					if prev, seen := owner[j.ID]; seen {
						dupes = append(dupes, fmt.Sprintf("%s claimed by both %s and %s", j.ID, prev, id))
					}
					owner[j.ID] = id
					if tokens[j.LeaseToken] {
						dupes = append(dupes, fmt.Sprintf("lease token %s handed out twice", j.LeaseToken))
					}
					tokens[j.LeaseToken] = true
				}
				mu.Unlock()
				if len(jobs) == 0 {
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("a claim failed under contention: %v", errs[0])
	}
	if len(dupes) > 0 {
		t.Fatalf("%d violations, first: %s", len(dupes), dupes[0])
	}
	if len(owner) != deliveries {
		t.Errorf("%d of %d deliveries were claimed", len(owner), deliveries)
	}
	byWorker := map[string]int{}
	for _, w := range owner {
		byWorker[w]++
	}
	if len(byWorker) < 2 {
		t.Errorf("only %d claimers got any work: the test did not create real contention", len(byWorker))
	}

	// What the database says agrees with what the claimers saw.
	var inFlight, withToken int
	if err := testPool.QueryRow(f.ctx, `SELECT count(*) FILTER (WHERE status = 'in_flight'), count(DISTINCT lease_token)
		FROM deliveries WHERE tenant_id = $1`, f.tenant).Scan(&inFlight, &withToken); err != nil {
		t.Fatal(err)
	}
	if inFlight != deliveries || withToken != deliveries {
		t.Errorf("database: %d in flight, %d distinct lease tokens, want %d of each", inFlight, withToken, deliveries)
	}
}

// Reaping from several workers at once must not reclaim a delivery twice
// (which would count a lost attempt twice and break attempt_count).
func TestConcurrentReapersReclaimEachLeaseOnce(t *testing.T) {
	f := newFixture(t)
	f.endpoint("https://example.invalid/reap")
	const deliveries = 120
	f.events(deliveries)

	cfg := testConfig("w-ghost")
	cfg.LeaseDuration, cfg.RequestTimeout = 300*time.Millisecond, 100*time.Millisecond
	ghost := f.worker(cfg)
	claimed := 0
	for claimed < deliveries {
		jobs, err := ghost.claim(context.Background(), 20)
		if err != nil || len(jobs) == 0 {
			t.Fatalf("claim: %v (%d jobs)", err, len(jobs))
		}
		claimed += len(jobs)
	}
	time.Sleep(450 * time.Millisecond) // every lease has expired; the ghost never reports

	const reapers = 8
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		total int
		mu    sync.Mutex
		errs  []error
	)
	for i := range reapers {
		wg.Add(1)
		c := cfg
		c.ID = fmt.Sprintf("reaper-%d", i)
		w := f.worker(c)
		go func() {
			defer wg.Done()
			<-start
			for {
				n, err := w.reapOnce(context.Background())
				mu.Lock()
				total += n
				if err != nil {
					errs = append(errs, err)
				}
				mu.Unlock()
				if n == 0 || err != nil {
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("a reaper failed: %v", errs[0])
	}
	if total != deliveries {
		t.Errorf("reapers reclaimed %d leases in total, want exactly %d", total, deliveries)
	}
	var wrong int
	if err := testPool.QueryRow(f.ctx, `SELECT count(*) FROM deliveries WHERE tenant_id = $1 AND (status <> 'pending' OR attempt_count <> 1)`, f.tenant).Scan(&wrong); err != nil {
		t.Fatal(err)
	}
	if wrong != 0 {
		t.Errorf("%d deliveries are not pending with exactly one (lost) attempt", wrong)
	}
	f.assertInvariant()
}
