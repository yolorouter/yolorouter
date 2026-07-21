-- migrations/sqlite/00006_create_api_keys.sql
--
-- API Key management (PRD §6.4 / design doc §3). The plaintext key is shown
-- only at create time; the row stores just key_hash + key_prefix. Each key's
-- model allowlist lives in api_key_models. Limit columns are NULL = no cap;
-- budget is integer micros (major-unit x 1e6, i.e. 6-decimal precision) to
-- avoid float drift on a cumulative hard cap.
--
-- +goose Up
CREATE TABLE api_keys (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    key_hash             TEXT NOT NULL UNIQUE,
    key_prefix           TEXT NOT NULL,
    owner_label          TEXT NOT NULL DEFAULT '',
    remark               TEXT NOT NULL DEFAULT '',
    status               SMALLINT NOT NULL DEFAULT 1,
    expires_at           DATETIME NULL,
    rpm_limit            INTEGER NULL,
    tpm_limit            INTEGER NULL,
    concurrency_limit    INTEGER NULL,
    budget_limit_micros   INTEGER NULL,
    budget_spent_micros   INTEGER NOT NULL DEFAULT 0,
    created_at           DATETIME NOT NULL,
    revoked_at           DATETIME NULL,
    updated_at           DATETIME NOT NULL
);

CREATE TABLE api_key_models (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    api_key_id  INTEGER NOT NULL REFERENCES api_keys(id),
    model_id    INTEGER NOT NULL REFERENCES models(id),
    created_at  DATETIME NOT NULL,
    UNIQUE(api_key_id, model_id)
);

CREATE INDEX idx_api_keys_owner_label ON api_keys (owner_label);
CREATE INDEX idx_api_key_models_api_key_id ON api_key_models (api_key_id);

-- +goose Down
DROP TABLE IF EXISTS api_key_models;
DROP TABLE IF EXISTS api_keys;
