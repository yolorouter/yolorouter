-- +goose Up
CREATE TABLE models (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    name                 VARCHAR(100) NOT NULL UNIQUE,
    management_status    SMALLINT NOT NULL DEFAULT 1,
    created_at           DATETIME NOT NULL,
    updated_at           DATETIME NOT NULL
);

CREATE TABLE model_candidates (
    id                          INTEGER PRIMARY KEY AUTOINCREMENT,
    model_id                    INTEGER NOT NULL REFERENCES models(id),
    provider_id                 INTEGER NOT NULL REFERENCES providers(id),
    provider_model_name         VARCHAR(200) NOT NULL,
    input_price                 NUMERIC NOT NULL DEFAULT 0,
    output_price                NUMERIC NOT NULL DEFAULT 0,
    cache_write_price           NUMERIC NULL,
    cache_read_price            NUMERIC NULL,
    max_output                  INTEGER NOT NULL DEFAULT 0,
    supports_streaming          BOOLEAN NOT NULL DEFAULT 0,
    supports_function_calling   BOOLEAN NOT NULL DEFAULT 0,
    management_status           SMALLINT NOT NULL DEFAULT 2,
    sort_order                  INTEGER NOT NULL,
    verification_status         SMALLINT NOT NULL DEFAULT 0,
    last_test_result             SMALLINT NULL,
    last_test_duration_ms        INTEGER NULL,
    last_tested_at               DATETIME NULL,
    created_at                   DATETIME NOT NULL,
    updated_at                   DATETIME NOT NULL,
    UNIQUE(model_id, provider_id),
    UNIQUE(model_id, sort_order)
);

-- +goose Down
DROP TABLE model_candidates;
DROP TABLE models;
