-- name: CreateTenant :one
INSERT INTO tenants (name)
VALUES ($1)
RETURNING *;

-- name: GetTenant :one
SELECT * FROM tenants WHERE id = $1;

-- name: DeleteTenant :execrows
-- Cascades to keys, endpoints, events, deliveries and attempts.
DELETE FROM tenants WHERE id = $1;
