-- migrations/postgres/00004_create_providers.sql
-- Mirrors migrations/sqlite/00004_create_providers.sql for the same reasons.

-- +goose Up
CREATE TABLE providers (
    id                   BIGSERIAL PRIMARY KEY,
    name                 VARCHAR(50) NOT NULL UNIQUE,
    provider_type        VARCHAR(20) NOT NULL DEFAULT 'openai',
    base_url             VARCHAR(255) NOT NULL,
    note                 VARCHAR(200) NULL,
    management_status    SMALLINT NOT NULL DEFAULT 1,
    destination_version  INTEGER NOT NULL DEFAULT 1,
    created_at           TIMESTAMPTZ NOT NULL,
    updated_at           TIMESTAMPTZ NOT NULL
);

CREATE TABLE provider_keys (
    id                              BIGSERIAL PRIMARY KEY,
    provider_id                     BIGINT NOT NULL REFERENCES providers(id),
    label                           VARCHAR(30) NOT NULL,
    encrypted_key                   TEXT NOT NULL,
    key_prefix                      VARCHAR(20) NOT NULL,
    sort_order                      INTEGER NOT NULL,
    test_model                      VARCHAR(100) NOT NULL,
    management_status               SMALLINT NOT NULL DEFAULT 1,
    verification_status             SMALLINT NOT NULL DEFAULT 0,
    authorized_destination_version  INTEGER NOT NULL,
    last_test_result                SMALLINT NULL,
    last_test_model                 VARCHAR(100) NULL,
    last_test_duration_ms           INTEGER NULL,
    last_tested_at                  TIMESTAMPTZ NULL,
    config_version                  INTEGER NOT NULL DEFAULT 1,
    test_generation                 INTEGER NOT NULL DEFAULT 0,
    created_at                      TIMESTAMPTZ NOT NULL,
    updated_at                      TIMESTAMPTZ NOT NULL,
    UNIQUE(provider_id, label),
    UNIQUE(provider_id, sort_order)
);

CREATE INDEX idx_provider_keys_provider_id ON provider_keys(provider_id);

CREATE TABLE provider_key_fingerprint (
    id              SMALLINT PRIMARY KEY,
    encrypted_probe TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL
);

-- +goose Down
DROP TABLE provider_key_fingerprint;
DROP TABLE provider_keys;
DROP TABLE providers;
