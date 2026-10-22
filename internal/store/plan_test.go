package store

import (
	"context"
	"encoding/json"
	"os"
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
	Type  string     `json:"Node Type"`
	Loops float64    `json:"Actual Loops"`
	Plans []planNode `json:"Plans"`
}

func (n planNode) walk(fn func(planNode)) {
	fn(n)
	for _, c := range n.Plans {
		c.walk(fn)
	}
}
