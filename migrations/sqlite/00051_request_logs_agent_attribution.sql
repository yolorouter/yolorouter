-- migrations/sqlite/00051_request_logs_agent_attribution.sql
--
-- SQLite mirror of migrations/postgres/00051_request_logs_agent_attribution.sql:
-- the coding-agent attribution of a request (normalized tool name + the
-- tool's own session id), promoted from the captured header snapshot to
-- queryable columns. NULL when no recognizable tool signature was sent and
-- on every pre-column row; the postgres twin carries the full column
-- semantics.

-- +goose Up
ALTER TABLE request_logs
    ADD COLUMN agent_client TEXT NULL;
ALTER TABLE request_logs
    ADD COLUMN agent_session_id TEXT NULL;

-- +goose Down
ALTER TABLE request_logs DROP COLUMN agent_session_id;
ALTER TABLE request_logs DROP COLUMN agent_client;
