-- +goose Up
-- Baseline: shared helpers every later table relies on. Phase 0 deliberately
-- creates no domain tables; the schema arrives in Phase 1.

-- +goose StatementBegin
CREATE FUNCTION set_updated_at() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION set_updated_at();
