-- migrations/sqlite/00044_request_logs_usage_seconds.sql
--
-- SQLite mirror of migrations/postgres/00044_request_logs_usage_seconds.sql:
-- the video settlement digest's usage half — delivered seconds, stamped
-- together with the cost back-fill. 0 on token rows and pre-column rows.

-- +goose Up
ALTER TABLE request_logs
    ADD COLUMN usage_seconds INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE request_logs DROP COLUMN usage_seconds;
