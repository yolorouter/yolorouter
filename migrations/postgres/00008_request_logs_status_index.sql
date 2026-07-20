-- migrations/postgres/00008_request_logs_status_index.sql
--
-- Composite index for the dashboard/analytics "by time + by status" aggregate
-- queries (PRD §6.6/§6.7). See the sqlite twin for the full rationale.
--
-- +goose Up
CREATE INDEX idx_request_logs_created_at_status ON request_logs (created_at, status_code);

-- +goose Down
DROP INDEX IF EXISTS idx_request_logs_created_at_status;
