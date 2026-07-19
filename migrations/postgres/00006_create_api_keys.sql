-- migrations/postgres/00006_create_api_keys.sql
--
-- API Key management (PRD §6.4 / design doc §3). The plaintext key is shown
-- only at create time; the row stores just key_hash + key_prefix. Each key's
-- model allowlist lives in api_key_models. Limit columns are NULL = no cap;
-- budget is integer cents (bigint) to avoid float drift on a cumulative hard
-- cap.
--
-- +goose Up
CREATE TABLE api_keys (
    id                   BIGSERIAL PRIMARY KEY,
    key_hash             VARCHAR(255) NOT NULL UNIQUE,
    key_prefix           VARCHAR(32) NOT NULL,
    owner_label          VARCHAR(50) NOT NULL DEFAULT '',
    remark               VARCHAR(200) NOT NULL DEFAULT '',
    status               SMALLINT NOT NULL DEFAULT 1,
    expires_at           TIMESTAMPTZ NULL,
    rpm_limit            INTEGER NULL,
    tpm_limit            INTEGER NULL,
    concurrency_limit    INTEGER NULL,
    budget_limit_cents   BIGINT NULL,
    budget_spent_cents   BIGINT NOT NULL DEFAULT 0,
    created_at           TIMESTAMPTZ NOT NULL,
    revoked_at           TIMESTAMPTZ NULL,
    updated_at           TIMESTAMPTZ NOT NULL
);

CREATE TABLE api_key_models (
    id          BIGSERIAL PRIMARY KEY,
    api_key_id  BIGINT NOT NULL REFERENCES api_keys(id),
    model_id    BIGINT NOT NULL REFERENCES models(id),
    created_at  TIMESTAMPTZ NOT NULL,
    UNIQUE(api_key_id, model_id)
);

CREATE INDEX idx_api_keys_owner_label ON api_keys (owner_label);
CREATE INDEX idx_api_key_models_api_key_id ON api_key_models (api_key_id);

-- +goose Down
DROP TABLE IF EXISTS api_key_models;
DROP TABLE IF EXISTS api_keys;
