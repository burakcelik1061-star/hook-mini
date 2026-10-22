package store

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This file is an internal test (package store, not store_test) so it can
// EXPLAIN the exact SQL text sqlc generated.

// TestClaimLocksOnce guards against a Postgres planner trap. A
// "LIMIT n FOR UPDATE SKIP LOCKED" selection placed as a subquery in an
// UPDATE's FROM list can land on the inner side of a nested loop and run once
// per outer row; every run skips the rows already locked and locks new ones,
// so the statement claims more than n rows. Both worker queries therefore
// select inside a MATERIALIZED CTE. This test runs them on a small data set
// (where the planner favours nested loops) and requires every LockRows node
// to have executed exactly once.
func TestClaimLocksOnce(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	// One tenant, three endpoints, one event: three deliveries, two of them
	// pending and one in flight with an expired lease.
	seed := `
WITH t AS (INSERT INTO tenants (name) VALUES ('plan test') RETURNING id),
     ep AS (INSERT INTO endpoints (tenant_id, url, secret)
            SELECT t.id, 'https://example.invalid/' || g, repeat('s', 30) FROM t, generate_series(1, 3) g
            RETURNING id, tenant_id),
     ev AS (INSERT INTO events (tenant_id, type, payload) SELECT id, 'a.b', '{}' FROM t RETURNING id, tenant_id)
INSERT INTO deliveries (tenant_id, event_id, endpoint_id) SELECT ev.tenant_id, ev.id, ep.id FROM ev, ep`
	if _, err := tx.Exec(ctx, seed); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE deliveries SET status = 'in_flight', locked_until = now() - interval '1 minute',
       locked_by = 'gone', lease_token = gen_random_uuid()
WHERE id = (SELECT id FROM deliveries WHERE tenant_id = (SELECT id FROM tenants WHERE name = 'plan test') LIMIT 1)`); err != nil {
		t.Fatal(err)
	}

	// Arguments in the order the generated methods pass them (see
	// worker.sql.go); a mismatch fails loudly with a type error.
	for name, q := range map[string]struct {
		sql  string
		args []any
	}{
		"ClaimDueDeliveries":   {claimDueDeliveries, []any{int64(1000), "w", []uuid.UUID{}, int32(100), int64(1), int32(1)}}, // lease_ms, worker_id, busy_endpoints, scan_size, per_endpoint, batch_size
		"ReclaimExpiredLeases": {reclaimExpiredLeases, []any{int32(5), "test", int32(1)}},                                    // max_attempts, reason, batch_size
	} {
		t.Run(name, func(t *testing.T) {
			sp, err := tx.Begin(ctx) // savepoint: EXPLAIN ANALYZE really runs the UPDATE
			if err != nil {
				t.Fatal(err)
			}
			defer sp.Rollback(ctx)

			var raw []byte
			if err := sp.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+q.sql, q.args...).Scan(&raw); err != nil {
				t.Fatalf("explain: %v", err)
			}
			var plans []struct {
				Plan planNode `json:"Plan"`
			}
			if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
				t.Fatalf("unexpected EXPLAIN output: %v", err)
			}

			found := 0
			plans[0].Plan.walk(func(n planNode) {
				if n.Type == "LockRows" {
					found++
					if n.Loops != 1 {
						t.Errorf("the locking selection ran %v times in one statement; it must run once", n.Loops)
					}
				}
			})
			if found == 0 {
				t.Fatal("no LockRows node in the plan: the query no longer locks the rows it claims")
			}
		})
	}
}

type planNode struct {
	Type      string     `json:"Node Type"`
	Relation  string     `json:"Relation Name"`
	Index     string     `json:"Index Name"`
	IndexCond string     `json:"Index Cond"`
	Loops     float64    `json:"Actual Loops"`
	Plans     []planNode `json:"Plans"`
}

// TestStatsQueriesUseBoundedIndexes keeps the console's statistics cheap for
// a tenant with a long history: GET /v1/stats is polled every few seconds by
// every open console. The 24-hour figures must reach deliveries through an
// index bounded by created_at, and the backlog through the partial indexes
// of unfinished deliveries, never by walking every delivery of the tenant.
func TestStatsQueriesUseBoundedIndexes(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// With sequential scans off, a table scan only appears when no index can
	// serve the query at all; the choice between indexes is what is tested.
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}

	tenant := uuid.New()
	for _, q := range []struct {
		name, sql string
		// check judges every index scan on deliveries.
		check func(n planNode) string
		// want lists indexes on deliveries the plan must use.
		want []string
	}{
		{"DeliveryBacklog", deliveryBacklog, func(n planNode) string {
			if n.Index != "deliveries_due_idx" && n.Index != "deliveries_lease_idx" {
				return "reads " + n.Index + ", not only the unfinished deliveries"
			}
			return ""
		}, []string{"deliveries_due_idx", "deliveries_lease_idx"}},
		{"DeliveryCountsLast24h", deliveryCountsLast24h, boundedByCreatedAt, nil},
		{"EndpointStatsLast24h", endpointStatsLast24h, boundedByCreatedAt, nil},
	} {
		t.Run(q.name, func(t *testing.T) {
			var raw []byte
			if err := tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+q.sql, tenant).Scan(&raw); err != nil {
				t.Fatalf("explain: %v", err)
			}
			var plans []struct {
				Plan planNode `json:"Plan"`
			}
			if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
				t.Fatalf("unexpected EXPLAIN output: %v", err)
			}
			used := map[string]bool{}
			plans[0].Plan.walk(func(n planNode) {
				if n.Type == "Seq Scan" && n.Relation == "deliveries" {
					t.Errorf("scans the whole deliveries table")
				}
				if !strings.HasPrefix(n.Index, "deliveries_") {
					return
				}
				used[n.Index] = true
				if problem := q.check(n); problem != "" {
					t.Errorf("%s (%s, condition %q)", problem, n.Type, n.IndexCond)
				}
			})
			if len(used) == 0 {
				t.Error("does not read deliveries through any index")
			}
			for _, idx := range q.want {
				if !used[idx] {
					t.Errorf("does not use %s", idx)
				}
			}
		})
	}
}

// boundedByCreatedAt accepts an index scan on deliveries only if its
// condition limits created_at, which is what keeps it to the last 24 hours.
func boundedByCreatedAt(n planNode) string {
	if !strings.Contains(n.IndexCond, "created_at >=") {
		return "reads " + n.Index + " without a created_at bound"
	}
	return ""
}

func (n planNode) walk(fn func(planNode)) {
	fn(n)
	for _, c := range n.Plans {
		c.walk(fn)
	}
}
