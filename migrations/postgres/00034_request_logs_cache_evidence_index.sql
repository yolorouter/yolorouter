-- migrations/postgres/00034_request_logs_cache_evidence_index.sql
--
-- Partial indexes for the cache-capability classification. See the sqlite
-- twin for the rationale (lifetime evidence queries must not scan the whole
-- request_logs history on every dashboard load).
--
-- CONCURRENTLY, outside a transaction: request_logs takes writes on every
-- gateway request, and a plain CREATE INDEX would hold a lock that blocks
-- settlement for the whole build on a large table. A failed concurrent
-- build leaves an INVALID index under the target name, so each CREATE is
-- preceded by its own concurrent DROP: a rerun after a failure clears the
-- invalid leftover and rebuilds, instead of IF NOT EXISTS "succeeding" by
-- skipping an index that can never be used.

-- +goose NO TRANSACTION
-- +goose Up
DROP INDEX CONCURRENTLY IF EXISTS idx_request_logs_cache_metering_evidence;
CREATE INDEX CONCURRENTLY idx_request_logs_cache_metering_evidence ON request_logs (provider_id)
    WHERE cache_read_tokens > 0 OR cache_write_tokens > 0;
DROP INDEX CONCURRENTLY IF EXISTS idx_request_logs_cache_savings_evidence;
CREATE INDEX CONCURRENTLY idx_request_logs_cache_savings_evidence ON request_logs (provider_id)
    WHERE cache_read_saved_micros <> 0 OR cache_write_extra_micros <> 0;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_request_logs_cache_savings_evidence;
DROP INDEX CONCURRENTLY IF EXISTS idx_request_logs_cache_metering_evidence;
