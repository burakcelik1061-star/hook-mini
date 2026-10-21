-- name: CreateEvent :one
-- Idempotent insert. When (tenant_id, idempotency_key) already exists nothing
-- is inserted and no row comes back (sql.ErrNoRows / pgx.ErrNoRows); the caller
-- then loads the original with GetEventByIdempotencyKey, inside the same
-- transaction, and can compare the payloads to reject a conflicting replay.
INSERT INTO events (tenant_id, type, payload, idempotency_key)
VALUES ($1, $2, $3, $4)
ON CONFLICT (tenant_id, idempotency_key) DO NOTHING
RETURNING *;

-- name: GetEventForReplay :one
-- Loads the event an idempotency key already points at and says whether the
-- replayed request carries the same content. The comparison is done by
-- Postgres on the normalised jsonb, so key order and whitespace in the replay
-- do not matter.
SELECT e.*,
       COALESCE(e.type = @type AND e.payload = @payload::jsonb, false)::boolean AS matches
FROM events e
WHERE e.tenant_id = @tenant_id AND e.idempotency_key = @idempotency_key;

-- name: GetEventByIdempotencyKey :one
SELECT * FROM events
WHERE tenant_id = $1 AND idempotency_key = $2;

-- name: GetEvent :one
SELECT * FROM events
WHERE id = $1 AND tenant_id = $2;

-- name: ListEvents :many
SELECT * FROM events
WHERE tenant_id = @tenant_id
  AND (
    sqlc.narg('after_created_at')::timestamptz IS NULL
    OR (created_at, id) < (sqlc.narg('after_created_at')::timestamptz, sqlc.narg('after_id')::uuid)
  )
ORDER BY created_at DESC, id DESC
LIMIT @page_size;
