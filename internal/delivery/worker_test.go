package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/hook-mini/internal/database"
	"github.com/burakcelik1061-star/hook-mini/internal/service"
	"github.com/burakcelik1061-star/hook-mini/internal/store"
)

// Worker tests run real workers against a real Postgres and real HTTP
// servers. A worker claims every due delivery in its database, so these tests
// use a scratch database of their own (created next to TEST_DATABASE_URL and
// dropped afterwards); they could otherwise grab deliveries created by other
// packages' tests running at the same time.

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		os.Exit(m.Run()) // database tests skip themselves
	}
	os.Exit(runWithScratchDatabase(m, base))
}

func runWithScratchDatabase(m *testing.M, base string) int {
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		return 1
	}
	defer admin.Close(ctx)

	name := "hookmini_scratch_delivery_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		fmt.Fprintln(os.Stderr, "create scratch database:", err)
		return 1
	}
	defer func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") }()

	u, err := url.Parse(base)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	u.Path = "/" + name
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := database.Migrate(ctx, quiet, u.String()); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		return 1
	}
	pool, err := database.Connect(ctx, quiet, u.String())
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		return 1
	}
	defer pool.Close()
	testPool = pool
	return m.Run()
}

// ---- helpers ----

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func testConfig(id string) Config {
	return Config{
		ID:             id,
		Concurrency:    4,
		BatchSize:      4,
		PollInterval:   10 * time.Millisecond,
		LeaseDuration:  3 * time.Second,
		ReapInterval:   50 * time.Millisecond,
		RequestTimeout: time.Second,
		DrainTimeout:   3 * time.Second,
		Retry:          RetryPolicy{MaxAttempts: 3, InitialBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond},
	}
}

type fixture struct {
	t      *testing.T
	ctx    context.Context
	svc    *service.Service
	q      *store.Queries
	tenant uuid.UUID
	log    *slog.Logger
	logs   *logBuffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL not set")
	}
	logs := &logBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("worker logs:\n%s", logs.String())
		}
	})

	f := &fixture{t: t, ctx: context.Background(), svc: service.New(testPool, log), q: store.New(testPool), log: log, logs: logs}
	tn, err := f.svc.CreateTenant(f.ctx, "Delivery "+uuid.NewString()[:8])
	if err != nil {
		t.Fatal(err)
	}
	f.tenant = tn.ID
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1`, tn.ID) })
	return f
}

func (f *fixture) endpoint(url string) uuid.UUID {
	f.t.Helper()
	ep, err := f.svc.CreateEndpoint(f.ctx, f.tenant, service.EndpointInput{URL: url})
	if err != nil {
		f.t.Fatal(err)
	}
	return ep.ID
}

// events submits n events and returns their ids.
func (f *fixture) events(n int) []uuid.UUID {
	f.t.Helper()
	ids := make([]uuid.UUID, n)
	for i := range n {
		res, err := f.svc.IngestEvent(f.ctx, f.tenant, service.IngestInput{
			Type: "test.event", Payload: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)),
		})
		if err != nil {
			f.t.Fatal(err)
		}
		ids[i] = res.Event.ID
	}
	return ids
}

func (f *fixture) deliveries() []store.Delivery {
	f.t.Helper()
	rows, err := f.q.ListDeliveries(f.ctx, store.ListDeliveriesParams{TenantID: f.tenant, PageSize: 1000})
	if err != nil {
		f.t.Fatal(err)
	}
	return rows
}

func (f *fixture) only() store.Delivery {
	f.t.Helper()
	ds := f.deliveries()
	if len(ds) != 1 {
		f.t.Fatalf("expected exactly one delivery, found %d", len(ds))
	}
	return ds[0]
}

func (f *fixture) delivery(id uuid.UUID) store.Delivery {
	f.t.Helper()
	d, err := f.q.GetDelivery(f.ctx, store.GetDeliveryParams{ID: id, TenantID: f.tenant})
	if err != nil {
		f.t.Fatal(err)
	}
	return d
}

func (f *fixture) attempts(id uuid.UUID) []store.DeliveryAttempt {
	f.t.Helper()
	a, err := f.q.ListDeliveryAttempts(f.ctx, store.ListDeliveryAttemptsParams{DeliveryID: id, TenantID: f.tenant})
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

func (f *fixture) worker(cfg Config) *Worker {
	f.t.Helper()
	w, err := NewWorker(testPool, f.log, cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	return w
}

// start runs a worker until the returned stop function is called (also
// called on cleanup). stop waits for Run to return.
func (f *fixture) start(cfg Config) (stop func()) {
	f.t.Helper()
	w := f.worker(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					f.t.Errorf("Run returned %v", err)
				}
			case <-time.After(10 * time.Second):
				f.t.Error("worker did not stop within 10s")
			}
		})
	}
	f.t.Cleanup(stop)
	return stop
}

// waitFor polls cond until it holds or the timeout passes.
func (f *fixture) waitFor(timeout time.Duration, what string, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			var states []string
			for _, d := range f.deliveries() {
				states = append(states, fmt.Sprintf("%s:%s/%d", d.ID.String()[:8], d.Status, d.AttemptCount))
			}
			f.t.Fatalf("timed out after %s waiting for %s; deliveries: %v", timeout, what, states)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *fixture) waitStatus(timeout time.Duration, status string, n int) {
	f.t.Helper()
	f.waitFor(timeout, fmt.Sprintf("%d deliveries %s", n, status), func() bool {
		c := 0
		for _, d := range f.deliveries() {
			if d.Status == status {
				c++
			}
		}
		return c == n
	})
}

// assertInvariant checks what every code path must preserve: each delivery
// has exactly attempt_count attempt rows, numbered 1..attempt_count, and no
// lease survives outside the in_flight state (the schema enforces the latter).
func (f *fixture) assertInvariant() {
	f.t.Helper()
	var bad []string
	rows, err := testPool.Query(f.ctx, `
		SELECT d.id::text, d.attempt_count, count(a.id), coalesce(max(a.attempt_no), 0)
		FROM deliveries d LEFT JOIN delivery_attempts a ON a.delivery_id = d.id
		WHERE d.tenant_id = $1
		GROUP BY d.id, d.attempt_count
		HAVING d.attempt_count <> count(a.id) OR d.attempt_count <> coalesce(max(a.attempt_no), 0)`, f.tenant)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var count, rowsN, maxNo int64
		if err := rows.Scan(&id, &count, &rowsN, &maxNo); err != nil {
			f.t.Fatal(err)
		}
		bad = append(bad, fmt.Sprintf("%s: attempt_count=%d rows=%d max_no=%d", id, count, rowsN, maxNo))
	}
	if len(bad) > 0 {
		f.t.Errorf("attempt_count does not match the attempt history: %v", bad)
	}
}

type hit struct {
	path      string
	webhookID string
	header    http.Header
	body      []byte
}

// recorder is a webhook endpoint that remembers every request. respond, if
// set, writes the response for the n-th request (1-based).
type recorder struct {
	mu      sync.Mutex
	hits    []hit
	respond func(w http.ResponseWriter, r *http.Request, n int)
}

func newRecorder(t *testing.T, respond func(w http.ResponseWriter, r *http.Request, n int)) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{respond: respond}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.hits = append(rec.hits, hit{path: r.URL.Path, webhookID: r.Header.Get("Webhook-Id"), header: r.Header.Clone(), body: body})
		n := len(rec.hits)
		rec.mu.Unlock()
		if rec.respond != nil {
			rec.respond(w, r, n)
			return
		}
		io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.hits)
}

func (r *recorder) all() []hit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]hit(nil), r.hits...)
}

// ---- tests ----

func TestDeliversAndRecordsSuccess(t *testing.T) {
	f := newFixture(t)
	srv, rec := newRecorder(t, nil)
	ep := f.endpoint(srv.URL + "/hooks")
	evID := f.events(1)[0]

	f.start(testConfig("w-success"))
	f.waitStatus(5*time.Second, "succeeded", 1)

	hits := rec.all()
	if len(hits) != 1 {
		t.Fatalf("endpoint received %d requests, want 1", len(hits))
	}
	h := hits[0]
	if h.path != "/hooks" || h.webhookID != evID.String() || h.header.Get("Content-Type") != "application/json" {
		t.Errorf("unexpected request: path=%s webhook-id=%s content-type=%s", h.path, h.webhookID, h.header.Get("Content-Type"))
	}
	var env struct {
		ID   uuid.UUID       `json:"id"`
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	var data map[string]any
	if err := json.Unmarshal(h.body, &env); err != nil || env.ID != evID || env.Type != "test.event" ||
		json.Unmarshal(env.Data, &data) != nil || data["n"] != float64(0) || len(data) != 1 {
		t.Errorf("unexpected body %s (err=%v)", h.body, err)
	}

	d := f.only()
	if d.AttemptCount != 1 || d.LastStatusCode == nil || *d.LastStatusCode != 200 || d.CompletedAt == nil ||
		d.LockedUntil != nil || d.LockedBy != nil || d.LastError != nil {
		t.Errorf("unexpected delivery row: %+v", d)
	}
	a := f.attempts(d.ID)
	if len(a) != 1 || a[0].AttemptNo != 1 || a[0].StatusCode == nil || *a[0].StatusCode != 200 ||
		a[0].ResponseSnippet == nil || *a[0].ResponseSnippet != "ok" || a[0].Error != nil {
		t.Errorf("unexpected attempt rows: %+v", a)
	}
	got, _ := f.svc.GetEndpoint(f.ctx, f.tenant, ep)
	if got.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures = %d after a success", got.ConsecutiveFailures)
	}
	f.assertInvariant()
}

func TestRetriesUntilSuccess(t *testing.T) {
	f := newFixture(t)
	srv, rec := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, n int) {
		if n <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, "boom")
			return
		}
		io.WriteString(w, "finally")
	})
	ep := f.endpoint(srv.URL)
	f.events(1)

	cfg := testConfig("w-retry")
	cfg.Retry.MaxAttempts = 5
	f.start(cfg)
	f.waitStatus(5*time.Second, "succeeded", 1)

	d := f.only()
	if rec.count() != 3 || d.AttemptCount != 3 {
		t.Errorf("requests=%d attempt_count=%d, want 3 and 3", rec.count(), d.AttemptCount)
	}
	var codes []int32
	for _, a := range f.attempts(d.ID) {
		codes = append(codes, *a.StatusCode)
	}
	if fmt.Sprint(codes) != "[500 500 200]" {
		t.Errorf("attempt status codes = %v, want [500 500 200]", codes)
	}
	// Two failures, then a success: the endpoint is healthy again.
	got, _ := f.svc.GetEndpoint(f.ctx, f.tenant, ep)
	if got.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures = %d, want 0 after the success", got.ConsecutiveFailures)
	}
	f.assertInvariant()
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	f := newFixture(t)
	srv, rec := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	ep := f.endpoint(srv.URL)
	f.events(1)

	f.start(testConfig("w-dead")) // MaxAttempts 3
	f.waitStatus(5*time.Second, "dead", 1)

	d := f.only()
	if rec.count() != 3 || d.AttemptCount != 3 || d.CompletedAt == nil {
		t.Errorf("requests=%d attempt_count=%d completed_at=%v", rec.count(), d.AttemptCount, d.CompletedAt)
	}
	if d.LastError == nil || !strings.Contains(*d.LastError, "503") {
		t.Errorf("last_error = %v, want it to mention 503", d.LastError)
	}
	got, _ := f.svc.GetEndpoint(f.ctx, f.tenant, ep)
	if got.ConsecutiveFailures != 3 {
		t.Errorf("consecutive_failures = %d, want 3", got.ConsecutiveFailures)
	}
	// Nothing more happens to a dead delivery.
	time.Sleep(200 * time.Millisecond)
	if rec.count() != 3 {
		t.Errorf("a dead delivery was retried: %d requests", rec.count())
	}
	f.assertInvariant()
}

func TestHonoursRetryAfter(t *testing.T) {
	f := newFixture(t)
	srv, _ := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	f.endpoint(srv.URL)
	f.events(1)

	cfg := testConfig("w-retry-after")
	cfg.Retry.MaxBackoff = time.Hour // the 10ms backoff alone would retry almost at once
	stop := f.start(cfg)
	f.waitFor(5*time.Second, "the first attempt", func() bool { return f.only().AttemptCount == 1 })
	stop()

	var wait float64
	if err := testPool.QueryRow(f.ctx, `SELECT extract(epoch FROM next_attempt_at - now()) FROM deliveries WHERE id = $1`, f.only().ID).Scan(&wait); err != nil {
		t.Fatal(err)
	}
	if wait < 110 || wait > 121 {
		t.Errorf("next attempt in %.1fs, want about 120s as the endpoint asked", wait)
	}
	if d := f.only(); d.Status != "pending" || d.LastStatusCode == nil || *d.LastStatusCode != 429 {
		t.Errorf("unexpected delivery: %+v", d)
	}
	f.assertInvariant()
}

func TestTimeoutIsAFailedAttempt(t *testing.T) {
	f := newFixture(t)
	srv, _ := newRecorder(t, func(_ http.ResponseWriter, r *http.Request, _ int) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	f.endpoint(srv.URL)
	f.events(1)

	cfg := testConfig("w-timeout")
	cfg.RequestTimeout = 300 * time.Millisecond
	cfg.Retry.MaxAttempts = 1
	f.start(cfg)
	f.waitStatus(5*time.Second, "dead", 1)

	a := f.attempts(f.only().ID)
	if len(a) != 1 || a[0].StatusCode != nil || a[0].Error == nil || !strings.Contains(*a[0].Error, "timeout") || a[0].DurationMs < 300 {
		t.Errorf("unexpected attempt: %+v", a)
	}
	f.assertInvariant()
}

func TestHostileResponseBodyIsStoredSafely(t *testing.T) {
	f := newFixture(t)
	srv, _ := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "\x00\xff\xfe"+strings.Repeat("é", 5000))
	})
	f.endpoint(srv.URL)
	f.events(1)

	cfg := testConfig("w-body")
	cfg.Retry.MaxAttempts = 1
	f.start(cfg)
	// Without cleaning, Postgres would reject the NUL byte and invalid UTF-8,
	// the attempt could never be recorded and the delivery would loop.
	f.waitStatus(5*time.Second, "dead", 1)

	a := f.attempts(f.only().ID)
	if len(a) != 1 || a[0].ResponseSnippet == nil {
		t.Fatalf("attempt not recorded: %+v", a)
	}
	s := *a[0].ResponseSnippet
	if !utf8.ValidString(s) || strings.ContainsRune(s, 0) || utf8.RuneCountInString(s) > maxSnippetRunes {
		t.Errorf("snippet not cleaned: %d runes, valid=%v", utf8.RuneCountInString(s), utf8.ValidString(s))
	}
	f.assertInvariant()
}

func TestDisabledEndpointIsAbandonedWithoutARequest(t *testing.T) {
	f := newFixture(t)
	srv, rec := newRecorder(t, nil)
	ep := f.endpoint(srv.URL)
	f.events(1)
	if _, err := f.q.DisableEndpoint(f.ctx, store.DisableEndpointParams{ID: ep, DisabledReason: ptr("test")}); err != nil {
		t.Fatal(err)
	}

	f.start(testConfig("w-disabled"))
	f.waitStatus(5*time.Second, "dead", 1)

	d := f.only()
	if rec.count() != 0 || d.AttemptCount != 0 || len(f.attempts(d.ID)) != 0 {
		t.Errorf("requests=%d attempt_count=%d: a disabled endpoint must not be contacted", rec.count(), d.AttemptCount)
	}
	if d.LastError == nil || !strings.Contains(*d.LastError, "disabled") {
		t.Errorf("last_error = %v", d.LastError)
	}
	f.assertInvariant()
}

// The core concurrency property: with several workers competing, every
// delivery reaches its endpoint exactly once.
func TestManyWorkersDeliverEachMessageExactlyOnce(t *testing.T) {
	f := newFixture(t)
	srv, rec := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		time.Sleep(time.Duration(rand.IntN(3)) * time.Millisecond) // interleave workers
		io.WriteString(w, "ok")
	})
	for _, p := range []string{"/a", "/b", "/c"} {
		f.endpoint(srv.URL + p)
	}
	const events = 40
	f.events(events)
	const total = events * 3

	for i := range 4 {
		cfg := testConfig(fmt.Sprintf("w-many-%d", i))
		cfg.Concurrency, cfg.BatchSize = 6, 3
		f.start(cfg)
	}
	f.waitStatus(30*time.Second, "succeeded", total)

	seen := map[string]int{}
	for _, h := range rec.all() {
		seen[h.path+" "+h.webhookID]++
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("%s delivered %d times", k, n)
		}
	}
	if len(seen) != total || rec.count() != total {
		t.Errorf("%d distinct deliveries in %d requests, want %d in %d", len(seen), rec.count(), total, total)
	}
	f.assertInvariant()
}

func TestClaimSkipsRowsLockedByOthers(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{"/a", "/b", "/c"} {
		f.endpoint("https://example.invalid" + p)
	}
	f.events(1)
	ds := f.deliveries()
	if len(ds) != 3 {
		t.Fatalf("want 3 deliveries, got %d", len(ds))
	}

	// Another transaction (think: another worker mid-claim) holds one row.
	tx, err := testPool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	locked := ds[0].ID
	if _, err := tx.Exec(f.ctx, `SELECT 1 FROM deliveries WHERE id = $1 FOR UPDATE`, locked); err != nil {
		t.Fatal(err)
	}

	w := f.worker(testConfig("w-skip"))
	start := time.Now()
	jobs, err := w.claim(f.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("claim waited %s for a locked row instead of skipping it", elapsed)
	}
	if len(jobs) != 2 {
		t.Fatalf("claimed %d deliveries, want the 2 unlocked ones", len(jobs))
	}
	for _, j := range jobs {
		if j.ID == locked {
			t.Error("claimed a row another transaction holds")
		}
	}
}

func TestClaimTakesOnlyDueDeliveriesOldestFirst(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{"/a", "/b", "/c"} {
		f.endpoint("https://example.invalid" + p)
	}
	f.events(1)
	ds := f.deliveries()
	older, old, future := ds[0].ID, ds[1].ID, ds[2].ID
	for id, offset := range map[uuid.UUID]string{older: "-2 minutes", old: "-1 minute", future: "1 hour"} {
		if _, err := testPool.Exec(f.ctx, `UPDATE deliveries SET next_attempt_at = now() + $2::interval WHERE id = $1`, id, offset); err != nil {
			t.Fatal(err)
		}
	}

	w := f.worker(testConfig("w-due"))
	first, err := w.claim(f.ctx, 1)
	if err != nil || len(first) != 1 || first[0].ID != older {
		t.Fatalf("first claim = %v (err=%v), want the oldest due delivery", first, err)
	}
	rest, err := w.claim(f.ctx, 10)
	if err != nil || len(rest) != 1 || rest[0].ID != old {
		t.Fatalf("second claim = %v (err=%v), want only the other due delivery, not the future one", rest, err)
	}

	// The lease is complete and the attempt counter untouched.
	for _, j := range append(first, rest...) {
		d := f.delivery(j.ID)
		if d.Status != "in_flight" || d.LockedBy == nil || *d.LockedBy != "w-due" || d.LockedUntil == nil || d.AttemptCount != 0 {
			t.Errorf("unexpected claimed row: %+v", d)
		}
		if until := time.Until(*d.LockedUntil); until < 2*time.Second || until > 4*time.Second {
			t.Errorf("lease ends in %s, want about the 3s lease", until)
		}
	}
	if first[0].LeaseToken == rest[0].LeaseToken {
		t.Error("two claims got the same lease token")
	}
	if f.delivery(future).Status != "pending" {
		t.Error("a delivery that is not due yet was claimed")
	}
}

// A worker that dies mid-attempt must not lose the delivery.
func TestCrashedWorkersLeaseIsReclaimed(t *testing.T) {
	f := newFixture(t)
	srv, rec := newRecorder(t, nil)
	f.endpoint(srv.URL)
	f.events(1)

	// The "crashed" worker claims with a short lease and never reports back.
	crashed := testConfig("w-crashed")
	crashed.LeaseDuration, crashed.RequestTimeout = 300*time.Millisecond, 100*time.Millisecond
	if jobs, err := f.worker(crashed).claim(f.ctx, 1); err != nil || len(jobs) != 1 {
		t.Fatalf("claim: %v (%d jobs)", err, len(jobs))
	}

	f.start(testConfig("w-survivor"))
	f.waitStatus(5*time.Second, "succeeded", 1)

	d := f.only()
	a := f.attempts(d.ID)
	if d.AttemptCount != 2 || len(a) != 2 || rec.count() != 1 {
		t.Fatalf("attempt_count=%d attempts=%d requests=%d, want 2, 2, 1", d.AttemptCount, len(a), rec.count())
	}
	if a[0].Error == nil || !strings.Contains(*a[0].Error, "lease expired") || a[0].StatusCode != nil {
		t.Errorf("first attempt should record the lost lease: %+v", a[0])
	}
	if a[1].StatusCode == nil || *a[1].StatusCode != 200 {
		t.Errorf("second attempt should be the successful delivery: %+v", a[1])
	}
	if !strings.Contains(f.logs.String(), "previous_worker=w-crashed") {
		t.Error("the reclaim was not logged with the previous worker")
	}
	f.assertInvariant()
}

// Fencing: a worker whose lease expired must not overwrite the result of the
// worker that took the delivery over.
func TestLateResultFromALostLeaseIsDiscarded(t *testing.T) {
	f := newFixture(t)
	f.endpoint("https://example.invalid/hook")
	f.events(1)

	slowCfg := testConfig("w-slow")
	slowCfg.LeaseDuration, slowCfg.RequestTimeout = 300*time.Millisecond, 100*time.Millisecond
	slow := f.worker(slowCfg)
	fast := f.worker(testConfig("w-fast"))

	jobs, err := slow.claim(f.ctx, 1)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim: %v", err)
	}
	stale := jobs[0]

	time.Sleep(400 * time.Millisecond) // the slow worker's lease expires
	if n, err := fast.reapOnce(f.ctx); err != nil || n != 1 {
		t.Fatalf("reap: n=%d err=%v", n, err)
	}
	jobs, err = fast.claim(f.ctx, 1)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("re-claim: %v", err)
	}
	current := jobs[0]
	if current.LeaseToken == stale.LeaseToken || current.AttemptCount != 1 {
		t.Fatalf("re-claim did not get a fresh lease: %+v", current)
	}

	// The slow worker finally reports a success. It must be ignored.
	slow.record(f.log, stale, result{started: time.Now(), statusCode: 200, snippet: "late"})
	d := f.delivery(stale.ID)
	if d.Status != "in_flight" || d.LockedBy == nil || *d.LockedBy != "w-fast" {
		t.Fatalf("the stale result changed the delivery: %+v", d)
	}
	if !strings.Contains(f.logs.String(), "result discarded") {
		t.Error("the discarded result was not logged")
	}

	fast.record(f.log, current, result{started: time.Now(), statusCode: 500, errText: "endpoint responded with HTTP 500"})
	d = f.delivery(stale.ID)
	a := f.attempts(stale.ID)
	if d.Status != "pending" || d.AttemptCount != 2 || len(a) != 2 || a[1].StatusCode == nil || *a[1].StatusCode != 500 {
		t.Errorf("the current worker's result was not recorded: %+v attempts=%+v", d, a)
	}
	for _, at := range a {
		if at.ResponseSnippet != nil && *at.ResponseSnippet == "late" {
			t.Error("the stale attempt was written to the history")
		}
	}
	f.assertInvariant()
}

func TestReclaimGivesUpAtMaxAttempts(t *testing.T) {
	f := newFixture(t)
	f.endpoint("https://example.invalid/hook")
	f.events(1)
	d := f.only()

	// Two attempts already happened (with their history rows).
	for n := int32(1); n <= 2; n++ {
		if _, err := f.q.CreateDeliveryAttempt(f.ctx, store.CreateDeliveryAttemptParams{
			DeliveryID: d.ID, AttemptNo: n, StartedAt: time.Now(), DurationMs: 1, StatusCode: ptr(int32(500)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := testPool.Exec(f.ctx, `UPDATE deliveries SET attempt_count = 2 WHERE id = $1`, d.ID); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig("w-last")
	cfg.LeaseDuration, cfg.RequestTimeout = 200*time.Millisecond, 100*time.Millisecond
	w := f.worker(cfg) // MaxAttempts 3
	if jobs, err := w.claim(f.ctx, 1); err != nil || len(jobs) != 1 {
		t.Fatal("claim failed")
	}
	time.Sleep(300 * time.Millisecond)
	if n, err := w.reapOnce(f.ctx); err != nil || n != 1 {
		t.Fatalf("reap: n=%d err=%v", n, err)
	}

	d = f.delivery(d.ID)
	if d.Status != "dead" || d.AttemptCount != 3 || d.CompletedAt == nil {
		t.Errorf("the third (lost) attempt should exhaust the delivery: %+v", d)
	}
	f.assertInvariant()
}

// releaseOnCleanup unblocks a test handler when the test ends, even if it
// fails early; otherwise closing the test server would wait for the handler
// forever. Call it after newRecorder so it runs before the server closes.
func releaseOnCleanup(t *testing.T, release chan struct{}) func() {
	var once sync.Once
	closeIt := func() { once.Do(func() { close(release) }) }
	t.Cleanup(closeIt)
	return closeIt
}

func TestShutdownLetsTheAttemptInFlightFinish(t *testing.T) {
	f := newFixture(t)
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	srv, _ := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		arrived <- struct{}{}
		<-release
		io.WriteString(w, "ok")
	})
	unblock := releaseOnCleanup(t, release)
	f.endpoint(srv.URL)
	f.events(1)

	stop := f.start(testConfig("w-drain"))
	<-arrived

	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("the worker stopped without waiting for the attempt in flight")
	case <-time.After(300 * time.Millisecond):
	}

	unblock()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not stop after the attempt finished")
	}
	if d := f.only(); d.Status != "succeeded" || d.AttemptCount != 1 {
		t.Errorf("the attempt that finished during shutdown was not recorded: %+v", d)
	}
	f.assertInvariant()
}

func TestShutdownCancelsAttemptsAfterTheDrainTimeout(t *testing.T) {
	f := newFixture(t)
	arrived := make(chan struct{}, 1)
	srv, _ := newRecorder(t, func(_ http.ResponseWriter, r *http.Request, _ int) {
		arrived <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
	f.endpoint(srv.URL)
	f.events(1)

	cfg := testConfig("w-abort")
	cfg.RequestTimeout = 5 * time.Second
	cfg.LeaseDuration = 10 * time.Second
	cfg.DrainTimeout = 200 * time.Millisecond
	stop := f.start(cfg)
	<-arrived

	start := time.Now()
	stop()
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("stopping took %s despite a 200ms drain timeout", elapsed)
	}
	// The cancelled attempt is recorded as a failure and will be retried.
	d := f.only()
	a := f.attempts(d.ID)
	if d.Status != "pending" || len(a) != 1 || a[0].Error == nil || !strings.Contains(*a[0].Error, "canceled") {
		t.Errorf("unexpected state after an aborted attempt: %+v attempts=%+v", d, a)
	}
	f.assertInvariant()
}

func TestEndpointDeletedWhileAttemptIsInFlight(t *testing.T) {
	f := newFixture(t)
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	slowSrv, _ := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		arrived <- struct{}{}
		<-release
	})
	unblock := releaseOnCleanup(t, release)
	doomed := f.endpoint(slowSrv.URL)
	f.events(1)

	f.start(testConfig("w-deleted"))
	<-arrived
	if err := f.svc.DeleteEndpoint(f.ctx, f.tenant, doomed); err != nil {
		t.Fatalf("deleting an endpoint with an attempt in flight: %v", err)
	}
	unblock()

	// The worker shrugs off the vanished delivery and keeps working.
	okSrv, rec := newRecorder(t, nil)
	f.endpoint(okSrv.URL)
	f.events(1)
	f.waitFor(5*time.Second, "the next delivery", func() bool { return rec.count() == 1 })
	f.waitStatus(5*time.Second, "succeeded", 1)
	if !strings.Contains(f.logs.String(), "result discarded") {
		t.Error("the result for the deleted delivery was not reported as discarded")
	}
	f.assertInvariant()
}

func TestNewWorkerRejectsBadConfig(t *testing.T) {
	ok := testConfig("x")
	for name, mutate := range map[string]func(*Config){
		"no concurrency":         func(c *Config) { c.Concurrency = 0 },
		"no batch":               func(c *Config) { c.BatchSize = 0 },
		"no poll interval":       func(c *Config) { c.PollInterval = 0 },
		"no timeout":             func(c *Config) { c.RequestTimeout = 0 },
		"lease shorter than req": func(c *Config) { c.LeaseDuration = c.RequestTimeout },
		"no drain":               func(c *Config) { c.DrainTimeout = 0 },
		"bad retry policy":       func(c *Config) { c.Retry.MaxAttempts = 0 },
	} {
		c := ok
		mutate(&c)
		if _, err := NewWorker(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	w, err := NewWorker(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{
		Concurrency: 2, BatchSize: 50, PollInterval: time.Second, LeaseDuration: time.Minute,
		RequestTimeout: time.Second, DrainTimeout: time.Second, Retry: DefaultRetryPolicy(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if w.cfg.BatchSize != 2 || w.cfg.ReapInterval != 30*time.Second || w.ID() == "" {
		t.Errorf("defaults not applied: batch=%d reap=%s id=%q", w.cfg.BatchSize, w.cfg.ReapInterval, w.ID())
	}
}

func ptr[T any](v T) *T { return &v }
