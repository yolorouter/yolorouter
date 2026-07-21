-- migrations/sqlite/00008_request_logs_status_index.sql
--
-- Composite index for the dashboard/analytics "by time + by status" aggregate
-- queries. The single-column created_at index from 00007
-- handles pure time-range scans; this one covers the status_code bucketing
-- that every success-rate / failure-count / cancel-count query layers on top.
--
-- +goose Up
CREATE INDEX idx_request_logs_created_at_status ON request_logs (created_at, status_code);

-- +goose Down
DROP INDEX IF EXISTS idx_request_logs_created_at_status;
