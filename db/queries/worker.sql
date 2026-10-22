-- Queries used only by the delivery worker.
--
-- Time is always taken from the database (now()), never from the worker's
-- clock: the claim query compares next_attempt_at with now(), so writing it
-- from a skewed worker clock would make retries fire early or late.
--
-- Parameters are written sqlc.arg(name), not the @name shorthand: in Postgres @
-- is also the absolute-value operator, and in arithmetic such as
-- `now() + @x::bigint * interval '1 ms'` sqlc misparses the shorthand.
--
-- Invariant kept by these queries: attempt_count always equals the number of
-- delivery_attempts rows for the delivery.

-- name: ClaimDueDeliveries :many
-- Atomically takes up to batch_size due deliveries and leases them to one
-- worker. SKIP LOCKED makes concurrent workers pass over rows another worker
-- is claiming instead of waiting for them, so no row is ever handed out twice.
-- attempt_count is not changed here; it advances when the attempt is recorded
-- (or when an expired lease is reclaimed). The event and endpoint are joined
-- in so the worker needs no second round trip.
--
-- The locking SELECT must be a MATERIALIZED CTE, not a subquery in FROM: as a
-- subquery the planner may put it on the inner side of a nested loop and run
-- it once per outer row. Each run skips the rows it already locked and locks
-- new ones, so the claim could return more than batch_size rows. A
-- materialized CTE runs exactly once (see TestClaimLocksOnce).
--
-- Per-endpoint fairness: without it one slow endpoint can take every worker
-- slot and hold up all the others. The worker passes the endpoints that
-- already have its per-endpoint maximum in flight (busy_endpoints), which are
-- skipped, and per_endpoint, the most one claim takes for any one endpoint.
-- Postgres does not allow a window function in a query that locks rows, so
-- the earliest due deliveries are locked and numbered per endpoint first
-- (candidates, at most scan_size rows) and the ones to take are picked in a
-- second step. The candidates must be locked (SKIP LOCKED) already: unlocked,
-- every concurrent claimer would pick the same first rows, all but one would
-- find them taken and come back empty (TestClaimUnderHeavyContention). Locks
-- on candidates that are not taken end with the statement. The status and
-- due time are checked again on the locked row.
WITH candidates AS MATERIALIZED (
    SELECT id, next_attempt_at,
           row_number() OVER (PARTITION BY endpoint_id ORDER BY next_attempt_at, id) AS nth
    FROM (
        SELECT id, endpoint_id, next_attempt_at
        FROM deliveries
        WHERE status = 'pending' AND next_attempt_at <= now()
          AND endpoint_id <> ALL (sqlc.arg(busy_endpoints)::uuid[])
        ORDER BY next_attempt_at
        LIMIT sqlc.arg(scan_size)
        FOR UPDATE SKIP LOCKED
    ) earliest
),
due AS MATERIALIZED (
    SELECT d.id
    FROM deliveries d, candidates c
    WHERE c.id = d.id
      AND c.nth <= sqlc.arg(per_endpoint)::bigint
      AND d.status = 'pending' AND d.next_attempt_at <= now()
    ORDER BY c.next_attempt_at
    LIMIT sqlc.arg(batch_size)
    FOR UPDATE OF d SKIP LOCKED
)
UPDATE deliveries d
SET status       = 'in_flight',
    locked_until = now() + sqlc.arg(lease_ms)::bigint * interval '1 millisecond',
    locked_by    = sqlc.arg(worker_id)::text,
    lease_token  = gen_random_uuid()
FROM due, events e, endpoints ep
WHERE d.id = due.id
  AND d.status = 'pending'
  AND e.id = d.event_id
  AND ep.id = d.endpoint_id
RETURNING d.id,
          d.tenant_id,
          d.event_id,
          d.endpoint_id,
          d.attempt_count,
          d.round_base,
          d.lease_token::uuid AS lease_token,
          e.type              AS event_type,
          e.payload           AS event_payload,
          e.created_at        AS event_created_at,
          ep.url              AS endpoint_url,
          ep.secret           AS endpoint_secret,
          -- During a rotation's grace period requests carry a second
          -- signature made with the previous secret. Whether the grace
          -- period is still running is decided by the database clock.
          ep.previous_secret  AS endpoint_previous_secret,
          COALESCE(ep.previous_secret_expires_at > now(), false)::boolean AS previous_secret_valid,
          ep.status           AS endpoint_status;

-- The three outcomes of an attempt. Each is fenced: it changes the row only
-- if it is still in flight under the caller's lease_token, and reports how
-- many rows it changed. 0 means the lease was lost (expired and reclaimed,
-- or the delivery was deleted) and the caller must discard its result.

-- name: MarkDeliverySucceeded :execrows
UPDATE deliveries
SET status           = 'succeeded',
    attempt_count    = attempt_count + 1,
    completed_at     = now(),
    locked_until     = NULL,
    locked_by        = NULL,
    lease_token      = NULL,
    last_status_code = sqlc.arg(status_code)::int,
    last_error       = NULL
WHERE id = sqlc.arg(id) AND lease_token = sqlc.arg(lease_token)::uuid AND status = 'in_flight';

-- name: ScheduleDeliveryRetry :execrows
UPDATE deliveries
SET status           = 'pending',
    attempt_count    = attempt_count + 1,
    next_attempt_at  = now() + sqlc.arg(delay_ms)::bigint * interval '1 millisecond',
    locked_until     = NULL,
    locked_by        = NULL,
    lease_token      = NULL,
    last_status_code = sqlc.narg(status_code)::int,
    last_error       = sqlc.arg(last_error)::text
WHERE id = sqlc.arg(id) AND lease_token = sqlc.arg(lease_token)::uuid AND status = 'in_flight';

-- name: MarkDeliveryDead :execrows
-- attempts_used is 1 when an HTTP attempt was made and 0 when the delivery is
-- abandoned without one (for example because its endpoint is disabled).
UPDATE deliveries
SET status           = 'dead',
    attempt_count    = attempt_count + sqlc.arg(attempts_used)::int,
    completed_at     = now(),
    locked_until     = NULL,
    locked_by        = NULL,
    lease_token      = NULL,
    last_status_code = sqlc.narg(status_code)::int,
    last_error       = sqlc.arg(last_error)::text
WHERE id = sqlc.arg(id) AND lease_token = sqlc.arg(lease_token)::uuid AND status = 'in_flight';

-- name: ReturnInterruptedDelivery :execrows
-- Hands back a delivery whose attempt the worker itself cut short while
-- shutting down. The attempt is recorded (the request may have reached the
-- endpoint) but is not held against the delivery: round_base moves along
-- with attempt_count, so the retry budget is unchanged, and it is due again
-- at once.
UPDATE deliveries
SET status           = 'pending',
    attempt_count    = attempt_count + 1,
    round_base       = round_base + 1,
    next_attempt_at  = now(),
    locked_until     = NULL,
    locked_by        = NULL,
    lease_token      = NULL,
    last_status_code = NULL,
    last_error       = sqlc.arg(last_error)::text
WHERE id = sqlc.arg(id) AND lease_token = sqlc.arg(lease_token)::uuid AND status = 'in_flight';

-- name: PostponeDelivery :execrows
-- Hands back a claimed delivery without attempting it, due again after
-- delay_ms. For when this worker cannot sign the request because of its own
-- configuration (the endpoint's secret is sealed with an encryption key it
-- does not have yet, as during a key change): nothing was sent, so no attempt
-- is recorded and no retry is used up, and the delivery must not die for a
-- problem a configuration change fixes.
UPDATE deliveries
SET status          = 'pending',
    next_attempt_at = now() + sqlc.arg(delay_ms)::bigint * interval '1 millisecond',
    locked_until    = NULL,
    locked_by       = NULL,
    lease_token     = NULL,
    last_error      = sqlc.arg(last_error)::text
WHERE id = sqlc.arg(id) AND lease_token = sqlc.arg(lease_token)::uuid AND status = 'in_flight';

-- name: ReclaimExpiredLeases :many
-- Returns deliveries whose worker vanished (crashed, was killed, lost its
-- network) to the queue. The lost attempt counts: the request may well have
-- reached the endpoint, and counting it is what stops a delivery that crashes
-- every worker from being retried forever. Run it in a transaction together
-- with RecordLostAttempts, which writes the matching attempt rows. The locking
-- SELECT is a MATERIALIZED CTE for the same reason as in ClaimDueDeliveries.
WITH expired AS MATERIALIZED (
    SELECT id, locked_by
    FROM deliveries
    WHERE status = 'in_flight' AND locked_until < now()
    ORDER BY locked_until
    LIMIT sqlc.arg(batch_size)
    FOR UPDATE SKIP LOCKED
)
UPDATE deliveries d
SET attempt_count    = d.attempt_count + 1,
    status           = CASE WHEN d.attempt_count + 1 - d.round_base >= sqlc.arg(max_attempts)::int THEN 'dead' ELSE 'pending' END,
    completed_at     = CASE WHEN d.attempt_count + 1 - d.round_base >= sqlc.arg(max_attempts)::int THEN now() END,
    next_attempt_at  = now(),
    locked_until     = NULL,
    locked_by        = NULL,
    lease_token      = NULL,
    last_status_code = NULL,
    last_error       = sqlc.arg(reason)::text
FROM expired
WHERE d.id = expired.id AND d.status = 'in_flight'
RETURNING d.id, d.attempt_count, d.status, expired.locked_by AS previous_worker;

-- name: RecordLostAttempts :exec
-- One attempt row per reclaimed delivery, numbered with its new attempt_count.
INSERT INTO delivery_attempts (delivery_id, attempt_no, started_at, duration_ms, error)
SELECT unnest(sqlc.arg(delivery_ids)::uuid[]), unnest(sqlc.arg(attempt_nos)::int[]), now(), 0, sqlc.arg(reason)::text;
