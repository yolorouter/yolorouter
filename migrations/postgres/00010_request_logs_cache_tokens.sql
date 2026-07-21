-- migrations/postgres/00010_request_logs_cache_tokens.sql
--
-- Cache token columns for request_logs. See the sqlite twin.

-- +goose Up
ALTER TABLE request_logs ADD COLUMN cache_write_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE request_logs ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE request_logs DROP COLUMN IF EXISTS cache_read_tokens;
ALTER TABLE request_logs DROP COLUMN IF EXISTS cache_write_tokens;
