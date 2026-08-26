-- migrations/sqlite/00034_request_logs_cache_evidence_index.sql
--
-- Partial indexes backing the cache-capability classification behind the
-- dashboard's cache KPI cards. The classifier asks two lifetime (unwindowed)
-- questions per provider: "has any row ever carried a cache token count?"
-- and "has any row ever settled a nonzero cache saving/premium?". Without
-- these, both questions scan the whole request_logs history on every
-- dashboard load. The predicates match the classifier's WHERE clauses
-- exactly, and the indexes stay tiny: only cache-active rows enter them.

-- +goose Up
CREATE INDEX idx_request_logs_cache_metering_evidence ON request_logs (provider_id)
    WHERE cache_read_tokens > 0 OR cache_write_tokens > 0;
CREATE INDEX idx_request_logs_cache_savings_evidence ON request_logs (provider_id)
    WHERE cache_read_saved_micros != 0 OR cache_write_extra_micros != 0;

-- +goose Down
DROP INDEX IF EXISTS idx_request_logs_cache_savings_evidence;
DROP INDEX IF EXISTS idx_request_logs_cache_metering_evidence;
