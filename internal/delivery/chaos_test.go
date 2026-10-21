package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/hook-mini/internal/service"
	"github.com/burakcelik1061-star/hook-mini/pkg/webhook"
)

// TestChaos runs the whole system under abuse and then checks what must hold
// no matter what happened:
//
//   - several workers, restarted at random, some "crashing" (claiming work and
//     never reporting back);
//   - endpoints that fail, hang, are slow or rate-limit;
//   - clients ingesting events (some with repeated idempotency keys) while an
//     operator replays deliveries, disables and enables endpoints, rotates a
//     secret and deletes an endpoint.
//
// Afterwards: every delivery reached a final state, the attempt history is
// consistent, nothing was delivered to the wrong tenant or the wrong
// endpoint, every request carried a valid signature, every "succeeded"
// delivery really reached its endpoint, and no worker reported an internal
// error. The random seed is printed on failure.
func TestChaos(t *testing.T) {
	f := newFixture(t)
	seed := uint64(time.Now().UnixNano())
	t.Logf("chaos seed: %d", seed)
	rng := func(salt uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, salt)) }

	// A second tenant, to prove nothing crosses over.
	tenantB, err := f.svc.CreateTenant(f.ctx, "Chaos B "+uuid.NewString()[:6])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantB.ID) })

	// ---- receivers ----
	type hit struct {
		path, eventID string
		sigOK         bool
	}
	var (
		hitsMu sync.Mutex
		hits   []hit
		badSig atomic.Int64
	)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r2 := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 7))

		// Verify the signature with the secrets the endpoint has right now
		// (current and previous), exactly as a real receiver with both would.
		sigOK, known := false, false
		var secret string
		var previous *string
		if err := testPool.QueryRow(context.Background(),
			`SELECT secret, previous_secret FROM endpoints WHERE url = $1`, srv.URL+r.URL.Path).Scan(&secret, &previous); err == nil {
			known = true
			secrets := []string{secret}
			if previous != nil {
				secrets = append(secrets, *previous)
			}
			if v, err := webhook.NewVerifier(secrets...); err == nil {
				sigOK = v.Verify(r.Header, body) == nil
			}
		}
		if known && !sigOK {
			badSig.Add(1)
		}
		hitsMu.Lock()
		hits = append(hits, hit{path: r.URL.Path, eventID: r.Header.Get(webhook.HeaderID), sigOK: sigOK || !known})
		hitsMu.Unlock()

		switch strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/a-"), "/b-") {
		case "dead":
			w.WriteHeader(http.StatusInternalServerError)
		case "flaky":
			if r2.Float64() < 0.35 {
				w.WriteHeader(http.StatusInternalServerError)
			}
		case "slow":
			time.Sleep(time.Duration(r2.IntN(60)) * time.Millisecond)
		case "hang":
			if r2.Float64() < 0.12 {
				select {
				case <-r.Context().Done():
				case <-time.After(400 * time.Millisecond):
				}
			}
		case "limited":
			if r2.Float64() < 0.2 {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
			}
		}
	}))
	t.Cleanup(srv.Close)

	// ---- endpoints ----
	type ep struct {
		id     uuid.UUID
		path   string
		tenant uuid.UUID
	}
	var endpoints []ep
	add := func(tenant uuid.UUID, path string) ep {
		e, err := f.svc.CreateEndpoint(f.ctx, tenant, service.EndpointInput{URL: srv.URL + path})
		if err != nil {
			t.Fatal(err)
		}
		x := ep{id: e.ID, path: path, tenant: tenant}
		endpoints = append(endpoints, x)
		return x
	}
	for _, p := range []string{"/a-good", "/a-flaky", "/a-slow", "/a-hang", "/a-limited", "/a-dead"} {
		add(f.tenant, p)
	}
	for _, p := range []string{"/b-good", "/b-flaky"} {
		add(tenantB.ID, p)
	}
	byName := func(path string) ep {
		for _, e := range endpoints {
			if e.path == path {
				return e
			}
		}
		t.Fatalf("no endpoint %s", path)
		return ep{}
	}

	// ---- workers ----
	cfg := func(id string) Config {
		c := testConfig(id)
		c.Concurrency, c.BatchSize = 6, 4
		c.PollInterval = 5 * time.Millisecond
		c.LeaseDuration, c.RequestTimeout = 800*time.Millisecond, 150*time.Millisecond
		c.ReapInterval = 100 * time.Millisecond
		c.DrainTimeout = 200 * time.Millisecond
		c.Retry = RetryPolicy{MaxAttempts: 4, InitialBackoff: 5 * time.Millisecond, MaxBackoff: 40 * time.Millisecond}
		c.AutoDisableAfter = 30
		return c
	}
	type running struct {
		cancel context.CancelFunc
		done   chan error
	}
	var (
		wMu      sync.Mutex
		active   = map[string]running{}
		started  atomic.Int64
		restarts atomic.Int64
		runErrs  []error
	)
	startWorker := func() {
		id := fmt.Sprintf("chaos-w%d", started.Add(1))
		w := f.worker(cfg(id))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- w.Run(ctx) }()
		wMu.Lock()
		active[id] = running{cancel, done}
		wMu.Unlock()
	}
	stopWorker := func(id string, r running) {
		r.cancel()
		if err := <-r.done; err != nil {
			wMu.Lock()
			runErrs = append(runErrs, err)
			wMu.Unlock()
		}
		wMu.Lock()
		delete(active, id)
		wMu.Unlock()
	}
	for range 3 {
		startWorker()
	}

	// ---- actors ----
	stopActors := make(chan struct{})
	var actors sync.WaitGroup
	every := func(r *rand.Rand, min, max time.Duration, fn func()) {
		actors.Add(1)
		go func() {
			defer actors.Done()
			for {
				select {
				case <-stopActors:
					return
				case <-time.After(min + time.Duration(r.Int64N(int64(max-min)+1))):
					fn()
				}
			}
		}()
	}

	// Producers: ingest events, some with idempotency keys that repeat.
	var (
		ingestMu sync.Mutex
		created  = map[uuid.UUID]int{} // tenant -> events created (not replays)
		replayed atomic.Int64
	)
	for p := range 3 {
		r := rng(uint64(10 + p))
		every(r, time.Millisecond, 12*time.Millisecond, func() {
			tenant := f.tenant
			if r.IntN(4) == 0 {
				tenant = tenantB.ID
			}
			in := service.IngestInput{
				Type:    []string{"order.created", "order.paid", "user.signed-up"}[r.IntN(3)],
				Payload: json.RawMessage(fmt.Sprintf(`{"n":%d,"note":"çelik ✓"}`, r.IntN(1000))),
			}
			if r.IntN(4) == 0 {
				key := fmt.Sprintf("k-%d", r.IntN(15))
				in.IdempotencyKey = &key
				// A repeated key must carry the same content, or it is (rightly)
				// refused as a client error; so keyed events have fixed content.
				in.Type = "order.created"
				in.Payload = json.RawMessage(`{"fixed":true}`)
			}
			res, err := f.svc.IngestEvent(context.Background(), tenant, in)
			if err != nil {
				t.Errorf("IngestEvent: %v", err)
				return
			}
			if res.Replayed {
				replayed.Add(1)
				return
			}
			ingestMu.Lock()
			created[tenant]++
			ingestMu.Unlock()
		})
	}

	// Crasher: claims deliveries and never reports, like a killed process.
	var ghosts atomic.Int64
	{
		r := rng(20)
		c := cfg("ghost")
		c.LeaseDuration, c.RequestTimeout = 250*time.Millisecond, 100*time.Millisecond
		ghost := f.worker(c)
		every(r, 20*time.Millisecond, 120*time.Millisecond, func() {
			jobs, err := ghost.claim(context.Background(), 1+r.IntN(3))
			if err == nil {
				ghosts.Add(int64(len(jobs)))
			}
		})
	}

	// Restarter: gracefully stops a random worker and starts a new one.
	{
		r := rng(30)
		every(r, 150*time.Millisecond, 400*time.Millisecond, func() {
			wMu.Lock()
			var victim string
			var vr running
			for id, run := range active {
				victim, vr = id, run
				break
			}
			wMu.Unlock()
			if victim == "" {
				return
			}
			stopWorker(victim, vr)
			startWorker()
			restarts.Add(1)
		})
	}

	// Operator: replays, toggles endpoints, rotates a secret, deletes one.
	var (
		opReplays, opToggles atomic.Int64
		rotations            atomic.Int64
		deleted              atomic.Bool
	)
	{
		r := rng(40)
		every(r, 40*time.Millisecond, 160*time.Millisecond, func() {
			ctx := context.Background()
			switch r.IntN(6) {
			case 0, 1: // replay a random finished delivery
				var id, tenant uuid.UUID
				err := testPool.QueryRow(ctx, `SELECT id, tenant_id FROM deliveries WHERE status IN ('dead', 'succeeded') ORDER BY random() LIMIT 1`).Scan(&id, &tenant)
				if err != nil {
					return
				}
				_, err = f.svc.ReplayDelivery(ctx, tenant, id)
				var ce *service.ConflictError
				if err == nil {
					opReplays.Add(1)
				} else if !errors.As(err, &ce) && !errors.Is(err, service.ErrNotFound) {
					t.Errorf("ReplayDelivery: %v", err)
				}
			case 2, 3: // disable then enable the slow endpoint
				e := byName("/a-slow")
				if _, err := f.svc.DisableEndpoint(ctx, e.tenant, e.id); err != nil && !errors.Is(err, service.ErrNotFound) {
					t.Errorf("DisableEndpoint: %v", err)
				}
				time.Sleep(time.Duration(r.IntN(30)) * time.Millisecond)
				if _, err := f.svc.EnableEndpoint(ctx, e.tenant, e.id); err != nil && !errors.Is(err, service.ErrNotFound) {
					t.Errorf("EnableEndpoint: %v", err)
				}
				opToggles.Add(1)
			case 4: // rotate the good endpoint's secret (twice at most)
				if rotations.Load() < 2 {
					e := byName("/a-good")
					if _, err := f.svc.RotateEndpointSecret(ctx, e.tenant, e.id); err != nil {
						t.Errorf("RotateEndpointSecret: %v", err)
					}
					rotations.Add(1)
				}
			case 5: // delete the hanging endpoint once, with attempts probably in flight
				if deleted.CompareAndSwap(false, true) {
					e := byName("/a-hang")
					if err := f.svc.DeleteEndpoint(ctx, e.tenant, e.id); err != nil {
						t.Errorf("DeleteEndpoint: %v", err)
					}
				}
			}
		})
	}

	// ---- run, then settle ----
	time.Sleep(6 * time.Second)
	close(stopActors)
	actors.Wait()

	// Re-enable everything the operator or the failure limit disabled, so the
	// remaining work can finish rather than being abandoned; then let it drain.
	if _, err := testPool.Exec(f.ctx, `UPDATE endpoints SET status = 'active', disabled_at = NULL, disabled_reason = NULL WHERE status = 'disabled'`); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(45 * time.Second)
	quiet := 0
	for quiet < 3 {
		var open int
		if err := testPool.QueryRow(f.ctx, `SELECT count(*) FROM deliveries WHERE status IN ('pending', 'in_flight')`).Scan(&open); err != nil {
			t.Fatal(err)
		}
		if open == 0 {
			quiet++
		} else {
			quiet = 0
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d deliveries still open after 45s: the queue did not drain", open)
		}
		time.Sleep(100 * time.Millisecond)
	}

	wMu.Lock()
	remaining := make(map[string]running, len(active))
	for id, r := range active {
		remaining[id] = r
	}
	wMu.Unlock()
	for id, r := range remaining {
		stopWorker(id, r)
	}

	// ---- what the run looked like (a test that did nothing proves nothing) ----
	var total, succeeded, dead, lost, retried int
	if err := testPool.QueryRow(f.ctx, `SELECT count(*), count(*) FILTER (WHERE status = 'succeeded'), count(*) FILTER (WHERE status = 'dead') FROM deliveries`).Scan(&total, &succeeded, &dead); err != nil {
		t.Fatal(err)
	}
	_ = testPool.QueryRow(f.ctx, `SELECT count(*) FROM delivery_attempts WHERE error LIKE 'lease expired%'`).Scan(&lost)
	_ = testPool.QueryRow(f.ctx, `SELECT count(*) FROM deliveries WHERE attempt_count > 1 AND status = 'succeeded'`).Scan(&retried)
	t.Logf("chaos: %d deliveries (%d succeeded, %d dead), %d events replayed by idempotency, %d crashed claims, %d lost leases reclaimed, %d succeeded after retries, %d worker restarts, %d operator replays, %d toggles, %d rotations, endpoint deleted: %v, %d requests received",
		total, succeeded, dead, replayed.Load(), ghosts.Load(), lost, retried, restarts.Load(), opReplays.Load(), opToggles.Load(), rotations.Load(), deleted.Load(), len(hits))
	for name, n := range map[string]int64{
		"deliveries": int64(total), "succeeded": int64(succeeded), "dead": int64(dead), "crashed claims": ghosts.Load(),
		"lost leases": int64(lost), "retries": int64(retried), "restarts": restarts.Load(), "idempotent replays": replayed.Load(),
	} {
		if n == 0 {
			t.Errorf("the chaos run produced no %s: the test did not exercise what it claims to", name)
		}
	}

	// ---- invariants ----

	// 1. Everything reached a final state; nothing holds a lease.
	var open int
	_ = testPool.QueryRow(f.ctx, `SELECT count(*) FROM deliveries WHERE status IN ('pending', 'in_flight') OR locked_until IS NOT NULL OR lease_token IS NOT NULL`).Scan(&open)
	if open != 0 {
		t.Errorf("%d deliveries are not in a final state or still hold a lease", open)
	}

	// 2. The attempt history is consistent: attempt_count equals the rows,
	// numbered 1..n without gaps or repeats.
	rows, err := testPool.Query(f.ctx, `
		SELECT d.id::text, d.attempt_count, count(a.id), coalesce(min(a.attempt_no), 0), coalesce(max(a.attempt_no), 0), count(DISTINCT a.attempt_no)
		FROM deliveries d LEFT JOIN delivery_attempts a ON a.delivery_id = d.id
		GROUP BY d.id, d.attempt_count
		HAVING d.attempt_count <> count(a.id)
		    OR (count(a.id) > 0 AND (min(a.attempt_no) <> 1 OR max(a.attempt_no) <> count(a.id) OR count(DISTINCT a.attempt_no) <> count(a.id)))`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		var ac, n, lo, hi, distinct int
		_ = rows.Scan(&id, &ac, &n, &lo, &hi, &distinct)
		t.Errorf("delivery %s: attempt_count=%d rows=%d numbers %d..%d (%d distinct)", id, ac, n, lo, hi, distinct)
	}
	rows.Close()

	// 3. Final states are coherent.
	var incoherent int
	_ = testPool.QueryRow(f.ctx, `SELECT count(*) FROM deliveries WHERE
		(status = 'succeeded' AND (last_status_code NOT BETWEEN 200 AND 299 OR last_status_code IS NULL OR completed_at IS NULL))
		OR (status = 'dead' AND (last_error IS NULL OR completed_at IS NULL))`).Scan(&incoherent)
	if incoherent != 0 {
		t.Errorf("%d deliveries have a final state that does not match their last result", incoherent)
	}

	// 4. Events: idempotency produced exactly the events we created.
	for tenant, want := range created {
		var got int
		_ = testPool.QueryRow(f.ctx, `SELECT count(*) FROM events WHERE tenant_id = $1`, tenant).Scan(&got)
		if got != want {
			t.Errorf("tenant %s has %d events, but %d ingests created one (the rest were idempotent replays)", tenant, got, want)
		}
	}

	// 5. Isolation and routing: every request went to the endpoint its
	// delivery belongs to, carrying an event of that endpoint's own tenant.
	eventTenant := map[string]uuid.UUID{}
	erows, err := testPool.Query(f.ctx, `SELECT id::text, tenant_id FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	for erows.Next() {
		var id string
		var tn uuid.UUID
		_ = erows.Scan(&id, &tn)
		eventTenant[id] = tn
	}
	erows.Close()
	pathTenant := map[string]uuid.UUID{}
	for _, e := range endpoints {
		pathTenant[e.path] = e.tenant
	}
	received := map[string]bool{} // path+" "+event id
	for _, h := range hits {
		tn, known := eventTenant[h.eventID]
		if !known {
			t.Errorf("%s received a request for an unknown event %q", h.path, h.eventID)
			continue
		}
		if tn != pathTenant[h.path] {
			t.Errorf("%s (tenant %s) received an event of another tenant (%s)", h.path, pathTenant[h.path], tn)
		}
		received[h.path+" "+h.eventID] = true
	}

	// 6. At least once: every delivery recorded as succeeded reached its endpoint.
	pathByID := map[uuid.UUID]string{}
	for _, e := range endpoints {
		pathByID[e.id] = e.path
	}
	srows, err := testPool.Query(f.ctx, `SELECT endpoint_id, event_id::text FROM deliveries WHERE status = 'succeeded'`)
	if err != nil {
		t.Fatal(err)
	}
	for srows.Next() {
		var epID uuid.UUID
		var evID string
		_ = srows.Scan(&epID, &evID)
		if !received[pathByID[epID]+" "+evID] {
			t.Errorf("delivery of event %s to %s is marked succeeded, but the endpoint never received it", evID, pathByID[epID])
		}
	}
	srows.Close()

	// 7. Signatures: every request verified with the endpoint's secrets.
	if n := badSig.Load(); n != 0 {
		t.Errorf("%d requests carried an invalid signature", n)
	}

	// 8. No internal errors anywhere, and every worker stopped cleanly.
	for _, bad := range []string{"recording the attempt failed", "claim returned more deliveries", "deadlock", "reclaiming expired leases failed", "claiming deliveries failed", "abandoning the delivery failed"} {
		if strings.Contains(f.logs.String(), bad) {
			t.Errorf("a worker logged %q", bad)
		}
	}
	for _, err := range runErrs {
		t.Errorf("a worker's Run returned %v", err)
	}
}
