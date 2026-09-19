-- migrations/sqlite/00047_observed_rate_limits.sql
--
-- SQLite mirror of migrations/postgres/00047_observed_rate_limits.sql.
--
-- One row per (provider key, meter) of what that key's upstream has said
-- about its own rate limits when rejecting with a 429 — the numeric
-- ceiling, the window it applies over when the upstream named one, and
-- the freshest remaining/reset snapshot. Rows are learned evidence, not
-- configuration: limit_value only ever moves DOWN (a lone malformed
-- response must not raise the remembered ceiling), and an admin deleting
-- a row simply lets the next 429 teach it again. Deleting the key removes
-- its rows in the same transaction (no FK pragma assumptions here — the
-- delete path owns the cascade).

-- +goose Up
CREATE TABLE observed_rate_limits (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    provider_key_id INTEGER NOT NULL,
    meter           TEXT NOT NULL,
    limit_value     INTEGER NULL,
    window_seconds  INTEGER NULL,
    last_remaining  INTEGER NULL,
    last_reset_at   DATETIME NULL,
    observed_at     DATETIME NOT NULL,
    updated_at      DATETIME NOT NULL,
    UNIQUE (provider_key_id, meter)
);
CREATE INDEX idx_observed_rate_limits_provider_key_id ON observed_rate_limits(provider_key_id);

-- +goose Down
DROP INDEX IF EXISTS idx_observed_rate_limits_provider_key_id;
DROP TABLE IF EXISTS observed_rate_limits;
