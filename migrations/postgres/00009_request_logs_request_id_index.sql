-- migrations/postgres/00009_request_logs_request_id_index.sql
--
-- Index for request_id lookups. See the sqlite twin for the full rationale.

-- +goose Up
CREATE INDEX idx_request_logs_request_id ON request_logs (request_id);

-- +goose Down
DROP INDEX IF EXISTS idx_request_logs_request_id;
