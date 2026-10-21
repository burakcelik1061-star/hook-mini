-- +goose Up
-- Fencing tokens for delivery leases.
--
-- A worker that claims a delivery gets a fresh random lease_token. It may
-- record the result only while that token is still on the row. If its lease
-- expires and another worker reclaims the delivery, the token changes, so the
-- first worker's late result is rejected instead of overwriting the newer one.
ALTER TABLE deliveries ADD COLUMN lease_token uuid;

-- An in-flight delivery carries a full lease (expiry, owner, token); any other
-- state carries none of it.
ALTER TABLE deliveries DROP CONSTRAINT deliveries_lease_consistent;
ALTER TABLE deliveries ADD CONSTRAINT deliveries_lease_consistent CHECK (
    (status = 'in_flight') = (locked_until IS NOT NULL)
    AND (status = 'in_flight') = (locked_by IS NOT NULL)
    AND (status = 'in_flight') = (lease_token IS NOT NULL)
);

-- +goose Down
ALTER TABLE deliveries DROP CONSTRAINT deliveries_lease_consistent;
ALTER TABLE deliveries ADD CONSTRAINT deliveries_lease_consistent
    CHECK ((status = 'in_flight') = (locked_until IS NOT NULL));
ALTER TABLE deliveries DROP COLUMN lease_token;
