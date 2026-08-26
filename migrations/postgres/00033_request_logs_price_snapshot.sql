-- migrations/postgres/00033_request_logs_price_snapshot.sql
--
-- Price snapshot columns for request_logs. See the sqlite twin for the column
-- semantics (effective billed prices, NULL together on unpriced and
-- pre-migration rows, never backfilled).

-- +goose Up
ALTER TABLE request_logs ADD COLUMN settled_input_price NUMERIC(20,10) NULL;
ALTER TABLE request_logs ADD COLUMN settled_output_price NUMERIC(20,10) NULL;
ALTER TABLE request_logs ADD COLUMN settled_cache_write_price NUMERIC(20,10) NULL;
ALTER TABLE request_logs ADD COLUMN settled_cache_read_price NUMERIC(20,10) NULL;

-- +goose Down
ALTER TABLE request_logs DROP COLUMN IF EXISTS settled_cache_read_price;
ALTER TABLE request_logs DROP COLUMN IF EXISTS settled_cache_write_price;
ALTER TABLE request_logs DROP COLUMN IF EXISTS settled_output_price;
ALTER TABLE request_logs DROP COLUMN IF EXISTS settled_input_price;
