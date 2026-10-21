-- +goose Up
-- Signing secret rotation. After a rotation the old secret stays valid until
-- previous_secret_expires_at: every request is signed with both secrets in
-- that window, so a receiver can switch to the new secret whenever it is
-- ready without rejecting a single delivery.
ALTER TABLE endpoints
    ADD COLUMN previous_secret            text CHECK (char_length(previous_secret) >= 24),
    ADD COLUMN previous_secret_expires_at timestamptz,
    ADD CONSTRAINT endpoints_previous_secret_consistent
        CHECK ((previous_secret IS NULL) = (previous_secret_expires_at IS NULL));

-- +goose Down
ALTER TABLE endpoints
    DROP CONSTRAINT endpoints_previous_secret_consistent,
    DROP COLUMN previous_secret_expires_at,
    DROP COLUMN previous_secret;
