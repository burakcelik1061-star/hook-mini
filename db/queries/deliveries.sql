-- Phase 1 covers creating and reading deliveries. The worker's queries (claim
-- with FOR UPDATE SKIP LOCKED, complete, schedule a retry, mark dead, reclaim
-- expired leases) are designed together with the worker in Phase 3.

-- name: CreateDeliveriesForEvent :many
-- Fan-out in a single statement: one pending delivery per active endpoint of
-- the tenant. Safe to repeat: (event_id, endpoint_id) is unique, so a second
-- call inserts nothing. The composite foreign key rejects an event that does
-- not belong to the tenant.
INSERT INTO deliveries (tenant_id, event_id, endpoint_id)
SELECT e.tenant_id, @event_id, e.id
FROM endpoints e
WHERE e.tenant_id = @tenant_id AND e.status = 'active'
ON CONFLICT (event_id, endpoint_id) DO NOTHING
RETURNING *;

-- name: CountDeliveriesForEvent :one
SELECT count(*) FROM deliveries
WHERE event_id = $1 AND tenant_id = $2;

-- name: GetDelivery :one
SELECT * FROM deliveries
WHERE id = $1 AND tenant_id = $2;

-- name: ListDeliveries :many
-- Optional filters by endpoint, event and status, newest first,
-- keyset-paginated.
SELECT * FROM deliveries
WHERE tenant_id = @tenant_id
  AND (sqlc.narg('endpoint_id')::uuid IS NULL OR endpoint_id = sqlc.narg('endpoint_id')::uuid)
  AND (sqlc.narg('event_id')::uuid IS NULL OR event_id = sqlc.narg('event_id')::uuid)
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text)
  AND (
    sqlc.narg('after_created_at')::timestamptz IS NULL
    OR (created_at, id) < (sqlc.narg('after_created_at')::timestamptz, sqlc.narg('after_id')::uuid)
  )
ORDER BY created_at DESC, id DESC
LIMIT @page_size;

-- name: ReplayDelivery :one
-- Sends a finished delivery again. Only dead or succeeded deliveries qualify;
-- the status guard is what makes two simultaneous replays safe (the second
-- finds the row pending and changes nothing). The retry budget starts over
-- (round_base), the attempt history is kept.
UPDATE deliveries
SET status          = 'pending',
    round_base      = attempt_count,
    next_attempt_at = now(),
    completed_at    = NULL
WHERE id = $1 AND tenant_id = $2 AND status IN ('dead', 'succeeded')
RETURNING *;

-- name: CreateDeliveryAttempt :one
INSERT INTO delivery_attempts (
    delivery_id, attempt_no, started_at, duration_ms, status_code, response_snippet, error
) VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ListDeliveryAttempts :many
-- Joined to deliveries so the tenant check cannot be forgotten.
SELECT a.*
FROM delivery_attempts a
JOIN deliveries d ON d.id = a.delivery_id
WHERE a.delivery_id = @delivery_id AND d.tenant_id = @tenant_id
ORDER BY a.attempt_no;
