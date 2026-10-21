-- Every query that takes an endpoint id from outside also takes the tenant id.
-- That is what stops one tenant from reading or changing another's endpoint.

-- name: CreateEndpoint :one
-- The id is chosen by the caller: the secret is encrypted bound to it, so it
-- must be known before the row exists.
INSERT INTO endpoints (id, tenant_id, url, description, secret)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetEndpoint :one
SELECT * FROM endpoints
WHERE id = $1 AND tenant_id = $2;

-- name: ListEndpoints :many
-- Keyset pagination on (created_at, id): stable under concurrent inserts and
-- constant cost per page, unlike OFFSET.
SELECT * FROM endpoints
WHERE tenant_id = @tenant_id
  AND (
    sqlc.narg('after_created_at')::timestamptz IS NULL
    OR (created_at, id) < (sqlc.narg('after_created_at')::timestamptz, sqlc.narg('after_id')::uuid)
  )
ORDER BY created_at DESC, id DESC
LIMIT @page_size;

-- name: UpdateEndpoint :one
-- Only the fields that are passed change; NULL means "leave as is".
UPDATE endpoints
SET url         = COALESCE(sqlc.narg('url'), url),
    description = COALESCE(sqlc.narg('description'), description)
WHERE id = @id AND tenant_id = @tenant_id
RETURNING *;

-- name: DeleteEndpoint :execrows
DELETE FROM endpoints
WHERE id = $1 AND tenant_id = $2;

-- name: ListActiveEndpointIDs :many
-- Fan-out target list. Served by endpoints_active_idx.
SELECT id FROM endpoints
WHERE tenant_id = $1 AND status = 'active';

-- name: RecordEndpointSuccess :exec
UPDATE endpoints
SET consecutive_failures = 0
WHERE id = $1 AND consecutive_failures <> 0;

-- name: RecordEndpointFailure :one
-- An atomic increment, not read-modify-write: two workers failing at the same
-- moment must count as two failures, not one.
UPDATE endpoints
SET consecutive_failures = consecutive_failures + 1
WHERE id = $1
RETURNING consecutive_failures;

-- name: DisableEndpoint :execrows
-- Idempotent: only an active endpoint is changed, so the first reason sticks.
UPDATE endpoints
SET status = 'disabled', disabled_at = now(), disabled_reason = $2
WHERE id = $1 AND status = 'active';

-- name: RotateEndpointSecret :one
-- The current secret becomes the previous one, valid for grace_ms more; a
-- second rotation inside that window drops the oldest secret.
UPDATE endpoints
SET previous_secret            = secret,
    previous_secret_expires_at = now() + sqlc.arg(grace_ms)::bigint * interval '1 millisecond',
    secret                     = sqlc.arg(new_secret)::text
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id)
RETURNING *;

-- name: DisableEndpointForTenant :one
-- The tenant-scoped, manual counterpart of DisableEndpoint. Returns no row
-- when the endpoint is already disabled or does not exist for the tenant.
UPDATE endpoints
SET status = 'disabled', disabled_at = now(), disabled_reason = sqlc.arg(reason)::text
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id) AND status = 'active'
RETURNING *;

-- name: EnableEndpoint :one
UPDATE endpoints
SET status = 'active', disabled_at = NULL, disabled_reason = NULL, consecutive_failures = 0
WHERE id = $1 AND tenant_id = $2
RETURNING *;
