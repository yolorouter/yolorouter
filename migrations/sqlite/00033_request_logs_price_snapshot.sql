-- migrations/sqlite/00033_request_logs_price_snapshot.sql
--
-- Price snapshot for request_logs: the four unit prices (per million tokens)
-- the settlement actually billed with, copied onto the row at write time so a
-- historical row can be re-priced by hand after the candidate's prices change.
--
-- The cache columns hold the EFFECTIVE prices — a candidate without a
-- configured cache price bills cache tokens at the input price, and that
-- fallback value is what lands here, because the snapshot answers "what was
-- this row billed at", not "what was configured".
--
-- All four are NULL together on rows that predate this migration and on rows
-- whose cost could not be priced (cost_known = false): an unpriced row has no
-- billed prices to snapshot, and history is never backfilled.

-- +goose Up
ALTER TABLE request_logs ADD COLUMN settled_input_price NUMERIC NULL;
ALTER TABLE request_logs ADD COLUMN settled_output_price NUMERIC NULL;
ALTER TABLE request_logs ADD COLUMN settled_cache_write_price NUMERIC NULL;
ALTER TABLE request_logs ADD COLUMN settled_cache_read_price NUMERIC NULL;

-- +goose Down
ALTER TABLE request_logs DROP COLUMN settled_cache_read_price;
ALTER TABLE request_logs DROP COLUMN settled_cache_write_price;
ALTER TABLE request_logs DROP COLUMN settled_output_price;
ALTER TABLE request_logs DROP COLUMN settled_input_price;
