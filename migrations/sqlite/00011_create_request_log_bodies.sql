-- migrations/sqlite/00011_create_request_log_bodies.sql
--
-- Request/response bodies for one gateway request (PRD §6.8.4/§6.8.6, LOG-06/08).
-- 1:1 with request_logs via request_id (UNIQUE — enforced 1:1 + idempotent
-- UPSERT). No FK, per reference repo relay_log_bodies. Stream sent-SSE lives
-- at stream_body_path (data/bodies/<request_id>.stream) — these two TEXT
-- response columns are empty for stream requests.
--
-- +goose Up
CREATE TABLE request_log_bodies (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id              TEXT NOT NULL DEFAULT '',
    request_body            TEXT NOT NULL DEFAULT '',
    upstream_request_body   TEXT NOT NULL DEFAULT '',
    response_body           TEXT NOT NULL DEFAULT '',
    upstream_response_body  TEXT NOT NULL DEFAULT '',
    stream_body_path        TEXT NOT NULL DEFAULT '',
    stream_body_truncated   INTEGER NOT NULL DEFAULT 0,
    created_at              DATETIME NOT NULL
);
CREATE UNIQUE INDEX idx_request_log_bodies_request_id ON request_log_bodies (request_id);
CREATE INDEX idx_request_log_bodies_created_at ON request_log_bodies (created_at);

-- +goose Down
DROP TABLE IF EXISTS request_log_bodies;
