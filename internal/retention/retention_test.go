package retention

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/hook-mini/internal/database"
	"github.com/burakcelik1061-star/hook-mini/internal/store"
)

// These tests run against a real Postgres: point TEST_DATABASE_URL at a
// throwaway database. Without it they skip. Every test builds its own tenant
// and removes it afterwards (which cascades to everything it created); rows
// are made "old" by writing old timestamps directly.

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		os.Exit(m.Run())
	}
	ctx := context.Background()
	log := quiet()
	if _, err := database.Migrate(ctx, log, url); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
	pool, err := database.Connect(ctx, log, url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	testPool = pool
	code := m.Run()
	pool.Close()
	os.Exit(code)
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fixture is one tenant with two endpoints.
type fixture struct {
	t         *testing.T
	ctx       context.Context
	tenant    uuid.UUID
	endpoints [2]uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL not set")
	}
	f := &fixture{t: t, ctx: context.Background()}
	must(t, testPool.QueryRow(f.ctx, `INSERT INTO tenants (name) VALUES ($1) RETURNING id`, "Retention "+uuid.NewString()[:8]).Scan(&f.tenant))
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1`, f.tenant) })
	for i := range f.endpoints {
		must(t, testPool.QueryRow(f.ctx, `INSERT INTO endpoints (tenant_id, url, secret) VALUES ($1, $2, $3) RETURNING id`,
			f.tenant, "https://example.invalid/"+uuid.NewString()[:8], "whsec_0123456789abcdef01234567").Scan(&f.endpoints[i]))
	}
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// event inserts an event created ageHours ago, optionally with an idempotency key.
func (f *fixture) event(ageHours int, key *string) uuid.UUID {
	f.t.Helper()
	var id uuid.UUID
	must(f.t, testPool.QueryRow(f.ctx, `INSERT INTO events (tenant_id, type, payload, idempotency_key, created_at)
		VALUES ($1, 'a.b', '{}', $2, now() - make_interval(hours => $3)) RETURNING id`, f.tenant, key, ageHours).Scan(&id))
	return id
}

// delivery inserts a delivery of event to endpoint n in the given status. For
// succeeded and dead, completedAgoHours says how long ago it ended; it gets
// one attempt row. In-flight deliveries get a valid lease.
func (f *fixture) delivery(event uuid.UUID, endpoint int, status string, completedAgoHours int) uuid.UUID {
	f.t.Helper()
	var id uuid.UUID
	switch status {
	case "succeeded", "dead":
		must(f.t, testPool.QueryRow(f.ctx, `INSERT INTO deliveries (tenant_id, event_id, endpoint_id, status, attempt_count, completed_at)
			VALUES ($1, $2, $3, $4, 1, now() - make_interval(hours => $5)) RETURNING id`,
			f.tenant, event, f.endpoints[endpoint], status, completedAgoHours).Scan(&id))
		_, err := testPool.Exec(f.ctx, `INSERT INTO delivery_attempts (delivery_id, attempt_no, started_at, duration_ms) VALUES ($1, 1, now(), 5)`, id)
		must(f.t, err)
	case "in_flight":
		must(f.t, testPool.QueryRow(f.ctx, `INSERT INTO deliveries (tenant_id, event_id, endpoint_id, status, locked_until, locked_by, lease_token)
			VALUES ($1, $2, $3, 'in_flight', now() + interval '1 hour', 'w', gen_random_uuid()) RETURNING id`,
			f.tenant, event, f.endpoints[endpoint]).Scan(&id))
	default:
		must(f.t, testPool.QueryRow(f.ctx, `INSERT INTO deliveries (tenant_id, event_id, endpoint_id, status)
			VALUES ($1, $2, $3, $4) RETURNING id`, f.tenant, event, f.endpoints[endpoint], status).Scan(&id))
	}
	return id
}

func (f *fixture) deliveryExists(id uuid.UUID) bool {
	f.t.Helper()
	var n int
	must(f.t, testPool.QueryRow(f.ctx, `SELECT count(*) FROM deliveries WHERE id = $1`, id).Scan(&n))
	return n == 1
}

func (f *fixture) eventExists(id uuid.UUID) bool {
	f.t.Helper()
	var n int
	must(f.t, testPool.QueryRow(f.ctx, `SELECT count(*) FROM events WHERE id = $1`, id).Scan(&n))
	return n == 1
}

func (f *fixture) attempts(delivery uuid.UUID) int {
	f.t.Helper()
	var n int
	must(f.t, testPool.QueryRow(f.ctx, `SELECT count(*) FROM delivery_attempts WHERE delivery_id = $1`, delivery).Scan(&n))
	return n
}

func (f *fixture) cleaner(period time.Duration, batch int) *Cleaner {
	f.t.Helper()
	c, err := New(testPool, quiet(), Config{Period: period, Interval: time.Hour, BatchSize: batch})
	must(f.t, err)
	return c
}

const day = 24 // hours

func TestOnlyExpiredFinishedDeliveriesAreDeleted(t *testing.T) {
	f := newFixture(t)
	period := 72 * time.Hour

	type row struct {
		name     string
		status   string
		ageHours int // of the event; the delivery ended at the same time
		wantGone bool
	}
	rows := []row{
		{"old succeeded", "succeeded", 100, true},
		{"old dead", "dead", 100, true},
		{"just inside the period", "dead", 71, false},
		{"just outside the period", "succeeded", 73, true},
		{"recent succeeded", "succeeded", 1, false},
		{"old but pending", "pending", 200, false},
		{"old but in flight", "in_flight", 200, false},
	}
	type made struct {
		row
		event, delivery uuid.UUID
	}
	var all []made
	for _, r := range rows {
		ev := f.event(r.ageHours, nil)
		all = append(all, made{r, ev, f.delivery(ev, 0, r.status, r.ageHours)})
	}
	emptyOld := f.event(200, nil) // never had a delivery
	emptyYoung := f.event(1, nil) // never had a delivery, but is recent

	res, err := f.cleaner(period, 0).Once(f.ctx)
	must(t, err)
	if res.Deliveries < 3 || res.Events < 4 {
		t.Errorf("result %+v: expected at least the 3 expired deliveries and the 4 expired events to be counted", res)
	}

	for _, m := range all {
		if got := !f.deliveryExists(m.delivery); got != m.wantGone {
			t.Errorf("%s: delivery gone=%v, want %v", m.name, got, m.wantGone)
		}
		if m.wantGone && f.attempts(m.delivery) != 0 {
			t.Errorf("%s: its attempts were not deleted with it", m.name)
		}
		if !m.wantGone && m.status != "pending" && m.status != "in_flight" && f.attempts(m.delivery) != 1 {
			t.Errorf("%s: a kept delivery lost its attempt history", m.name)
		}
		// An event goes only when it is old enough and has no delivery left.
		eventGone := !f.eventExists(m.event)
		wantEventGone := m.wantGone // all expired deliveries here belong to events at least as old
		if eventGone != wantEventGone {
			t.Errorf("%s: event gone=%v, want %v", m.name, eventGone, wantEventGone)
		}
	}
	if f.eventExists(emptyOld) {
		t.Error("an old event without deliveries was kept")
	}
	if !f.eventExists(emptyYoung) {
		t.Error("a recent event without deliveries was deleted")
	}
}

// An event with one expired and one live delivery must stay, and so must the
// live delivery; the expired one goes.
func TestAnEventStaysWhileAnyDeliveryRemains(t *testing.T) {
	f := newFixture(t)
	for _, live := range []string{"pending", "in_flight", "succeeded"} {
		ev := f.event(300, nil)
		gone := f.delivery(ev, 0, "dead", 300)
		kept := f.delivery(ev, 1, live, 1) // "succeeded" ended an hour ago: not expired
		_, err := f.cleaner(72*time.Hour, 0).Once(f.ctx)
		must(t, err)
		if f.deliveryExists(gone) {
			t.Errorf("%s: the expired delivery was kept", live)
		}
		if !f.deliveryExists(kept) || !f.eventExists(ev) {
			t.Errorf("%s: the live delivery or its event was deleted", live)
		}
	}
}

// Deleting an event deletes its idempotency key: the key is free again once
// retention has passed, and not before.
func TestIdempotencyKeyIsFreeAfterRetention(t *testing.T) {
	f := newFixture(t)
	key := "order-1001"
	old := f.event(300, &key)
	f.delivery(old, 0, "succeeded", 300)
	other := "order-1002"
	recent := f.event(1, &other)
	f.delivery(recent, 0, "succeeded", 1)

	reuse := func(k string) error {
		_, err := testPool.Exec(f.ctx, `INSERT INTO events (tenant_id, type, payload, idempotency_key) VALUES ($1, 'a.b', '{}', $2)`, f.tenant, k)
		return err
	}
	if err := reuse(key); err == nil {
		t.Fatal("the key could be reused before cleaning: the fixture is wrong")
	}
	_, err := f.cleaner(72*time.Hour, 0).Once(f.ctx)
	must(t, err)
	if err := reuse(key); err != nil {
		t.Errorf("the key is still taken after its event expired: %v", err)
	}
	if err := reuse(other); err == nil {
		t.Error("the key of a recent event was freed")
	}
}

func TestPeriodZeroKeepsEverything(t *testing.T) {
	f := newFixture(t)
	ev := f.event(100000, nil)
	d := f.delivery(ev, 0, "dead", 100000)

	c := f.cleaner(0, 0)
	if c.Enabled() {
		t.Error("a zero period must turn the cleaner off")
	}
	res, err := c.Once(f.ctx)
	if err != nil || res != (Result{}) {
		t.Fatalf("Once = %+v, %v; want nothing", res, err)
	}
	if !f.deliveryExists(d) || !f.eventExists(ev) {
		t.Error("data was deleted with retention off")
	}
	done := make(chan struct{})
	go func() { c.Run(f.ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return at once with retention off")
	}
}

// One DELETE removes at most batch_size rows; a pass keeps going until
// nothing expired is left.
func TestDeletionIsBatched(t *testing.T) {
	f := newFixture(t)
	const total = 10
	var deliveries []uuid.UUID
	for range total {
		ev := f.event(200, nil)
		deliveries = append(deliveries, f.delivery(ev, 0, "dead", 200))
	}

	q := store.New(testPool)
	retentionMs := (72 * time.Hour).Milliseconds()
	var batches []int64
	for {
		n, err := q.DeleteExpiredDeliveries(f.ctx, store.DeleteExpiredDeliveriesParams{RetentionMs: retentionMs, BatchSize: 3})
		must(t, err)
		if n == 0 {
			break
		}
		batches = append(batches, n)
	}
	// Other tests' leftovers cannot appear here (each test removes its tenant),
	// so the 10 rows go in batches of 3, 3, 3, 1.
	if fmt.Sprint(batches) != "[3 3 3 1]" {
		t.Errorf("batches = %v, want [3 3 3 1]", batches)
	}

	// The events those deliveries left behind go in batches too.
	var eventBatches []int64
	for {
		n, err := q.DeleteExpiredEvents(f.ctx, store.DeleteExpiredEventsParams{RetentionMs: retentionMs, BatchSize: 3})
		must(t, err)
		if n == 0 {
			break
		}
		eventBatches = append(eventBatches, n)
	}
	if fmt.Sprint(eventBatches) != "[3 3 3 1]" {
		t.Errorf("event batches = %v, want [3 3 3 1]", eventBatches)
	}

	// A whole pass with a small batch clears the rest.
	for range total {
		ev := f.event(200, nil)
		deliveries = append(deliveries, f.delivery(ev, 0, "succeeded", 200))
	}
	res, err := f.cleaner(72*time.Hour, 3).Once(f.ctx)
	must(t, err)
	if res.Deliveries != total || res.Events < total {
		t.Errorf("result %+v, want %d deliveries and at least %d events", res, total, total)
	}
	for _, d := range deliveries {
		if f.deliveryExists(d) {
			t.Fatal("a pass with a small batch size left expired deliveries behind")
		}
	}
}

// Several workers clean at once: every row is deleted exactly once between
// them (the counts add up) and nothing is left.
func TestConcurrentCleanersDivideTheWork(t *testing.T) {
	f := newFixture(t)
	const total = 60
	for range total {
		ev := f.event(200, nil)
		f.delivery(ev, 0, "dead", 200)
	}

	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		deliveries int64
		events     int64
	)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := f.cleaner(72*time.Hour, 5).Once(f.ctx)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			deliveries += res.Deliveries
			events += res.Events
			mu.Unlock()
		}()
	}
	wg.Wait()
	if deliveries != total {
		t.Errorf("the cleaners reported %d deleted deliveries between them, want exactly %d", deliveries, total)
	}
	var left int
	must(t, testPool.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM deliveries WHERE tenant_id = $1) + (SELECT count(*) FROM events WHERE tenant_id = $1)`, f.tenant).Scan(&left))
	if left != 0 {
		t.Errorf("%d rows are left", left)
	}
	if events < total {
		t.Errorf("only %d of %d events were deleted", events, total)
	}
}

// A row another transaction holds is skipped, not waited for: the cleaner
// never blocks behind the worker or a replay, and a delivery that is made
// pending again (a replay) in the meantime is not deleted afterwards.
func TestCleanerSkipsLockedRowsAndKeepsReplayedOnes(t *testing.T) {
	f := newFixture(t)
	ev := f.event(200, nil)
	d := f.delivery(ev, 0, "dead", 200)

	tx, err := testPool.Begin(f.ctx)
	must(t, err)
	defer tx.Rollback(f.ctx)
	_, err = tx.Exec(f.ctx, `SELECT 1 FROM deliveries WHERE id = $1 FOR UPDATE`, d)
	must(t, err)

	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	defer cancel()
	res, err := f.cleaner(72*time.Hour, 0).Once(ctx)
	if err != nil {
		t.Fatalf("the cleaner waited for a locked row: %v", err)
	}
	if res.Deliveries != 0 {
		t.Errorf("the locked delivery was deleted: %+v", res)
	}

	// The holder replays it: the delivery is pending again and must survive.
	_, err = tx.Exec(f.ctx, `UPDATE deliveries SET status = 'pending', completed_at = NULL, next_attempt_at = now() WHERE id = $1`, d)
	must(t, err)
	must(t, tx.Commit(f.ctx))
	if _, err := f.cleaner(72*time.Hour, 0).Once(f.ctx); err != nil {
		t.Fatal(err)
	}
	if !f.deliveryExists(d) || !f.eventExists(ev) {
		t.Error("a replayed delivery or its event was deleted")
	}
}

// Fan-out (ingesting an event) holds a key-share lock on the event row while
// it adds deliveries. The cleaner must skip such an event, not wait for it.
func TestCleanerSkipsAnEventBeingFannedOut(t *testing.T) {
	f := newFixture(t)
	ev := f.event(200, nil)

	tx, err := testPool.Begin(f.ctx)
	must(t, err)
	defer tx.Rollback(f.ctx)
	_, err = tx.Exec(f.ctx, `SELECT 1 FROM events WHERE id = $1 FOR KEY SHARE`, ev)
	must(t, err)

	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	defer cancel()
	if _, err := f.cleaner(72*time.Hour, 0).Once(ctx); err != nil {
		t.Fatalf("the cleaner waited for an event being fanned out: %v", err)
	}
	if !f.eventExists(ev) {
		t.Error("an event being fanned out was deleted")
	}
}

func TestRunCleansOnStartAndStopsWithItsContext(t *testing.T) {
	f := newFixture(t)
	ev := f.event(200, nil)
	d := f.delivery(ev, 0, "dead", 200)

	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan struct{})
	c := f.cleaner(72*time.Hour, 0)
	go func() { c.Run(ctx); close(done) }()

	deadline := time.Now().Add(5 * time.Second)
	for f.deliveryExists(d) || f.eventExists(ev) {
		if time.Now().After(deadline) {
			t.Fatal("Run did not clean when it started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop when its context ended")
	}
}

// recorder is a store.DBTX that remembers the statements it is given.
type recorder struct {
	store.DBTX
	mu   sync.Mutex
	sqls []string
	args [][]any
}

func (r *recorder) note(sql string, args []any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sqls = append(r.sqls, sql)
	r.args = append(r.args, args)
}

func (r *recorder) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.note(sql, args)
	return r.DBTX.Exec(ctx, sql, args...)
}

func (r *recorder) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.note(sql, args)
	return r.DBTX.Query(ctx, sql, args...)
}

func (r *recorder) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	r.note(sql, args)
	return r.DBTX.QueryRow(ctx, sql, args...)
}

// The statements the cleaner really sends (not copies of them) find their
// rows through the indexes the migration adds, not by scanning the tables.
func TestRetentionQueriesUseTheirIndexes(t *testing.T) {
	f := newFixture(t)
	rec := &recorder{DBTX: testPool}
	c, err := New(rec, quiet(), Config{Period: 72 * time.Hour, Interval: time.Hour})
	must(t, err)
	if _, err := c.Once(f.ctx); err != nil {
		t.Fatal(err)
	}
	if len(rec.sqls) != 2 {
		t.Fatalf("the cleaner sent %d statements, want 2 (deliveries, events)", len(rec.sqls))
	}

	tx, err := testPool.Begin(f.ctx)
	must(t, err)
	defer tx.Rollback(f.ctx)
	_, err = tx.Exec(f.ctx, `SET LOCAL enable_seqscan = off`)
	must(t, err)
	for i, want := range []string{"deliveries_finished_idx", "events_created_idx"} {
		rows, err := tx.Query(f.ctx, "EXPLAIN "+rec.sqls[i], rec.args[i]...)
		must(t, err)
		var plan strings.Builder
		for rows.Next() {
			var line string
			must(t, rows.Scan(&line))
			plan.WriteString(line + "\n")
		}
		rows.Close()
		// With sequential scans disabled a Seq Scan only shows up when no index
		// can serve some part of the statement (including the part that picks
		// the rows, which the DELETE's own index use would otherwise hide).
		if !strings.Contains(plan.String(), want) || strings.Contains(plan.String(), "Seq Scan") {
			t.Errorf("statement %d does not find its rows through %s alone:\n%s", i+1, want, plan.String())
		}
	}
}

func TestConfigValidation(t *testing.T) {
	for name, cfg := range map[string]Config{
		"negative period": {Period: -time.Hour, Interval: time.Hour},
		"zero interval":   {Period: time.Hour},
		"negative batch":  {Period: time.Hour, Interval: time.Hour, BatchSize: -1},
	} {
		if _, err := New(nil, quiet(), cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
