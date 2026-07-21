-- migrations/sqlite/00004_create_providers.sql
-- Provider management

-- +goose Up
CREATE TABLE providers (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    name                 VARCHAR(50) NOT NULL UNIQUE,
    provider_type        VARCHAR(20) NOT NULL DEFAULT 'openai',
    base_url             VARCHAR(255) NOT NULL,
    note                 VARCHAR(200) NULL,
    management_status    SMALLINT NOT NULL DEFAULT 1,
    -- Bumped atomically (same UPDATE as base_url) every time the address
    -- changes — the "destination version binding" invariant. A provider_keys row's
    -- authorized_destination_version must equal this value before its
    -- plaintext may be decrypted for any test or relay call.
    destination_version  INTEGER NOT NULL DEFAULT 1,
    created_at           DATETIME NOT NULL,
    updated_at           DATETIME NOT NULL
);

CREATE TABLE provider_keys (
    id                              INTEGER PRIMARY KEY AUTOINCREMENT,
    provider_id                     INTEGER NOT NULL REFERENCES providers(id),
    label                           VARCHAR(30) NOT NULL,
    encrypted_key                   TEXT NOT NULL,
    key_prefix                      VARCHAR(20) NOT NULL,
    sort_order                      INTEGER NOT NULL,
    -- The model name to send in every test call for this key (single-key
    -- test, batch test, and the server-side re-verify on create/edit).
    -- Admin-supplied since there is no real model mapping yet — the
    -- "enter a model name ad hoc to run a test" flow; required on every create/edit.
    test_model                      VARCHAR(100) NOT NULL,
    management_status               SMALLINT NOT NULL DEFAULT 1,
    verification_status             SMALLINT NOT NULL DEFAULT 0,
    authorized_destination_version  INTEGER NOT NULL,
    last_test_result                SMALLINT NULL,
    last_test_model                 VARCHAR(100) NULL,
    last_test_duration_ms           INTEGER NULL,
    last_tested_at                  DATETIME NULL,
    -- Bumped whenever a new plaintext is submitted (create / edit-with-new-key
    -- / re-entry) — part of the write-back CAS.
    config_version                  INTEGER NOT NULL DEFAULT 1,
    -- Claimed (+1) at the start of every test attempt so an
    -- earlier-started-but-later-finishing test can't overwrite a
    -- later-started-but-earlier-finishing one's result.
    test_generation                 INTEGER NOT NULL DEFAULT 0,
    created_at                      DATETIME NOT NULL,
    updated_at                      DATETIME NOT NULL,
    UNIQUE(provider_id, label),
    UNIQUE(provider_id, sort_order)
);

CREATE INDEX idx_provider_keys_provider_id ON provider_keys(provider_id);

-- Single-row table: a fixed known plaintext encrypted with the current
-- provider_master_key, checked at startup so a mismatched master key
-- (e.g. a DB-only backup restore without its matching config.yaml) fails
-- loudly at boot instead of silently making every encrypted_key
-- undecryptable.
CREATE TABLE provider_key_fingerprint (
    id              INTEGER PRIMARY KEY,
    encrypted_probe TEXT NOT NULL,
    created_at      DATETIME NOT NULL
);

-- +goose Down
DROP TABLE provider_key_fingerprint;
DROP TABLE provider_keys;
DROP TABLE providers;
