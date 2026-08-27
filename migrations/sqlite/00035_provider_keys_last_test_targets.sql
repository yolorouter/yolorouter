-- migrations/sqlite/00035_provider_keys_last_test_targets.sql
--
-- provider_keys.last_test_targets keeps the per-protocol breakdown of the
-- most recent verification run: a JSON array with one entry per destination
-- the run probed (protocol id, outcome, duration, and the upstream's own
-- diagnostic text).
--
-- The aggregate columns next to it (verification_status, last_test_result)
-- only ever describe the WORST destination, which is why a provider whose
-- primary protocol is healthy but whose extra endpoint rejects the
-- credential looks, from those columns alone, like a bad key. This column
-- carries the part that was being thrown away, so the admin UI can name the
-- protocol that failed and show what the upstream said.
--
-- NULL means "no breakdown recorded": rows last tested before this column
-- existed, and rows never tested at all. History is not backfilled — the
-- breakdown only exists for runs that happened after the upgrade.

-- +goose Up
ALTER TABLE provider_keys ADD COLUMN last_test_targets TEXT;

-- +goose Down
ALTER TABLE provider_keys DROP COLUMN last_test_targets;
