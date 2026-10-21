-- name: CreateAPIKey :one
INSERT INTO api_keys (tenant_id, name, prefix, key_hash)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetActiveAPIKeyByPrefix :one
-- Authentication path: look the key up by its public prefix, then the caller
-- compares key_hash in constant time. Revoked keys are never returned.
SELECT * FROM api_keys
WHERE prefix = $1 AND revoked_at IS NULL;

-- name: TouchAPIKey :exec
-- Throttled to one write per minute per key, so authenticating a request does
-- not turn into a write on a hot row.
UPDATE api_keys
SET last_used_at = now()
WHERE id = $1
  AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: ListAPIKeys :many
-- Deliberately omits key_hash: listing must never be able to expose it.
SELECT id, tenant_id, name, prefix, last_used_at, revoked_at, created_at
FROM api_keys
WHERE tenant_id = $1
ORDER BY created_at DESC, id DESC;

-- name: RevokeAPIKey :one
-- Returns no row if the key does not exist for this tenant or is already revoked.
UPDATE api_keys
SET revoked_at = now()
WHERE id = $1 AND tenant_id = $2 AND revoked_at IS NULL
RETURNING id;
