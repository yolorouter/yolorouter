-- migrations/postgres/00007_create_request_logs.sql
--
-- Gateway request logs (PRD §6.5 GATE-13/21, §6.8). See the sqlite twin for
-- the full rationale. Postgres uses BIGINT/BOOLEAN/TIMESTAMPTZ; the
-- cost_known sentinel is a real BOOLEAN here rather than the 0/1 SMALLINT
-- convention on sqlite.
--
-- +goose Up
CREATE TABLE request_logs (
    id             BIGSERIAL PRIMARY KEY,
    request_id     TEXT NOT NULL,
    api_key_id     BIGINT NULL REFERENCES api_keys(id),
    model_name     TEXT NOT NULL,
    provider_id    BIGINT NULL REFERENCES providers(id),
    is_stream      BOOLEAN NOT NULL DEFAULT FALSE,
    status_code    INTEGER NOT NULL,
    input_tokens   INTEGER NOT NULL DEFAULT 0,
    output_tokens  INTEGER NOT NULL DEFAULT 0,
    cost_micros     BIGINT NOT NULL DEFAULT 0,
    cost_known     BOOLEAN NOT NULL DEFAULT FALSE,
    fail_reason    TEXT NULL,
    attempts        INTEGER NOT NULL DEFAULT 1,
    attempts_detail TEXT NULL,
    duration_ms     BIGINT NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_request_logs_api_key_id ON request_logs (api_key_id);
CREATE INDEX idx_request_logs_created_at ON request_logs (created_at);
CREATE INDEX idx_request_logs_model_name ON request_logs (model_name);

-- +goose Down
DROP TABLE IF EXISTS request_logs;
