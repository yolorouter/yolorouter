--
-- The observed rate limits table: what upstreams have said about a key's
-- own rate limits when rejecting with a 429, one row per (provider key,
-- meter). Column semantics are shared with the sqlite twin — see its
-- header note for the learning/reset contract. limit_value is only-down
-- by the upsert itself (the CASE in the repository), not by a constraint:
-- the rule must survive concurrent 429s landing out of order, which a
-- check constraint cannot express.
--

-- +goose Up
CREATE TABLE observed_rate_limits (
    id              BIGSERIAL PRIMARY KEY,
    provider_key_id BIGINT NOT NULL,
    meter           TEXT NOT NULL,
    limit_value     BIGINT NULL,
    window_seconds  BIGINT NULL,
    last_remaining  BIGINT NULL,
    last_reset_at   TIMESTAMPTZ NULL,
    observed_at     TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    UNIQUE (provider_key_id, meter)
);
CREATE INDEX idx_observed_rate_limits_provider_key_id ON observed_rate_limits(provider_key_id);

-- +goose Down
DROP INDEX IF EXISTS idx_observed_rate_limits_provider_key_id;
DROP TABLE IF EXISTS observed_rate_limits;
