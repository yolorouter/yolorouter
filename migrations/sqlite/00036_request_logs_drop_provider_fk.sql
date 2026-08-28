-- migrations/sqlite/00036_request_logs_drop_provider_fk.sql
--
-- Providers can now be deleted outright, and request_logs rows are history
-- that must outlive the provider they point at: per-provider aggregates keep
-- grouping by the stored provider_id, the name-lookup layer already renders
-- a missing provider as an empty name, and attempts_detail carries its own
-- snapshot of the provider/key names for the detail view. The
-- REFERENCES providers(id) clause from the original table definition is the
-- only thing in the way — it would reject the provider DELETE for as long
-- as any history row exists.
--
-- SQLite cannot drop a single constraint in place, so this rebuilds the
-- table: create the same table minus that one clause, copy every row, swap
-- names, and recreate every index. Explicit column lists guard against
-- column-order drift. Nothing references request_logs by foreign key
-- (request_log_bodies links by request_id on purpose), so the swap is safe
-- with foreign keys enforced. The api_key_id constraint is kept as-is.
-- Copying the rows with their original ids advances the rebuilt table's
-- autoincrement counter to the highest copied id, so fresh inserts cannot
-- reuse the id of any surviving row. (An EMPTY table restarts ids at 1 —
-- acceptable because nothing joins on request_logs.id; request_log_bodies
-- links by request_id, and id is only an ordering tiebreaker.)
--
-- Operational note: request_logs is the highest-volume table and this
-- duplicates it inside one transaction. The upgrade transiently needs free
-- disk on the order of the table's size (on top of the pre-migration
-- backup), takes time proportional to the row count, and the pages freed by
-- dropping the old table stay on the file's free list for later reuse
-- rather than shrinking the file.

-- +goose Up
CREATE TABLE request_logs_rebuilt (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id     TEXT NOT NULL,
    api_key_id     INTEGER NULL REFERENCES api_keys(id),
    model_name     TEXT NOT NULL,
    provider_id    INTEGER NULL,
    is_stream      INTEGER NOT NULL DEFAULT 0,
    status_code    INTEGER NOT NULL,
    input_tokens   INTEGER NOT NULL DEFAULT 0,
    output_tokens  INTEGER NOT NULL DEFAULT 0,
    cost_micros     INTEGER NOT NULL DEFAULT 0,
    cost_known     INTEGER NOT NULL DEFAULT 0,
    fail_reason    TEXT NULL,
    attempts        INTEGER NOT NULL DEFAULT 1,
    attempts_detail TEXT NULL,
    duration_ms     INTEGER NOT NULL DEFAULT 0,
    created_at     DATETIME NOT NULL,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_saved_micros INTEGER NOT NULL DEFAULT 0,
    cache_write_extra_micros INTEGER NOT NULL DEFAULT 0,
    compress_estimated_tokens_saved      INTEGER NOT NULL DEFAULT 0,
    compress_estimated_cost_saved_micros INTEGER NOT NULL DEFAULT 0,
    compress_skip_reason                 TEXT    NOT NULL DEFAULT '',
    compressors_applied                  TEXT    NOT NULL DEFAULT '',
    request_path TEXT NOT NULL DEFAULT '',
    upstream_url TEXT NOT NULL DEFAULT '',
    facts_json TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT '',
    parent_request_id TEXT NOT NULL DEFAULT '',
    user_id INTEGER NULL,
    settled_input_price NUMERIC NULL,
    settled_output_price NUMERIC NULL,
    settled_cache_write_price NUMERIC NULL,
    settled_cache_read_price NUMERIC NULL
);

INSERT INTO request_logs_rebuilt (
    id, request_id, api_key_id, model_name, provider_id, is_stream,
    status_code, input_tokens, output_tokens, cost_micros, cost_known,
    fail_reason, attempts, attempts_detail, duration_ms, created_at,
    cache_write_tokens, cache_read_tokens, cache_read_saved_micros,
    cache_write_extra_micros, compress_estimated_tokens_saved,
    compress_estimated_cost_saved_micros, compress_skip_reason,
    compressors_applied, request_path, upstream_url, facts_json, source,
    parent_request_id, user_id, settled_input_price, settled_output_price,
    settled_cache_write_price, settled_cache_read_price
)
SELECT
    id, request_id, api_key_id, model_name, provider_id, is_stream,
    status_code, input_tokens, output_tokens, cost_micros, cost_known,
    fail_reason, attempts, attempts_detail, duration_ms, created_at,
    cache_write_tokens, cache_read_tokens, cache_read_saved_micros,
    cache_write_extra_micros, compress_estimated_tokens_saved,
    compress_estimated_cost_saved_micros, compress_skip_reason,
    compressors_applied, request_path, upstream_url, facts_json, source,
    parent_request_id, user_id, settled_input_price, settled_output_price,
    settled_cache_write_price, settled_cache_read_price
FROM request_logs;

DROP TABLE request_logs;
ALTER TABLE request_logs_rebuilt RENAME TO request_logs;

CREATE INDEX idx_request_logs_api_key_id ON request_logs (api_key_id);
CREATE INDEX idx_request_logs_created_at ON request_logs (created_at);
CREATE INDEX idx_request_logs_model_name ON request_logs (model_name);
CREATE INDEX idx_request_logs_created_at_status ON request_logs (created_at, status_code);
CREATE INDEX idx_request_logs_request_id ON request_logs (request_id);
CREATE INDEX idx_request_logs_user_id ON request_logs (user_id);
CREATE INDEX idx_request_logs_cache_metering_evidence ON request_logs (provider_id)
    WHERE cache_read_tokens > 0 OR cache_write_tokens > 0;
CREATE INDEX idx_request_logs_cache_savings_evidence ON request_logs (provider_id)
    WHERE cache_read_saved_micros != 0 OR cache_write_extra_micros != 0;

-- +goose Down
-- Restores the provider_id foreign key with the same rebuild in reverse.
-- The copy fails if any row references a provider that has since been
-- deleted — deliberate: a downgrade is only defined for databases where no
-- provider deletion ever happened, because the old schema cannot represent
-- an orphaned history row.
CREATE TABLE request_logs_rebuilt (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id     TEXT NOT NULL,
    api_key_id     INTEGER NULL REFERENCES api_keys(id),
    model_name     TEXT NOT NULL,
    provider_id    INTEGER NULL REFERENCES providers(id),
    is_stream      INTEGER NOT NULL DEFAULT 0,
    status_code    INTEGER NOT NULL,
    input_tokens   INTEGER NOT NULL DEFAULT 0,
    output_tokens  INTEGER NOT NULL DEFAULT 0,
    cost_micros     INTEGER NOT NULL DEFAULT 0,
    cost_known     INTEGER NOT NULL DEFAULT 0,
    fail_reason    TEXT NULL,
    attempts        INTEGER NOT NULL DEFAULT 1,
    attempts_detail TEXT NULL,
    duration_ms     INTEGER NOT NULL DEFAULT 0,
    created_at     DATETIME NOT NULL,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_saved_micros INTEGER NOT NULL DEFAULT 0,
    cache_write_extra_micros INTEGER NOT NULL DEFAULT 0,
    compress_estimated_tokens_saved      INTEGER NOT NULL DEFAULT 0,
    compress_estimated_cost_saved_micros INTEGER NOT NULL DEFAULT 0,
    compress_skip_reason                 TEXT    NOT NULL DEFAULT '',
    compressors_applied                  TEXT    NOT NULL DEFAULT '',
    request_path TEXT NOT NULL DEFAULT '',
    upstream_url TEXT NOT NULL DEFAULT '',
    facts_json TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT '',
    parent_request_id TEXT NOT NULL DEFAULT '',
    user_id INTEGER NULL,
    settled_input_price NUMERIC NULL,
    settled_output_price NUMERIC NULL,
    settled_cache_write_price NUMERIC NULL,
    settled_cache_read_price NUMERIC NULL
);

INSERT INTO request_logs_rebuilt (
    id, request_id, api_key_id, model_name, provider_id, is_stream,
    status_code, input_tokens, output_tokens, cost_micros, cost_known,
    fail_reason, attempts, attempts_detail, duration_ms, created_at,
    cache_write_tokens, cache_read_tokens, cache_read_saved_micros,
    cache_write_extra_micros, compress_estimated_tokens_saved,
    compress_estimated_cost_saved_micros, compress_skip_reason,
    compressors_applied, request_path, upstream_url, facts_json, source,
    parent_request_id, user_id, settled_input_price, settled_output_price,
    settled_cache_write_price, settled_cache_read_price
)
SELECT
    id, request_id, api_key_id, model_name, provider_id, is_stream,
    status_code, input_tokens, output_tokens, cost_micros, cost_known,
    fail_reason, attempts, attempts_detail, duration_ms, created_at,
    cache_write_tokens, cache_read_tokens, cache_read_saved_micros,
    cache_write_extra_micros, compress_estimated_tokens_saved,
    compress_estimated_cost_saved_micros, compress_skip_reason,
    compressors_applied, request_path, upstream_url, facts_json, source,
    parent_request_id, user_id, settled_input_price, settled_output_price,
    settled_cache_write_price, settled_cache_read_price
FROM request_logs;

DROP TABLE request_logs;
ALTER TABLE request_logs_rebuilt RENAME TO request_logs;

CREATE INDEX idx_request_logs_api_key_id ON request_logs (api_key_id);
CREATE INDEX idx_request_logs_created_at ON request_logs (created_at);
CREATE INDEX idx_request_logs_model_name ON request_logs (model_name);
CREATE INDEX idx_request_logs_created_at_status ON request_logs (created_at, status_code);
CREATE INDEX idx_request_logs_request_id ON request_logs (request_id);
CREATE INDEX idx_request_logs_user_id ON request_logs (user_id);
CREATE INDEX idx_request_logs_cache_metering_evidence ON request_logs (provider_id)
    WHERE cache_read_tokens > 0 OR cache_write_tokens > 0;
CREATE INDEX idx_request_logs_cache_savings_evidence ON request_logs (provider_id)
    WHERE cache_read_saved_micros != 0 OR cache_write_extra_micros != 0;
