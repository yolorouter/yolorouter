-- migrations/sqlite/00007_create_request_logs.sql
--
-- Gateway request logs (PRD §6.5 GATE-13/21, §6.8). One row per gateway
-- business request — a failover still counts as ONE row, with the attempts
-- column recording how many candidates were tried (GATE-13). v0.1 stores no
-- request/response bodies: there is no §6.8 query page yet, so the optional
-- body-capture switch from the reference project is omitted entirely.
--
-- cost_known = 0 marks "price or token missing, do NOT display as zero cost"
-- (GATE-21 / PRD §6.7.6) — the row still records the request happened.
--
-- +goose Up
CREATE TABLE request_logs (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id     TEXT NOT NULL,
    api_key_id     INTEGER NULL REFERENCES api_keys(id),
    model_name     TEXT NOT NULL,
    provider_id    INTEGER NULL REFERENCES providers(id),
    is_stream      INTEGER NOT NULL DEFAULT 0,
    status_code    INTEGER NOT NULL,
    input_tokens   INTEGER NOT NULL DEFAULT 0,
    output_tokens  INTEGER NOT NULL DEFAULT 0,
    cost_cents     INTEGER NOT NULL DEFAULT 0,
    cost_known     INTEGER NOT NULL DEFAULT 0,
    fail_reason    TEXT NULL,
    attempts        INTEGER NOT NULL DEFAULT 1,
    attempts_detail TEXT NULL,
    duration_ms     INTEGER NOT NULL DEFAULT 0,
    created_at     DATETIME NOT NULL
);

CREATE INDEX idx_request_logs_api_key_id ON request_logs (api_key_id);
CREATE INDEX idx_request_logs_created_at ON request_logs (created_at);
CREATE INDEX idx_request_logs_model_name ON request_logs (model_name);

-- +goose Down
DROP TABLE IF EXISTS request_logs;
