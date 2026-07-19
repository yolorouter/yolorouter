-- +goose Up
CREATE TABLE models (
    id                   BIGSERIAL PRIMARY KEY,
    name                 VARCHAR(100) NOT NULL UNIQUE,
    management_status    SMALLINT NOT NULL DEFAULT 1,
    created_at           TIMESTAMPTZ NOT NULL,
    updated_at           TIMESTAMPTZ NOT NULL
);

CREATE TABLE model_candidates (
    id                          BIGSERIAL PRIMARY KEY,
    model_id                    BIGINT NOT NULL REFERENCES models(id),
    provider_id                 BIGINT NOT NULL REFERENCES providers(id),
    provider_model_name         VARCHAR(200) NOT NULL,
    input_price                 NUMERIC(20,10) NOT NULL DEFAULT 0,
    output_price                NUMERIC(20,10) NOT NULL DEFAULT 0,
    cache_write_price           NUMERIC(20,10) NULL,
    cache_read_price            NUMERIC(20,10) NULL,
    max_output                  INTEGER NOT NULL DEFAULT 0,
    supports_streaming          BOOLEAN NOT NULL DEFAULT false,
    supports_function_calling   BOOLEAN NOT NULL DEFAULT false,
    management_status           SMALLINT NOT NULL DEFAULT 2,
    sort_order                  INTEGER NOT NULL,
    verification_status         SMALLINT NOT NULL DEFAULT 0,
    last_test_result             SMALLINT NULL,
    last_test_duration_ms        INTEGER NULL,
    last_tested_at               TIMESTAMPTZ NULL,
    created_at                   TIMESTAMPTZ NOT NULL,
    updated_at                   TIMESTAMPTZ NOT NULL,
    UNIQUE(model_id, provider_id),
    UNIQUE(model_id, sort_order)
);

-- +goose Down
DROP TABLE model_candidates;
DROP TABLE models;
