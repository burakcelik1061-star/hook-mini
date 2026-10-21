-- +goose Up
-- Replay support. A delivery that ended (dead or succeeded) can be sent again
-- on request. Its retry budget must start over, but attempt_count must keep
-- equalling the number of rows in delivery_attempts (the history is never
-- rewritten). round_base is the attempt_count when the current round began:
-- attempts used in this round = attempt_count - round_base.
ALTER TABLE deliveries
    ADD COLUMN round_base integer NOT NULL DEFAULT 0,
    ADD CONSTRAINT deliveries_round_base_check CHECK (round_base >= 0 AND round_base <= attempt_count);

-- +goose Down
ALTER TABLE deliveries
    DROP CONSTRAINT deliveries_round_base_check,
    DROP COLUMN round_base;
