-- Tenant statistics for the console's overview. Each query is scoped to one
-- tenant and bounded: the 24-hour figures use deliveries_tenant_idx and
-- deliveries_endpoint_idx with a created_at range, the backlog reads only the
-- unfinished deliveries (see TestStatsQueriesUseBoundedIndexes).

-- name: DeliveryCountsLast24h :many
-- Deliveries created in the last 24 hours, by their status now.
SELECT status, count(*)::bigint AS deliveries
FROM deliveries
WHERE tenant_id = $1 AND created_at >= now() - interval '24 hours'
GROUP BY status;

-- name: DeliveryBacklog :one
-- Unfinished deliveries, however old. One count per status, so each can use
-- that status's partial index (deliveries_due_idx, deliveries_lease_idx):
-- "status IN ('pending', 'in_flight')" implies neither index's predicate, and
-- the planner would walk the tenant's whole history instead (every delivery
-- of the retention period) to find the few that are unfinished. The partial
-- indexes hold every tenant's unfinished deliveries, which is the queue and
-- stays small while the workers keep up.
SELECT (SELECT count(*) FROM deliveries p WHERE p.status = 'pending' AND p.tenant_id = $1)::bigint     AS pending,
       (SELECT count(*) FROM deliveries f WHERE f.status = 'in_flight' AND f.tenant_id = $1)::bigint AS in_flight;

-- name: EndpointStatsLast24h :many
-- Every endpoint of the tenant with its deliveries of the last 24 hours by
-- status (zeros for an endpoint without any), oldest endpoint first.
SELECT ep.id, ep.url, ep.status, ep.consecutive_failures,
       count(d.id) FILTER (WHERE d.status = 'succeeded')::bigint AS succeeded,
       count(d.id) FILTER (WHERE d.status = 'dead')::bigint      AS dead,
       count(d.id) FILTER (WHERE d.status = 'pending')::bigint   AS pending,
       count(d.id) FILTER (WHERE d.status = 'in_flight')::bigint AS in_flight
FROM endpoints ep
LEFT JOIN deliveries d
       ON d.endpoint_id = ep.id AND d.tenant_id = ep.tenant_id
      AND d.created_at >= now() - interval '24 hours'
WHERE ep.tenant_id = $1
GROUP BY ep.id
ORDER BY ep.created_at, ep.id;
