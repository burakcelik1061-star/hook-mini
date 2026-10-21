-- +goose Up
-- Retention. The cleaner looks for finished deliveries by age and for events
-- by age; without these indexes every pass would scan the whole table.
-- The delivery index is partial, like the worker's: it holds only finished
-- rows and ignores the pending and in-flight ones the worker cares about.
CREATE INDEX deliveries_finished_idx ON deliveries (completed_at) WHERE status IN ('succeeded', 'dead');
CREATE INDEX events_created_idx ON events (created_at);

-- +goose Down
DROP INDEX events_created_idx;
DROP INDEX deliveries_finished_idx;
