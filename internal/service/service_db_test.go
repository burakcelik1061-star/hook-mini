package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/hook-mini/internal/database"
	"github.com/burakcelik1061-star/hook-mini/internal/safedial"
	"github.com/burakcelik1061-star/hook-mini/internal/store"
)

// These tests run the use cases against a real Postgres. Point
// TEST_DATABASE_URL at a throwaway database (see README). Without it they skip.
//
// The service owns its transactions, so unlike the store tests they cannot be
// rolled back; each test creates its own tenant and deletes it on cleanup
// (which cascades to everything the test created).

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		os.Exit(m.Run())
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

func newService(t *testing.T) *Service {
	t.Helper()
	if testPool == nil {
		t.Skip("TEST_DATABASE_URL not set")
	}
	return New(testPool, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// newTenant creates a tenant that is removed when the test ends.
func newTenant(t *testing.T, s *Service) store.Tenant {
	t.Helper()
	tn, err := s.CreateTenant(context.Background(), "Test "+uuid.NewString()[:8])
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	t.Cleanup(func() { _, _ = s.q.DeleteTenant(context.Background(), tn.ID) })
	return tn
}

func newEndpoint(t *testing.T, s *Service, tenant uuid.UUID) store.Endpoint {
	t.Helper()
	ep, err := s.CreateEndpoint(context.Background(), tenant, EndpointInput{
		URL: "https://example.invalid/hooks/" + uuid.NewString()[:8],
	})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	return ep
}

func ctx() context.Context { return context.Background() }

func count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(ctx(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAuthenticate(t *testing.T) {
	s := newService(t)
	tn := newTenant(t, s)

	row, full, err := s.IssueAPIKey(ctx(), tn.ID, "ci")
	if err != nil {
		t.Fatalf("IssueAPIKey: %v", err)
	}

	p, err := s.Authenticate(ctx(), full)
	if err != nil || p.TenantID != tn.ID || p.KeyID != row.ID {
		t.Fatalf("Authenticate(valid) = %+v, %v; want tenant %v key %v", p, err, tn.ID, row.ID)
	}

	// The same prefix with a wrong secret, an unknown prefix, malformed input
	// and the empty string must all look identical: ErrUnauthorized.
	wrongSecret := full[:len(full)-1] + map[bool]string{true: "0", false: "1"}[full[len(full)-1] != '0']
	unknownPrefix := "hm_00000000_" + strings.Repeat("a", 64)
	for name, in := range map[string]string{
		"wrong secret":   wrongSecret,
		"unknown prefix": unknownPrefix,
		"malformed":      "not-a-key",
		"empty":          "",
		"with bearer":    "Bearer " + full,
	} {
		if _, err := s.Authenticate(ctx(), in); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: err = %v, want ErrUnauthorized", name, err)
		}
	}

	if err := s.RevokeAPIKey(ctx(), tn.ID, row.ID); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	if _, err := s.Authenticate(ctx(), full); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("a revoked key still authenticates: %v", err)
	}
}

func TestIssueAPIKeyEdgeCases(t *testing.T) {
	s := newService(t)
	tn := newTenant(t, s)

	if _, _, err := s.IssueAPIKey(ctx(), uuid.New(), "ci"); !errors.Is(err, ErrNotFound) {
		t.Errorf("key for an unknown tenant: err = %v, want ErrNotFound", err)
	}
	_, _, err := s.IssueAPIKey(ctx(), tn.ID, "   ")
	wantField(t, err, "name")

	// The stored row must not contain the key itself.
	row, full, err := s.IssueAPIKey(ctx(), tn.ID, "second")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(row.KeyHash), full) || strings.Contains(row.Prefix, full[len(row.Prefix)+1:]) {
		t.Error("the stored row contains the secret part of the key")
	}
}

func TestEndpointLifecycleAndIsolation(t *testing.T) {
	s := newService(t)
	a, b := newTenant(t, s), newTenant(t, s)

	ep, err := s.CreateEndpoint(ctx(), a.ID, EndpointInput{URL: "https://example.invalid/hook", Description: "billing"})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if !strings.HasPrefix(ep.Secret, "whsec_") || ep.Status != "active" || ep.Description != "billing" {
		t.Errorf("unexpected new endpoint: %+v", ep)
	}

	got, err := s.GetEndpoint(ctx(), a.ID, ep.ID)
	if err != nil || got.ID != ep.ID || got.Secret != ep.Secret {
		t.Fatalf("owner cannot read its endpoint: %v", err)
	}

	// Every operation by another tenant behaves as if the endpoint did not exist.
	if _, err := s.GetEndpoint(ctx(), b.ID, ep.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get by another tenant: %v, want ErrNotFound", err)
	}
	if _, err := s.UpdateEndpoint(ctx(), b.ID, ep.ID, EndpointPatch{Description: ptr("hijacked")}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update by another tenant: %v, want ErrNotFound", err)
	}
	if err := s.DeleteEndpoint(ctx(), b.ID, ep.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete by another tenant: %v, want ErrNotFound", err)
	}
	if res, err := s.ListEndpoints(ctx(), b.ID, Page{}); err != nil || len(res.Items) != 0 {
		t.Errorf("another tenant lists %d endpoints (err=%v)", len(res.Items), err)
	}

	// Partial update changes only what is sent.
	up, err := s.UpdateEndpoint(ctx(), a.ID, ep.ID, EndpointPatch{URL: ptr("https://example.invalid/new")})
	if err != nil || up.Url != "https://example.invalid/new" || up.Description != "billing" {
		t.Fatalf("partial update: %+v err=%v", up, err)
	}
	up, err = s.UpdateEndpoint(ctx(), a.ID, ep.ID, EndpointPatch{Description: ptr("")})
	if err != nil || up.Description != "" || up.Url != "https://example.invalid/new" {
		t.Fatalf("clearing the description: %+v err=%v", up, err)
	}
	if up.Secret != ep.Secret {
		t.Error("an update must not change the signing secret")
	}

	_, err = s.UpdateEndpoint(ctx(), a.ID, ep.ID, EndpointPatch{})
	wantField(t, err, "body")
	_, err = s.UpdateEndpoint(ctx(), a.ID, ep.ID, EndpointPatch{URL: ptr("ftp://nope")})
	wantField(t, err, "url")

	if err := s.DeleteEndpoint(ctx(), a.ID, ep.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.DeleteEndpoint(ctx(), a.ID, ep.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v, want ErrNotFound", err)
	}
}

func TestEndpointURLsPointingInsideAreRejected(t *testing.T) {
	s := newService(t)
	tn := newTenant(t, s)
	ok := newEndpoint(t, s, tn.ID)

	for _, u := range []string{
		"http://127.0.0.1/hook", "http://127.0.0.1:5432/", "http://localhost:8080/", "http://api.localhost/",
		"http://[::1]/", "http://10.0.0.8/", "http://192.168.1.10/", "http://169.254.169.254/latest/meta-data/",
		"http://[::ffff:127.0.0.1]/", "http://0.0.0.0/",
	} {
		_, err := s.CreateEndpoint(ctx(), tn.ID, EndpointInput{URL: u})
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Field != "url" || !strings.Contains(ve.Message, "not allowed") {
			t.Errorf("create %s: err = %v, want a url validation error", u, err)
		}
		if _, err := s.UpdateEndpoint(ctx(), tn.ID, ok.ID, EndpointPatch{URL: &u}); !errors.As(err, &ve) {
			t.Errorf("update to %s: err = %v, want a url validation error", u, err)
		}
	}

	// Development setups can opt in.
	dev := New(testPool, s.log, WithEgressPolicy(safedial.Policy{AllowPrivate: true}))
	if _, err := dev.CreateEndpoint(ctx(), tn.ID, EndpointInput{URL: "http://127.0.0.1:9000/hook"}); err != nil {
		t.Errorf("with private destinations allowed: %v", err)
	}
}

func TestRotateEndpointSecret(t *testing.T) {
	s := newService(t)
	a, b := newTenant(t, s), newTenant(t, s)
	ep := newEndpoint(t, s, a.ID)

	r1, err := s.RotateEndpointSecret(ctx(), a.ID, ep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Secret == ep.Secret || !strings.HasPrefix(r1.Secret, "whsec_") {
		t.Errorf("new secret %q is not a fresh secret", r1.Secret)
	}
	if r1.PreviousSecret == nil || *r1.PreviousSecret != ep.Secret {
		t.Error("the old secret was not kept as the previous one")
	}
	if r1.PreviousSecretExpiresAt == nil {
		t.Fatal("no expiry for the previous secret")
	}
	if d := time.Until(*r1.PreviousSecretExpiresAt); d < SecretRotationGrace-time.Minute || d > SecretRotationGrace+time.Minute {
		t.Errorf("previous secret expires in %s, want about %s", d, SecretRotationGrace)
	}

	// A second rotation keeps only the immediately previous secret.
	r2, err := s.RotateEndpointSecret(ctx(), a.ID, ep.ID)
	if err != nil || *r2.PreviousSecret != r1.Secret || r2.Secret == r1.Secret {
		t.Errorf("second rotation: previous=%v err=%v", r2.PreviousSecret, err)
	}

	if _, err := s.RotateEndpointSecret(ctx(), b.ID, ep.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("rotation by another tenant: %v, want ErrNotFound", err)
	}
	if got, _ := s.GetEndpoint(ctx(), a.ID, ep.ID); got.Secret != r2.Secret {
		t.Error("the other tenant's attempt changed the secret")
	}
}

func TestCreateEndpointValidation(t *testing.T) {
	s := newService(t)
	tn := newTenant(t, s)

	_, err := s.CreateEndpoint(ctx(), tn.ID, EndpointInput{URL: ""})
	wantField(t, err, "url")
	_, err = s.CreateEndpoint(ctx(), tn.ID, EndpointInput{URL: "https://example.invalid", Description: strings.Repeat("x", 501)})
	wantField(t, err, "description")

	// Two endpoints with the same URL are allowed (different secrets).
	a, err1 := s.CreateEndpoint(ctx(), tn.ID, EndpointInput{URL: "https://example.invalid/same"})
	b, err2 := s.CreateEndpoint(ctx(), tn.ID, EndpointInput{URL: "https://example.invalid/same"})
	if err1 != nil || err2 != nil || a.Secret == b.Secret {
		t.Errorf("duplicate URLs: %v %v, equal secrets: %v", err1, err2, a.Secret == b.Secret)
	}
}

func TestListPaginationWalksEverythingOnce(t *testing.T) {
	s := newService(t)
	tn := newTenant(t, s)

	want := map[uuid.UUID]bool{}
	for range 8 {
		want[newEndpoint(t, s, tn.ID).ID] = true
	}

	seen := map[uuid.UUID]bool{}
	cursor := ""
	pages := 0
	for {
		res, err := s.ListEndpoints(ctx(), tn.ID, Page{Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, e := range res.Items {
			if seen[e.ID] {
				t.Fatalf("endpoint %v returned on two pages", e.ID)
			}
			seen[e.ID] = true
		}
		if res.NextCursor == "" {
			break
		}
		if len(res.Items) != 3 {
			t.Fatalf("a non-final page has %d items, want 3", len(res.Items))
		}
		cursor = res.NextCursor
		if pages > 10 {
			t.Fatal("pagination does not terminate")
		}
	}
	if len(seen) != len(want) || pages != 3 {
		t.Errorf("walked %d endpoints in %d pages, want 8 in 3", len(seen), pages)
	}

	_, err := s.ListEndpoints(ctx(), tn.ID, Page{Cursor: "garbage"})
	wantField(t, err, "cursor")
	_, err = s.ListEndpoints(ctx(), tn.ID, Page{Limit: MaxPageSize + 1})
	wantField(t, err, "limit")
}

func TestIngestFansOutToActiveEndpointsOfTheTenantOnly(t *testing.T) {
	s := newService(t)
	a, b := newTenant(t, s), newTenant(t, s)
	e1, _ := newEndpoint(t, s, a.ID), newEndpoint(t, s, a.ID)
	off := newEndpoint(t, s, a.ID)
	newEndpoint(t, s, b.ID)
	if _, err := s.q.DisableEndpoint(ctx(), store.DisableEndpointParams{ID: off.ID, DisabledReason: ptr("test")}); err != nil {
		t.Fatal(err)
	}

	res, err := s.IngestEvent(ctx(), a.ID, IngestInput{Type: "invoice.paid", Payload: json.RawMessage(`{"amount":42}`)})
	if err != nil {
		t.Fatalf("IngestEvent: %v", err)
	}
	if res.Replayed || res.DeliveryCount != 2 {
		t.Errorf("replayed=%v deliveries=%d, want a fresh event with 2 deliveries", res.Replayed, res.DeliveryCount)
	}
	if n := count(t, `SELECT count(*) FROM deliveries WHERE event_id = $1 AND status = 'pending'`, res.Event.ID); n != 2 {
		t.Errorf("%d pending deliveries stored, want 2", n)
	}
	if n := count(t, `SELECT count(*) FROM deliveries WHERE event_id = $1 AND endpoint_id = $2`, res.Event.ID, e1.ID); n != 1 {
		t.Errorf("the first endpoint has %d deliveries, want 1", n)
	}
	if n := count(t, `SELECT count(*) FROM deliveries WHERE event_id = $1 AND endpoint_id = $2`, res.Event.ID, off.ID); n != 0 {
		t.Errorf("a disabled endpoint received %d deliveries", n)
	}

	got, err := s.GetEvent(ctx(), a.ID, res.Event.ID)
	if err != nil || got.DeliveryCount != 2 {
		t.Errorf("GetEvent: %+v err=%v", got, err)
	}
	if _, err := s.GetEvent(ctx(), b.ID, res.Event.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another tenant reads the event: %v", err)
	}
}

func TestIngestWithoutEndpointsStillStoresTheEvent(t *testing.T) {
	s := newService(t)
	tn := newTenant(t, s)
	res, err := s.IngestEvent(ctx(), tn.ID, IngestInput{Type: "user.created", Payload: json.RawMessage(`{"id":1}`)})
	if err != nil || res.DeliveryCount != 0 || res.Replayed {
		t.Fatalf("got %+v err=%v, want a stored event with 0 deliveries", res, err)
	}
	if n := count(t, `SELECT count(*) FROM events WHERE id = $1`, res.Event.ID); n != 1 {
		t.Errorf("event not stored")
	}
}

func TestIngestIdempotency(t *testing.T) {
	s := newService(t)
	tn := newTenant(t, s)
	newEndpoint(t, s, tn.ID)
	newEndpoint(t, s, tn.ID)
	key := ptr("order-1001")

	first, err := s.IngestEvent(ctx(), tn.ID, IngestInput{
		Type: "invoice.paid", Payload: json.RawMessage(`{"amount":42,"currency":"EUR"}`), IdempotencyKey: key,
	})
	if err != nil || first.Replayed || first.DeliveryCount != 2 {
		t.Fatalf("first: %+v err=%v", first, err)
	}

	// Same content, different key order and whitespace: a replay.
	again, err := s.IngestEvent(ctx(), tn.ID, IngestInput{
		Type: "invoice.paid", Payload: json.RawMessage(`{ "currency" : "EUR",   "amount": 42 }`), IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !again.Replayed || again.Event.ID != first.Event.ID || again.DeliveryCount != 2 {
		t.Errorf("replay: %+v, want the original event %v with 2 deliveries", again, first.Event.ID)
	}

	if n := count(t, `SELECT count(*) FROM events WHERE tenant_id = $1`, tn.ID); n != 1 {
		t.Errorf("%d events stored, want 1", n)
	}
	if n := count(t, `SELECT count(*) FROM deliveries WHERE tenant_id = $1`, tn.ID); n != 2 {
		t.Errorf("%d deliveries stored, want 2 (a replay must not fan out again)", n)
	}

	// Same key, different content: rejected, and nothing changes.
	for name, in := range map[string]IngestInput{
		"different payload": {Type: "invoice.paid", Payload: json.RawMessage(`{"amount":43,"currency":"EUR"}`), IdempotencyKey: key},
		"different type":    {Type: "invoice.void", Payload: json.RawMessage(`{"amount":42,"currency":"EUR"}`), IdempotencyKey: key},
		"extra field":       {Type: "invoice.paid", Payload: json.RawMessage(`{"amount":42,"currency":"EUR","x":1}`), IdempotencyKey: key},
	} {
		if _, err := s.IngestEvent(ctx(), tn.ID, in); !errors.Is(err, ErrIdempotencyMismatch) {
			t.Errorf("%s: err = %v, want ErrIdempotencyMismatch", name, err)
		}
	}
	if n := count(t, `SELECT count(*) FROM events WHERE tenant_id = $1`, tn.ID); n != 1 {
		t.Errorf("a rejected replay changed the data: %d events", n)
	}

	// The same key belongs to another tenant independently.
	other := newTenant(t, s)
	res, err := s.IngestEvent(ctx(), other.ID, IngestInput{Type: "invoice.paid", Payload: json.RawMessage(`{"amount":1}`), IdempotencyKey: key})
	if err != nil || res.Replayed || res.Event.ID == first.Event.ID {
		t.Errorf("idempotency keys must be scoped per tenant: %+v err=%v", res, err)
	}
}

// The property the whole service depends on: if a client retries a request
// while the first attempt is still in flight, exactly one event exists.
func TestIngestConcurrentSameKeyCreatesExactlyOneEvent(t *testing.T) {
	s := newService(t)
	tn := newTenant(t, s)
	for range 3 {
		newEndpoint(t, s, tn.ID)
	}

	const workers = 24
	var (
		wg       sync.WaitGroup
		start    = make(chan struct{})
		mu       sync.Mutex
		results  []EventResult
		failures []error
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := s.IngestEvent(ctx(), tn.ID, IngestInput{
				Type: "payment.captured", Payload: json.RawMessage(`{"id":"pay_1"}`), IdempotencyKey: ptr("pay_1-attempt"),
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			results = append(results, res)
		}()
	}
	close(start) // release every goroutine at once
	wg.Wait()

	if len(failures) > 0 {
		t.Fatalf("%d of %d concurrent requests failed, first: %v", len(failures), workers, failures[0])
	}
	created, ids := 0, map[uuid.UUID]bool{}
	for _, r := range results {
		ids[r.Event.ID] = true
		if !r.Replayed {
			created++
		}
		if r.DeliveryCount != 3 {
			t.Errorf("a response reports %d deliveries, want 3", r.DeliveryCount)
		}
	}
	if created != 1 || len(ids) != 1 {
		t.Errorf("%d requests created an event and %d distinct events were returned; want exactly 1 and 1", created, len(ids))
	}
	if n := count(t, `SELECT count(*) FROM events WHERE tenant_id = $1`, tn.ID); n != 1 {
		t.Errorf("%d events stored, want 1", n)
	}
	if n := count(t, `SELECT count(*) FROM deliveries WHERE tenant_id = $1`, tn.ID); n != 3 {
		t.Errorf("%d deliveries stored, want 3", n)
	}
}

func TestIngestValidation(t *testing.T) {
	s := newService(t)
	tn := newTenant(t, s)

	cases := map[string]struct {
		in    IngestInput
		field string
	}{
		"bad type":      {IngestInput{Type: "Bad Type", Payload: json.RawMessage(`{}`)}, "type"},
		"no payload":    {IngestInput{Type: "a.b"}, "payload"},
		"array payload": {IngestInput{Type: "a.b", Payload: json.RawMessage(`[1]`)}, "payload"},
		"empty key":     {IngestInput{Type: "a.b", Payload: json.RawMessage(`{}`), IdempotencyKey: ptr("")}, "Idempotency-Key"},
		"long key":      {IngestInput{Type: "a.b", Payload: json.RawMessage(`{}`), IdempotencyKey: ptr(strings.Repeat("k", 256))}, "Idempotency-Key"},
	}
	for name, c := range cases {
		_, err := s.IngestEvent(ctx(), tn.ID, c.in)
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Field != c.field {
			t.Errorf("%s: err = %v, want a validation error on %q", name, err, c.field)
		}
	}
	if n := count(t, `SELECT count(*) FROM events WHERE tenant_id = $1`, tn.ID); n != 0 {
		t.Errorf("rejected input left %d events behind", n)
	}
}

func TestListEventsPagination(t *testing.T) {
	s := newService(t)
	tn := newTenant(t, s)
	for i := range 5 {
		if _, err := s.IngestEvent(ctx(), tn.ID, IngestInput{Type: "tick", Payload: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[uuid.UUID]bool{}
	cursor := ""
	for {
		res, err := s.ListEvents(ctx(), tn.ID, Page{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range res.Items {
			if seen[e.ID] {
				t.Fatalf("event %v listed twice", e.ID)
			}
			seen[e.ID] = true
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	if len(seen) != 5 {
		t.Errorf("listed %d events, want 5", len(seen))
	}
}

// deliveryFixture creates an endpoint and one delivery for it, then forces the
// delivery into the given state (nothing here runs a worker).
func deliveryFixture(t *testing.T, s *Service, tenant uuid.UUID, status string) (store.Endpoint, store.Delivery) {
	t.Helper()
	ep := newEndpoint(t, s, tenant)
	res, err := s.IngestEvent(ctx(), tenant, IngestInput{Type: "a.b", Payload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	var d store.Delivery
	rows, err := s.q.ListDeliveries(ctx(), store.ListDeliveriesParams{TenantID: tenant, EventID: &res.Event.ID, EndpointID: &ep.ID, PageSize: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("expected one delivery: %v (%d)", err, len(rows))
	}
	d = rows[0]
	switch status {
	case "pending":
	case "dead", "succeeded":
		if _, err := testPool.Exec(ctx(), `UPDATE deliveries SET status = $2, attempt_count = 3, completed_at = now(), last_error = 'earlier failure' WHERE id = $1`, d.ID, status); err != nil {
			t.Fatal(err)
		}
	case "in_flight":
		if _, err := testPool.Exec(ctx(), `UPDATE deliveries SET status = 'in_flight', locked_until = now() + interval '1 minute', locked_by = 'w', lease_token = gen_random_uuid() WHERE id = $1`, d.ID); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown status %q", status)
	}
	d, _ = s.q.GetDelivery(ctx(), store.GetDeliveryParams{ID: d.ID, TenantID: tenant})
	return ep, d
}

func TestListDeliveriesFiltersAndValidates(t *testing.T) {
	s := newService(t)
	a, b := newTenant(t, s), newTenant(t, s)
	deliveryFixture(t, s, b.ID, "dead") // another tenant's, must never show up

	// Two endpoints and two events: four deliveries, with controlled states.
	ep1, ep2 := newEndpoint(t, s, a.ID), newEndpoint(t, s, a.ID)
	var events []uuid.UUID
	for range 2 {
		res, err := s.IngestEvent(ctx(), a.ID, IngestInput{Type: "a.b", Payload: json.RawMessage(`{}`)})
		if err != nil || res.DeliveryCount != 2 {
			t.Fatalf("ingest: %v (%d deliveries)", err, res.DeliveryCount)
		}
		events = append(events, res.Event.ID)
	}
	// event 0 -> ep1 dead; everything else stays pending.
	if _, err := testPool.Exec(ctx(), `UPDATE deliveries SET status = 'dead', attempt_count = 1, completed_at = now()
		WHERE event_id = $1 AND endpoint_id = $2`, events[0], ep1.ID); err != nil {
		t.Fatal(err)
	}

	count := func(f DeliveryFilter) int {
		t.Helper()
		res, err := s.ListDeliveries(ctx(), a.ID, f, Page{})
		if err != nil {
			t.Fatal(err)
		}
		return len(res.Items)
	}
	if n := count(DeliveryFilter{}); n != 4 {
		t.Errorf("unfiltered = %d, want 4 (another tenant's delivery must not appear)", n)
	}
	if n := count(DeliveryFilter{Status: "dead"}); n != 1 {
		t.Errorf("status=dead: %d, want 1", n)
	}
	if n := count(DeliveryFilter{Status: "pending"}); n != 3 {
		t.Errorf("status=pending: %d, want 3", n)
	}
	if n := count(DeliveryFilter{EndpointID: &ep2.ID}); n != 2 {
		t.Errorf("by endpoint: %d, want 2", n)
	}
	if n := count(DeliveryFilter{EventID: &events[0]}); n != 2 {
		t.Errorf("by event: %d, want 2", n)
	}
	if n := count(DeliveryFilter{EndpointID: &ep1.ID, EventID: &events[0], Status: "dead"}); n != 1 {
		t.Errorf("all three filters: %d, want 1", n)
	}
	if n := count(DeliveryFilter{EndpointID: &ep2.ID, Status: "dead"}); n != 0 {
		t.Errorf("endpoint+status mismatch: %d, want 0", n)
	}
	_, err := s.ListDeliveries(ctx(), a.ID, DeliveryFilter{Status: "failed"}, Page{})
	wantField(t, err, "status")
}

func TestGetDeliveryReturnsAttemptsInOrder(t *testing.T) {
	s := newService(t)
	a, b := newTenant(t, s), newTenant(t, s)
	_, d := deliveryFixture(t, s, a.ID, "dead")
	for n := int32(3); n >= 1; n-- { // inserted out of order on purpose
		if _, err := s.q.CreateDeliveryAttempt(ctx(), store.CreateDeliveryAttemptParams{
			DeliveryID: d.ID, AttemptNo: n, StartedAt: time.Now(), DurationMs: 1, StatusCode: ptr(int32(500)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetDelivery(ctx(), a.ID, d.ID)
	if err != nil || len(got.Attempts) != 3 {
		t.Fatalf("GetDelivery: %d attempts, err=%v", len(got.Attempts), err)
	}
	for i, at := range got.Attempts {
		if at.AttemptNo != int32(i+1) {
			t.Errorf("attempt %d has number %d", i, at.AttemptNo)
		}
	}
	if _, err := s.GetDelivery(ctx(), b.ID, d.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another tenant reads the delivery: %v", err)
	}
}

func TestReplayDelivery(t *testing.T) {
	s := newService(t)
	a, b := newTenant(t, s), newTenant(t, s)

	for _, status := range []string{"dead", "succeeded"} {
		_, d := deliveryFixture(t, s, a.ID, status)
		got, err := s.ReplayDelivery(ctx(), a.ID, d.ID)
		if err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		if got.Status != "pending" || got.RoundBase != 3 || got.AttemptCount != 3 || got.CompletedAt != nil {
			t.Errorf("%s replay: %+v, want pending, round_base=3, attempt_count kept, completed_at cleared", status, got)
		}
		if time.Since(got.NextAttemptAt) < -time.Second {
			t.Errorf("replayed delivery is not due now: %v", got.NextAttemptAt)
		}
	}

	// Not finished yet: conflict, nothing changes.
	for _, status := range []string{"pending", "in_flight"} {
		_, d := deliveryFixture(t, s, a.ID, status)
		_, err := s.ReplayDelivery(ctx(), a.ID, d.ID)
		var ce *ConflictError
		if !errors.As(err, &ce) || ce.Code != ConflictNotReplayable {
			t.Errorf("%s: err = %v, want %s", status, err, ConflictNotReplayable)
		}
		if after, _ := s.q.GetDelivery(ctx(), store.GetDeliveryParams{ID: d.ID, TenantID: a.ID}); after.Status != status || after.RoundBase != 0 {
			t.Errorf("a refused replay changed the %s delivery: %+v", status, after)
		}
	}

	// A disabled endpoint would only abandon it again.
	ep, d := deliveryFixture(t, s, a.ID, "dead")
	if _, err := s.DisableEndpoint(ctx(), a.ID, ep.ID); err != nil {
		t.Fatal(err)
	}
	var ce *ConflictError
	if _, err := s.ReplayDelivery(ctx(), a.ID, d.ID); !errors.As(err, &ce) || ce.Code != ConflictEndpointDisabled {
		t.Errorf("disabled endpoint: err = %v, want %s", err, ConflictEndpointDisabled)
	}
	if _, err := s.EnableEndpoint(ctx(), a.ID, ep.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplayDelivery(ctx(), a.ID, d.ID); err != nil {
		t.Errorf("after enabling the endpoint: %v", err)
	}

	// Another tenant, and ids that do not exist.
	_, other := deliveryFixture(t, s, a.ID, "dead")
	if _, err := s.ReplayDelivery(ctx(), b.ID, other.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("replay by another tenant: %v, want ErrNotFound", err)
	}
	if after, _ := s.q.GetDelivery(ctx(), store.GetDeliveryParams{ID: other.ID, TenantID: a.ID}); after.Status != "dead" {
		t.Error("another tenant's replay attempt changed the delivery")
	}
	if _, err := s.ReplayDelivery(ctx(), a.ID, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown delivery: %v", err)
	}
}

// Two clients pressing "replay" at once must queue the delivery once.
func TestConcurrentReplaysQueueOneRound(t *testing.T) {
	s := newService(t)
	a := newTenant(t, s)
	_, d := deliveryFixture(t, s, a.ID, "dead")

	const clients = 16
	var (
		wg       sync.WaitGroup
		start    = make(chan struct{})
		mu       sync.Mutex
		accepted int
		refused  int
		other    []error
	)
	for range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.ReplayDelivery(ctx(), a.ID, d.ID)
			var ce *ConflictError
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				accepted++
			case errors.As(err, &ce) && ce.Code == ConflictNotReplayable:
				refused++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if accepted != 1 || refused != clients-1 || len(other) != 0 {
		t.Errorf("accepted=%d refused=%d other=%v, want 1, %d and none", accepted, refused, other, clients-1)
	}
}

func TestDisableAndEnableEndpoint(t *testing.T) {
	s := newService(t)
	a, b := newTenant(t, s), newTenant(t, s)
	ep := newEndpoint(t, s, a.ID)
	if _, err := testPool.Exec(ctx(), `UPDATE endpoints SET consecutive_failures = 7 WHERE id = $1`, ep.ID); err != nil {
		t.Fatal(err)
	}

	off, err := s.DisableEndpoint(ctx(), a.ID, ep.ID)
	if err != nil || off.Status != "disabled" || off.DisabledAt == nil || off.DisabledReason == nil || *off.DisabledReason != "disabled manually" {
		t.Fatalf("DisableEndpoint: %+v err=%v", off, err)
	}
	// Idempotent: the first disable's timestamp and reason stay.
	again, err := s.DisableEndpoint(ctx(), a.ID, ep.ID)
	if err != nil || !again.DisabledAt.Equal(*off.DisabledAt) {
		t.Errorf("second disable: %+v err=%v", again, err)
	}
	// A disabled endpoint receives no new deliveries from fan-out.
	res, err := s.IngestEvent(ctx(), a.ID, IngestInput{Type: "a.b", Payload: json.RawMessage(`{}`)})
	if err != nil || res.DeliveryCount != 0 {
		t.Errorf("fan-out to a disabled endpoint: %d deliveries, err=%v", res.DeliveryCount, err)
	}

	on, err := s.EnableEndpoint(ctx(), a.ID, ep.ID)
	if err != nil || on.Status != "active" || on.DisabledAt != nil || on.DisabledReason != nil || on.ConsecutiveFailures != 0 {
		t.Errorf("EnableEndpoint: %+v err=%v, want active with the failure counter reset", on, err)
	}

	if _, err := s.DisableEndpoint(ctx(), b.ID, ep.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("disable by another tenant: %v", err)
	}
	if _, err := s.EnableEndpoint(ctx(), b.ID, ep.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("enable by another tenant: %v", err)
	}
	if got, _ := s.GetEndpoint(ctx(), a.ID, ep.ID); got.Status != "active" {
		t.Error("another tenant changed the endpoint's state")
	}
}

func ptr[T any](v T) *T { return &v }
