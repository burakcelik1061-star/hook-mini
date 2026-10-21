-- Retention: deleting what has outlived its usefulness, a batch at a time.
-- Both queries are safe to run from several workers at once (FOR UPDATE SKIP
-- LOCKED), and neither touches a delivery that is still pending or in flight.

-- name: DeleteExpiredDeliveries :execrows
-- Deletes up to batch_size finished deliveries (succeeded or dead) that ended
-- more than retention_ms ago; their attempts go with them (ON DELETE
-- CASCADE). The locking SELECT is a MATERIALIZED CTE for the same reason as in
-- ClaimDueDeliveries. A delivery that a replay has made pending again no
-- longer matches the status filter and stays; one a replay holds locked right
-- now is skipped, and a replay that arrives after the delete finds nothing
-- and is answered 404.
WITH expired AS MATERIALIZED (
    SELECT id
    FROM deliveries
    WHERE status IN ('succeeded', 'dead')
      AND completed_at < now() - sqlc.arg(retention_ms)::bigint * interval '1 millisecond'
    ORDER BY completed_at
    LIMIT sqlc.arg(batch_size)
    FOR UPDATE SKIP LOCKED
)
DELETE FROM deliveries d
USING expired
WHERE d.id = expired.id;

-- name: DeleteExpiredEvents :execrows
-- Deletes up to batch_size events older than retention_ms that no delivery
-- refers to any more (their deliveries were deleted, or there were none). An
-- event that still has a pending, retrying or recent delivery stays, whatever
-- its age. Deleting the event also deletes its idempotency key, so a key can
-- be used again once retention has passed.
WITH expired AS MATERIALIZED (
    SELECT e.id
    FROM events e
    WHERE e.created_at < now() - sqlc.arg(retention_ms)::bigint * interval '1 millisecond'
      AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.event_id = e.id)
    ORDER BY e.created_at
    LIMIT sqlc.arg(batch_size)
    FOR UPDATE OF e SKIP LOCKED
)
DELETE FROM events e
USING expired
WHERE e.id = expired.id
  AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.event_id = e.id);
