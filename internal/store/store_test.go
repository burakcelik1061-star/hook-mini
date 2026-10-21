package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/hook-mini/internal/database"
	"github.com/burakcelik1061-star/hook-mini/internal/store"
)

// These tests exercise the schema and the generated queries together against a
// real Postgres. Point TEST_DATABASE_URL at a throwaway database:
//
//	TEST_DATABASE_URL=postgres://hookmini:hookmini@127.0.0.1:5432/hookmini?sslmode=disable
//
// Every test runs inside a transaction that is rolled back, so tests never see
// each other's rows and leave nothing behind. Statements that are expected to
// fail run inside a savepoint, because a failed statement would otherwise
// poison the whole transaction.

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		os.Exit(m.Run()) // every test skips itself
	}

	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
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

func begin(t *testing.T) (context.Context, pgx.Tx, *store.Queries) {
	t.Helper()
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return ctx, tx, store.New(tx)
}

// requireViolation runs fn in a savepoint and asserts that Postgres rejected
// it by the named constraint, so a test cannot pass because of the wrong rule.
func requireViolation(t *testing.T, tx pgx.Tx, constraint string, fn func(pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	defer func() { _ = sp.Rollback(ctx) }()

	err = fn(sp)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("want violation of %q, got %v", constraint, err)
	}
	if pgErr.ConstraintName != constraint {
		t.Fatalf("violated %q (%s), want %q", pgErr.ConstraintName, pgErr.Message, constraint)
	}
}

func exec(ctx context.Context, sql string, args ...any) func(pgx.Tx) error {
	return func(sp pgx.Tx) error {
		_, err := sp.Exec(ctx, sql, args...)
		return err
	}
}

const secret = "whsec_0123456789abcdef01234567" // >= 24 chars

func newTenant(t *testing.T, ctx context.Context, q *store.Queries) store.Tenant {
	t.Helper()
	tn, err := q.CreateTenant(ctx, "Acme "+uuid.NewString()[:8])
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	return tn
}

func newEndpoint(t *testing.T, ctx context.Context, q *store.Queries, tenant uuid.UUID) store.Endpoint {
	t.Helper()
	ep, err := q.CreateEndpoint(ctx, store.CreateEndpointParams{
		TenantID: tenant, Url: "https://example.invalid/hooks/" + uuid.NewString()[:8], Secret: secret,
	})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	return ep
}

func newEvent(t *testing.T, ctx context.Context, q *store.Queries, tenant uuid.UUID, key *string) store.Event {
	t.Helper()
	ev, err := q.CreateEvent(ctx, store.CreateEventParams{
		TenantID: tenant, Type: "invoice.paid", Payload: json.RawMessage(`{"amount":42}`), IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	return ev
}

// newDelivery builds tenant -> endpoint -> event -> one pending delivery.
func newDelivery(t *testing.T, ctx context.Context, q *store.Queries) (store.Tenant, store.Endpoint, store.Event, store.Delivery) {
	t.Helper()
	tn := newTenant(t, ctx, q)
	ep := newEndpoint(t, ctx, q, tn.ID)
	ev := newEvent(t, ctx, q, tn.ID, nil)
	ds, err := q.CreateDeliveriesForEvent(ctx, store.CreateDeliveriesForEventParams{EventID: ev.ID, TenantID: tn.ID})
	if err != nil || len(ds) != 1 {
		t.Fatalf("CreateDeliveriesForEvent: %v (%d rows)", err, len(ds))
	}
	return tn, ep, ev, ds[0]
}

func ptr[T any](v T) *T { return &v }

func cmpUUID(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }

func TestDeliveryCannotCrossTenants(t *testing.T) {
	ctx, tx, q := begin(t)
	a, b := newTenant(t, ctx, q), newTenant(t, ctx, q)
	evA := newEvent(t, ctx, q, a.ID, nil)
	epB := newEndpoint(t, ctx, q, b.ID)

	// A tenant-A delivery pointing at tenant B's endpoint must be impossible,
	// even for code that bypasses the query layer.
	requireViolation(t, tx, "deliveries_endpoint_fk", exec(ctx,
		`INSERT INTO deliveries (tenant_id, event_id, endpoint_id) VALUES ($1, $2, $3)`,
		a.ID, evA.ID, epB.ID))

	// And the same for an event from another tenant.
	evB := newEvent(t, ctx, q, b.ID, nil)
	epA := newEndpoint(t, ctx, q, a.ID)
	requireViolation(t, tx, "deliveries_event_fk", exec(ctx,
		`INSERT INTO deliveries (tenant_id, event_id, endpoint_id) VALUES ($1, $2, $3)`,
		a.ID, evB.ID, epA.ID))
}

func TestTenantScopedLookups(t *testing.T) {
	ctx, _, q := begin(t)
	a, b := newTenant(t, ctx, q), newTenant(t, ctx, q)
	epA := newEndpoint(t, ctx, q, a.ID)
	evA := newEvent(t, ctx, q, a.ID, nil)

	if _, err := q.GetEndpoint(ctx, store.GetEndpointParams{ID: epA.ID, TenantID: b.ID}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetEndpoint with the wrong tenant: err = %v, want ErrNoRows", err)
	}
	if _, err := q.GetEvent(ctx, store.GetEventParams{ID: evA.ID, TenantID: b.ID}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetEvent with the wrong tenant: err = %v, want ErrNoRows", err)
	}
	if n, err := q.DeleteEndpoint(ctx, store.DeleteEndpointParams{ID: epA.ID, TenantID: b.ID}); err != nil || n != 0 {
		t.Errorf("DeleteEndpoint with the wrong tenant: n=%d err=%v, want 0 rows", n, err)
	}
	if _, err := q.UpdateEndpoint(ctx, store.UpdateEndpointParams{ID: epA.ID, TenantID: b.ID, Url: ptr("https://evil.example")}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("UpdateEndpoint with the wrong tenant: err = %v, want ErrNoRows", err)
	}
	if got, err := q.GetEndpoint(ctx, store.GetEndpointParams{ID: epA.ID, TenantID: a.ID}); err != nil || got.Url != epA.Url {
		t.Errorf("owner can no longer read its endpoint: %v", err)
	}
}

func TestFanOutTargetsOnlyActiveEndpointsOfTheTenant(t *testing.T) {
	ctx, _, q := begin(t)
	a, b := newTenant(t, ctx, q), newTenant(t, ctx, q)
	active1, active2 := newEndpoint(t, ctx, q, a.ID), newEndpoint(t, ctx, q, a.ID)
	off := newEndpoint(t, ctx, q, a.ID)
	other := newEndpoint(t, ctx, q, b.ID)

	if n, err := q.DisableEndpoint(ctx, store.DisableEndpointParams{ID: off.ID, DisabledReason: ptr("manual")}); err != nil || n != 1 {
		t.Fatalf("DisableEndpoint: n=%d err=%v", n, err)
	}

	ev := newEvent(t, ctx, q, a.ID, nil)
	ds, err := q.CreateDeliveriesForEvent(ctx, store.CreateDeliveriesForEventParams{EventID: ev.ID, TenantID: a.ID})
	if err != nil {
		t.Fatalf("fan-out: %v", err)
	}
	var got []uuid.UUID
	for _, d := range ds {
		got = append(got, d.EndpointID)
		if d.Status != "pending" || d.AttemptCount != 0 || d.TenantID != a.ID {
			t.Errorf("unexpected new delivery: %+v", d)
		}
	}
	want := []uuid.UUID{active1.ID, active2.ID}
	slices.SortFunc(got, cmpUUID)
	slices.SortFunc(want, cmpUUID)
	if !slices.Equal(got, want) {
		t.Errorf("fan-out endpoints = %v, want %v (disabled %v and other tenant's %v must be excluded)", got, want, off.ID, other.ID)
	}

	// Repeating the fan-out must not create duplicates.
	again, err := q.CreateDeliveriesForEvent(ctx, store.CreateDeliveriesForEventParams{EventID: ev.ID, TenantID: a.ID})
	if err != nil || len(again) != 0 {
		t.Errorf("second fan-out: %d rows, err=%v, want 0 (idempotent)", len(again), err)
	}
}

func TestEventIdempotency(t *testing.T) {
	ctx, _, q := begin(t)
	a, b := newTenant(t, ctx, q), newTenant(t, ctx, q)
	key := ptr("order-1001")

	first := newEvent(t, ctx, q, a.ID, key)

	// Same tenant, same key: nothing inserted, no row returned.
	_, err := q.CreateEvent(ctx, store.CreateEventParams{
		TenantID: a.ID, Type: "invoice.paid", Payload: json.RawMessage(`{"amount":99}`), IdempotencyKey: key,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("replayed CreateEvent: err = %v, want ErrNoRows", err)
	}
	orig, err := q.GetEventByIdempotencyKey(ctx, store.GetEventByIdempotencyKeyParams{TenantID: a.ID, IdempotencyKey: key})
	if err != nil || orig.ID != first.ID {
		t.Fatalf("GetEventByIdempotencyKey: %v, id %v want %v", err, orig.ID, first.ID)
	}
	var payload map[string]int
	if err := json.Unmarshal(orig.Payload, &payload); err != nil || payload["amount"] != 42 {
		t.Errorf("original payload was overwritten: %s", orig.Payload)
	}

	// The same key under another tenant is a different event.
	if other := newEvent(t, ctx, q, b.ID, key); other.ID == first.ID {
		t.Error("idempotency keys must be scoped per tenant")
	}

	// Events without a key are never deduplicated.
	n1, n2 := newEvent(t, ctx, q, a.ID, nil), newEvent(t, ctx, q, a.ID, nil)
	if n1.ID == n2.ID {
		t.Error("two keyless events collapsed into one")
	}
}

func TestConstraintsRejectBadData(t *testing.T) {
	ctx, tx, q := begin(t)
	tn, ep, _, d := newDelivery(t, ctx, q)

	t.Run("endpoint url", func(t *testing.T) {
		for _, url := range []string{"ftp://example.invalid", "example.invalid/hook", "http://a b", "https://", ""} {
			requireViolation(t, tx, "endpoints_url_check", exec(ctx,
				`INSERT INTO endpoints (tenant_id, url, secret) VALUES ($1, $2, $3)`, tn.ID, url, secret))
		}
	})
	t.Run("endpoint secret too short", func(t *testing.T) {
		requireViolation(t, tx, "endpoints_secret_check", exec(ctx,
			`INSERT INTO endpoints (tenant_id, url, secret) VALUES ($1, 'https://example.invalid', 'short')`, tn.ID))
	})
	t.Run("endpoint status", func(t *testing.T) {
		requireViolation(t, tx, "endpoints_status_check", exec(ctx,
			`UPDATE endpoints SET status = 'paused' WHERE id = $1`, ep.ID))
	})
	t.Run("disabled endpoint must carry a timestamp", func(t *testing.T) {
		requireViolation(t, tx, "endpoints_disabled_consistent", exec(ctx,
			`UPDATE endpoints SET status = 'disabled' WHERE id = $1`, ep.ID))
		requireViolation(t, tx, "endpoints_disabled_consistent", exec(ctx,
			`UPDATE endpoints SET disabled_at = now() WHERE id = $1`, ep.ID))
	})
	t.Run("event type", func(t *testing.T) {
		for _, typ := range []string{"Invoice.Paid", "invoice..paid", ".invoice", "invoice paid", "", strings.Repeat("a", 129)} {
			requireViolation(t, tx, "events_type_check", exec(ctx,
				`INSERT INTO events (tenant_id, type, payload) VALUES ($1, $2, '{}')`, tn.ID, typ))
		}
	})
	t.Run("event payload must be an object", func(t *testing.T) {
		for _, p := range []string{`[1,2]`, `"text"`, `42`, `null`} {
			requireViolation(t, tx, "events_payload_check", exec(ctx,
				`INSERT INTO events (tenant_id, type, payload) VALUES ($1, 'a.b', $2::jsonb)`, tn.ID, p))
		}
	})
	t.Run("delivery status", func(t *testing.T) {
		requireViolation(t, tx, "deliveries_status_check", exec(ctx,
			`UPDATE deliveries SET status = 'failed' WHERE id = $1`, d.ID))
	})
	t.Run("in_flight needs a lease", func(t *testing.T) {
		requireViolation(t, tx, "deliveries_lease_consistent", exec(ctx,
			`UPDATE deliveries SET status = 'in_flight' WHERE id = $1`, d.ID))
	})
	t.Run("a lease on a non in_flight delivery is rejected", func(t *testing.T) {
		requireViolation(t, tx, "deliveries_lease_consistent", exec(ctx,
			`UPDATE deliveries SET locked_until = now() WHERE id = $1`, d.ID))
		requireViolation(t, tx, "deliveries_lease_consistent", exec(ctx,
			`UPDATE deliveries SET lease_token = gen_random_uuid() WHERE id = $1`, d.ID))
	})
	t.Run("in_flight needs every part of the lease", func(t *testing.T) {
		for _, set := range []string{
			`locked_until = now(), locked_by = 'w1'`,                // no token
			`locked_until = now(), lease_token = gen_random_uuid()`, // no owner
			`locked_by = 'w1', lease_token = gen_random_uuid()`,     // no expiry
		} {
			requireViolation(t, tx, "deliveries_lease_consistent", exec(ctx,
				`UPDATE deliveries SET status = 'in_flight', `+set+` WHERE id = $1`, d.ID))
		}
	})
	t.Run("finished delivery needs completed_at", func(t *testing.T) {
		requireViolation(t, tx, "deliveries_completed_consistent", exec(ctx,
			`UPDATE deliveries SET status = 'succeeded' WHERE id = $1`, d.ID))
		requireViolation(t, tx, "deliveries_completed_consistent", exec(ctx,
			`UPDATE deliveries SET status = 'dead' WHERE id = $1`, d.ID))
	})
	t.Run("negative attempt count", func(t *testing.T) {
		requireViolation(t, tx, "deliveries_attempt_count_check", exec(ctx,
			`UPDATE deliveries SET attempt_count = -1 WHERE id = $1`, d.ID))
	})
	t.Run("http status range", func(t *testing.T) {
		requireViolation(t, tx, "deliveries_last_status_code_check", exec(ctx,
			`UPDATE deliveries SET last_status_code = 999 WHERE id = $1`, d.ID))
	})
	t.Run("valid transitions are accepted", func(t *testing.T) {
		sp, err := tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = sp.Rollback(ctx) }()
		for _, stmt := range []string{
			`UPDATE deliveries SET status = 'in_flight', locked_until = now() + interval '30 seconds', locked_by = 'w1', lease_token = gen_random_uuid() WHERE id = $1`,
			`UPDATE deliveries SET status = 'pending', locked_until = NULL, locked_by = NULL, lease_token = NULL, attempt_count = 1 WHERE id = $1`,
			`UPDATE deliveries SET status = 'succeeded', completed_at = now(), last_status_code = 200 WHERE id = $1`,
		} {
			if _, err := sp.Exec(ctx, stmt, d.ID); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
	})
}

// A handler that marshals a store struct directly must not be able to leak the
// signing secret or the API key hash.
func TestSecretsNeverSerialize(t *testing.T) {
	ctx, _, q := begin(t)
	tn := newTenant(t, ctx, q)
	created := newEndpoint(t, ctx, q, tn.ID)
	const rotated = "whsec_ROTATEDROTATEDROTATEDROTATED00"
	ep, err := q.RotateEndpointSecret(ctx, store.RotateEndpointSecretParams{
		ID: created.ID, TenantID: tn.ID, NewSecret: rotated, GraceMs: 60_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := q.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		TenantID: tn.ID, Name: "k", Prefix: "hm_json_000001", KeyHash: bytes.Repeat([]byte{0xCD}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}

	epJSON, err := json.Marshal(ep)
	if err != nil {
		t.Fatal(err)
	}
	// Neither the current nor the previous secret, by value or by field.
	if strings.Contains(string(epJSON), secret) || strings.Contains(string(epJSON), rotated) {
		t.Errorf("endpoint JSON exposes a secret value: %s", epJSON)
	}
	var fields map[string]any
	_ = json.Unmarshal(epJSON, &fields)
	for _, f := range []string{"Secret", "PreviousSecret"} {
		if _, ok := fields[f]; ok {
			t.Errorf("endpoint JSON has a %s field: %s", f, epJSON)
		}
	}
	if _, ok := fields["PreviousSecretExpiresAt"]; !ok {
		t.Errorf("non-secret fields must stay visible: %s", epJSON)
	}

	keyJSON, err := json.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(keyJSON)), "hash") || strings.Contains(string(keyJSON), "zc3N") {
		t.Errorf("api key JSON exposes the hash: %s", keyJSON)
	}
	// The visible fields must still be there, or the test proves nothing.
	if !strings.Contains(string(epJSON), ep.Url) || !strings.Contains(string(keyJSON), key.Prefix) {
		t.Errorf("expected public fields missing: %s / %s", epJSON, keyJSON)
	}
}

func TestAPIKeyLifecycle(t *testing.T) {
	ctx, tx, q := begin(t)
	a, b := newTenant(t, ctx, q), newTenant(t, ctx, q)
	hash := make([]byte, 32)
	hash[0] = 0xAB

	key, err := q.CreateAPIKey(ctx, store.CreateAPIKeyParams{TenantID: a.ID, Name: "ci", Prefix: "hm_test_ab12cd", KeyHash: hash})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	got, err := q.GetActiveAPIKeyByPrefix(ctx, "hm_test_ab12cd")
	if err != nil || got.ID != key.ID || got.TenantID != a.ID || got.KeyHash[0] != 0xAB {
		t.Fatalf("GetActiveAPIKeyByPrefix: %+v err=%v", got, err)
	}

	requireViolation(t, tx, "api_keys_prefix_key", func(sp pgx.Tx) error {
		_, err := store.New(sp).CreateAPIKey(ctx, store.CreateAPIKeyParams{TenantID: b.ID, Name: "dup", Prefix: "hm_test_ab12cd", KeyHash: hash})
		return err
	})
	requireViolation(t, tx, "api_keys_key_hash_check", func(sp pgx.Tx) error {
		_, err := store.New(sp).CreateAPIKey(ctx, store.CreateAPIKeyParams{TenantID: b.ID, Name: "bad", Prefix: "hm_test_zzzzzz", KeyHash: []byte("short")})
		return err
	})

	// Another tenant cannot revoke it.
	if _, err := q.RevokeAPIKey(ctx, store.RevokeAPIKeyParams{ID: key.ID, TenantID: b.ID}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("revoke by the wrong tenant: err = %v, want ErrNoRows", err)
	}
	if _, err := q.RevokeAPIKey(ctx, store.RevokeAPIKeyParams{ID: key.ID, TenantID: a.ID}); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	if _, err := q.RevokeAPIKey(ctx, store.RevokeAPIKeyParams{ID: key.ID, TenantID: a.ID}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("second revoke: err = %v, want ErrNoRows", err)
	}
	if _, err := q.GetActiveAPIKeyByPrefix(ctx, "hm_test_ab12cd"); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("revoked key still authenticates: err = %v", err)
	}

	// Listing still shows it (as revoked) and, by construction, has no hash field.
	rows, err := q.ListAPIKeys(ctx, a.ID)
	if err != nil || len(rows) != 1 || rows[0].RevokedAt == nil {
		t.Fatalf("ListAPIKeys: %+v err=%v", rows, err)
	}
}

func TestTouchAPIKeyIsThrottled(t *testing.T) {
	ctx, tx, q := begin(t)
	a := newTenant(t, ctx, q)
	key, err := q.CreateAPIKey(ctx, store.CreateAPIKeyParams{TenantID: a.ID, Name: "k", Prefix: "hm_touch_000001", KeyHash: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}

	lastUsed := func() *time.Time {
		var ts *time.Time
		if err := tx.QueryRow(ctx, `SELECT last_used_at FROM api_keys WHERE id = $1`, key.ID).Scan(&ts); err != nil {
			t.Fatal(err)
		}
		return ts
	}

	if lastUsed() != nil {
		t.Fatal("a new key must have no last_used_at")
	}
	if err := q.TouchAPIKey(ctx, key.ID); err != nil || lastUsed() == nil {
		t.Fatalf("first touch should set last_used_at (err=%v)", err)
	}

	recent := time.Now().Add(-10 * time.Second).Truncate(time.Microsecond)
	if _, err := tx.Exec(ctx, `UPDATE api_keys SET last_used_at = $2 WHERE id = $1`, key.ID, recent); err != nil {
		t.Fatal(err)
	}
	_ = q.TouchAPIKey(ctx, key.ID)
	if got := lastUsed(); got == nil || !got.Equal(recent) {
		t.Errorf("a touch within a minute rewrote last_used_at: %v, want %v", got, recent)
	}

	stale := time.Now().Add(-2 * time.Hour).Truncate(time.Microsecond)
	if _, err := tx.Exec(ctx, `UPDATE api_keys SET last_used_at = $2 WHERE id = $1`, key.ID, stale); err != nil {
		t.Fatal(err)
	}
	_ = q.TouchAPIKey(ctx, key.ID)
	if got := lastUsed(); got == nil || !got.After(stale) {
		t.Errorf("a stale last_used_at was not refreshed: %v", got)
	}
}

func TestKeysetPaginationHasNoGapsOrDuplicates(t *testing.T) {
	ctx, tx, q := begin(t)
	tn := newTenant(t, ctx, q)

	var ids []uuid.UUID
	for range 7 {
		ids = append(ids, newEndpoint(t, ctx, q, tn.ID).ID)
	}
	// Inside one transaction now() is constant, so every row shares a
	// created_at. Move three of them back in time: the page boundary then
	// falls both inside a group of equal timestamps and between groups.
	if _, err := tx.Exec(ctx,
		`UPDATE endpoints SET created_at = now() - interval '1 day' WHERE id = ANY($1)`, ids[:3]); err != nil {
		t.Fatal(err)
	}

	type row struct {
		id uuid.UUID
		at time.Time
	}
	var all []row
	rows, err := tx.Query(ctx, `SELECT id, created_at FROM endpoints WHERE tenant_id = $1 ORDER BY created_at DESC, id DESC`, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.at); err != nil {
			t.Fatal(err)
		}
		all = append(all, r)
	}
	rows.Close()

	var walked []uuid.UUID
	p := store.ListEndpointsParams{TenantID: tn.ID, PageSize: 2}
	for pages := 0; pages < 20; pages++ {
		page, err := q.ListEndpoints(ctx, p)
		if err != nil {
			t.Fatalf("ListEndpoints: %v", err)
		}
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			walked = append(walked, e.ID)
		}
		last := page[len(page)-1]
		p.AfterCreatedAt, p.AfterID = &last.CreatedAt, &last.ID
	}

	var want []uuid.UUID
	for _, r := range all {
		want = append(want, r.id)
	}
	if !slices.Equal(walked, want) {
		t.Fatalf("paginated walk differs from a full ordered scan\n got: %v\nwant: %v", walked, want)
	}
	if len(walked) != 7 {
		t.Fatalf("walked %d rows, want 7", len(walked))
	}
}

func TestListDeliveriesFilters(t *testing.T) {
	ctx, tx, q := begin(t)
	tn, ep, _, d := newDelivery(t, ctx, q)
	other := newEndpoint(t, ctx, q, tn.ID)
	ev2 := newEvent(t, ctx, q, tn.ID, nil)
	if _, err := q.CreateDeliveriesForEvent(ctx, store.CreateDeliveriesForEventParams{EventID: ev2.ID, TenantID: tn.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE deliveries SET status = 'succeeded', completed_at = now() WHERE id = $1`, d.ID); err != nil {
		t.Fatal(err)
	}

	count := func(p store.ListDeliveriesParams) int {
		t.Helper()
		p.TenantID, p.PageSize = tn.ID, 50
		rows, err := q.ListDeliveries(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}

	// ep got two deliveries (event 1 and event 2), other got one (event 2).
	if got := count(store.ListDeliveriesParams{}); got != 3 {
		t.Errorf("unfiltered = %d, want 3", got)
	}
	if got := count(store.ListDeliveriesParams{EndpointID: &ep.ID}); got != 2 {
		t.Errorf("by endpoint = %d, want 2", got)
	}
	if got := count(store.ListDeliveriesParams{EndpointID: &other.ID}); got != 1 {
		t.Errorf("by other endpoint = %d, want 1", got)
	}
	if got := count(store.ListDeliveriesParams{Status: ptr("succeeded")}); got != 1 {
		t.Errorf("by status succeeded = %d, want 1", got)
	}
	if got := count(store.ListDeliveriesParams{Status: ptr("pending"), EndpointID: &ep.ID}); got != 1 {
		t.Errorf("by status+endpoint = %d, want 1", got)
	}

	stranger := newTenant(t, ctx, q)
	rows, err := q.ListDeliveries(ctx, store.ListDeliveriesParams{TenantID: stranger.ID, PageSize: 50})
	if err != nil || len(rows) != 0 {
		t.Errorf("another tenant sees %d deliveries (err=%v), want 0", len(rows), err)
	}
}

func TestDeliveryAttempts(t *testing.T) {
	ctx, tx, q := begin(t)
	tn, _, _, d := newDelivery(t, ctx, q)
	stranger := newTenant(t, ctx, q)
	now := time.Now().Truncate(time.Microsecond)

	for n := int32(1); n <= 3; n++ {
		_, err := q.CreateDeliveryAttempt(ctx, store.CreateDeliveryAttemptParams{
			DeliveryID: d.ID, AttemptNo: n, StartedAt: now, DurationMs: 12 * n,
			StatusCode: ptr(int32(500)), ResponseSnippet: ptr("upstream error"),
		})
		if err != nil {
			t.Fatalf("CreateDeliveryAttempt %d: %v", n, err)
		}
	}

	got, err := q.ListDeliveryAttempts(ctx, store.ListDeliveryAttemptsParams{DeliveryID: d.ID, TenantID: tn.ID})
	if err != nil || len(got) != 3 {
		t.Fatalf("ListDeliveryAttempts: %d rows err=%v", len(got), err)
	}
	for i, a := range got {
		if a.AttemptNo != int32(i+1) {
			t.Errorf("attempts not ordered by attempt_no: %v", got)
		}
	}

	leak, err := q.ListDeliveryAttempts(ctx, store.ListDeliveryAttemptsParams{DeliveryID: d.ID, TenantID: stranger.ID})
	if err != nil || len(leak) != 0 {
		t.Errorf("another tenant can read %d attempts (err=%v)", len(leak), err)
	}

	requireViolation(t, tx, "delivery_attempts_delivery_no_key", func(sp pgx.Tx) error {
		_, err := store.New(sp).CreateDeliveryAttempt(ctx, store.CreateDeliveryAttemptParams{
			DeliveryID: d.ID, AttemptNo: 2, StartedAt: now, DurationMs: 1,
		})
		return err
	})
	requireViolation(t, tx, "delivery_attempts_response_snippet_check", func(sp pgx.Tx) error {
		_, err := store.New(sp).CreateDeliveryAttempt(ctx, store.CreateDeliveryAttemptParams{
			DeliveryID: d.ID, AttemptNo: 9, StartedAt: now, DurationMs: 1, ResponseSnippet: ptr(strings.Repeat("x", 1025)),
		})
		return err
	})
	requireViolation(t, tx, "delivery_attempts_attempt_no_check", func(sp pgx.Tx) error {
		_, err := store.New(sp).CreateDeliveryAttempt(ctx, store.CreateDeliveryAttemptParams{
			DeliveryID: d.ID, AttemptNo: 0, StartedAt: now, DurationMs: 1,
		})
		return err
	})
}

func TestEndpointFailureCounterAndDisable(t *testing.T) {
	ctx, _, q := begin(t)
	tn := newTenant(t, ctx, q)
	ep := newEndpoint(t, ctx, q, tn.ID)

	for want := int32(1); want <= 3; want++ {
		got, err := q.RecordEndpointFailure(ctx, ep.ID)
		if err != nil || got != want {
			t.Fatalf("RecordEndpointFailure = %d, %v; want %d", got, err, want)
		}
	}
	if err := q.RecordEndpointSuccess(ctx, ep.ID); err != nil {
		t.Fatal(err)
	}
	if cur, _ := q.GetEndpoint(ctx, store.GetEndpointParams{ID: ep.ID, TenantID: tn.ID}); cur.ConsecutiveFailures != 0 {
		t.Errorf("success did not reset the counter: %d", cur.ConsecutiveFailures)
	}

	if n, err := q.DisableEndpoint(ctx, store.DisableEndpointParams{ID: ep.ID, DisabledReason: ptr("too many failures")}); err != nil || n != 1 {
		t.Fatalf("DisableEndpoint: n=%d err=%v", n, err)
	}
	// Idempotent: a second disable changes nothing and keeps the first reason.
	if n, _ := q.DisableEndpoint(ctx, store.DisableEndpointParams{ID: ep.ID, DisabledReason: ptr("something else")}); n != 0 {
		t.Errorf("second DisableEndpoint affected %d rows, want 0", n)
	}
	cur, _ := q.GetEndpoint(ctx, store.GetEndpointParams{ID: ep.ID, TenantID: tn.ID})
	if cur.Status != "disabled" || cur.DisabledAt == nil || cur.DisabledReason == nil || *cur.DisabledReason != "too many failures" {
		t.Errorf("unexpected disabled endpoint: %+v", cur)
	}

	ids, err := q.ListActiveEndpointIDs(ctx, tn.ID)
	if err != nil || len(ids) != 0 {
		t.Errorf("disabled endpoint is still listed as active: %v (err=%v)", ids, err)
	}

	en, err := q.EnableEndpoint(ctx, store.EnableEndpointParams{ID: ep.ID, TenantID: tn.ID})
	if err != nil || en.Status != "active" || en.DisabledAt != nil || en.DisabledReason != nil || en.ConsecutiveFailures != 0 {
		t.Errorf("EnableEndpoint did not fully reset: %+v err=%v", en, err)
	}
}

func TestUpdatedAtTrigger(t *testing.T) {
	ctx, tx, q := begin(t)
	tn := newTenant(t, ctx, q)
	ep := newEndpoint(t, ctx, q, tn.ID)

	// The trigger must win even when the statement tries to set the column.
	if _, err := tx.Exec(ctx, `UPDATE endpoints SET description = 'x', updated_at = '2000-01-01' WHERE id = $1`, ep.ID); err != nil {
		t.Fatal(err)
	}
	var updated, created time.Time
	if err := tx.QueryRow(ctx, `SELECT updated_at, created_at FROM endpoints WHERE id = $1`, ep.ID).Scan(&updated, &created); err != nil {
		t.Fatal(err)
	}
	if updated.Year() == 2000 || updated.Before(created) {
		t.Errorf("updated_at = %v: set_updated_at trigger did not run", updated)
	}
}

func TestDeletingTenantCascadesEverywhere(t *testing.T) {
	ctx, tx, q := begin(t)
	tn, _, _, d := newDelivery(t, ctx, q)
	if _, err := q.CreateAPIKey(ctx, store.CreateAPIKeyParams{TenantID: tn.ID, Name: "k", Prefix: "hm_casc_0000001", KeyHash: make([]byte, 32)}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateDeliveryAttempt(ctx, store.CreateDeliveryAttemptParams{
		DeliveryID: d.ID, AttemptNo: 1, StartedAt: time.Now(), DurationMs: 1,
	}); err != nil {
		t.Fatal(err)
	}

	if n, err := q.DeleteTenant(ctx, tn.ID); err != nil || n != 1 {
		t.Fatalf("DeleteTenant: n=%d err=%v", n, err)
	}

	for _, table := range []string{"api_keys", "endpoints", "events", "deliveries"} {
		var n int
		if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE tenant_id = $1`, table), tn.ID).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s still has %d rows for the deleted tenant (err=%v)", table, n, err)
		}
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM delivery_attempts WHERE delivery_id = $1`, d.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("delivery_attempts still has %d rows (err=%v)", n, err)
	}
}

func TestWorkerIndexesMatchTheirQueries(t *testing.T) {
	ctx, tx, _ := begin(t)

	// Existence and shape: the due-work index must be partial.
	for name, wantPredicate := range map[string]string{
		"deliveries_due_idx":   "status = 'pending'",
		"deliveries_lease_idx": "status = 'in_flight'",
		"endpoints_active_idx": "status = 'active'",
	} {
		var def string
		if err := tx.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname = $1`, name).Scan(&def); err != nil {
			t.Errorf("index %s missing: %v", name, err)
			continue
		}
		if !strings.Contains(def, wantPredicate) {
			t.Errorf("index %s is not partial on %q: %s", name, wantPredicate, def)
		}
	}

	// Usability: with sequential scans disabled the planner must still pick
	// the partial index for the worker's "next due" predicate. If the query's
	// WHERE clause did not imply the index predicate, it could not.
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, `EXPLAIN SELECT id FROM deliveries
		WHERE status = 'pending' AND next_attempt_at <= now()
		ORDER BY next_attempt_at LIMIT 10`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if !strings.Contains(plan.String(), "deliveries_due_idx") {
		t.Errorf("the due-work query does not use deliveries_due_idx:\n%s", plan.String())
	}
}
