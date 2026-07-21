-- migrations/sqlite/00010_request_logs_cache_tokens.sql
--
-- Cache token columns for request_logs. Modern models bill
-- prompt-cache reads/writes separately from plain input/output, and the
-- gateway now extracts these from the upstream usage object
-- (OpenAI prompt_tokens_details.cached_tokens, Anthropic
-- cache_creation_input_tokens). Without these columns the cache line items
-- in computeCost would silently drop, undercharging requests that used cache.
--
-- +goose Up
ALTER TABLE request_logs ADD COLUMN cache_write_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE request_logs ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0;

-- +goose Down
-- SQLite 3.35+ supports ALTER TABLE DROP COLUMN; goose's modernc driver
-- ships a newer version than that, so the down migration can actually roll
-- back (earlier this was a no-op, which broke the rollback contract).
ALTER TABLE request_logs DROP COLUMN cache_read_tokens;
ALTER TABLE request_logs DROP COLUMN cache_write_tokens;
