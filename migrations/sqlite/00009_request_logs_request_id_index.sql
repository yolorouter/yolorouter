-- migrations/sqlite/00009_request_logs_request_id_index.sql
--
-- Index for request_id lookups: the request-log detail endpoint and the
-- exact request_id list filter both run against this column
-- (locating a single request precisely by its request identifier). Without it,
-- every detail lookup degrades to
-- a full table scan as request_logs grows. request_id is effectively unique
-- (gateway generateRequestID is crypto/rand hex), but the index is non-unique
-- so a hypothetical collision can never fail the gateway's log write.
--
-- +goose Up
CREATE INDEX idx_request_logs_request_id ON request_logs (request_id);

-- +goose Down
DROP INDEX IF EXISTS idx_request_logs_request_id;
