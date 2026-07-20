-- migrations/postgres/00011_create_request_log_bodies.sql
-- Same shape as sqlite 00011; TIMESTAMPTZ per PITFALLS M5#4.
--
-- +goose Up
CREATE TABLE request_log_bodies (
    id                      BIGSERIAL PRIMARY KEY,
    request_id              VARCHAR(64) NOT NULL DEFAULT '',
    request_body            TEXT NOT NULL DEFAULT '',
    upstream_request_body   TEXT NOT NULL DEFAULT '',
    response_body           TEXT NOT NULL DEFAULT '',
    upstream_response_body  TEXT NOT NULL DEFAULT '',
    stream_body_path        TEXT NOT NULL DEFAULT '',
    stream_body_truncated   BOOLEAN NOT NULL DEFAULT false,
    created_at              TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX idx_request_log_bodies_request_id ON request_log_bodies (request_id);
CREATE INDEX idx_request_log_bodies_created_at ON request_log_bodies (created_at);

-- +goose Down
DROP TABLE IF EXISTS request_log_bodies CASCADE;
