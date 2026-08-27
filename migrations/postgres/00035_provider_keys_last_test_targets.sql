-- migrations/postgres/00035_provider_keys_last_test_targets.sql
--
-- Per-protocol breakdown of a provider key's most recent verification run.
-- See the sqlite twin for the column semantics (JSON array of one entry per
-- probed destination, NULL when no breakdown was recorded, never
-- backfilled).

-- +goose Up
ALTER TABLE provider_keys ADD COLUMN last_test_targets TEXT NULL;

-- +goose Down
ALTER TABLE provider_keys DROP COLUMN IF EXISTS last_test_targets;
