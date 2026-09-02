-- migrations/sqlite/00041_request_logs_image_count.sql
--
-- SQLite mirror of migrations/postgres/00041_request_logs_image_count.sql.
--
-- The delivered-image count of a per-image usage report, as its own column.
-- image_pricing_snapshot already carries actual_n, but a snapshot is only
-- written when a price RESOLVED — an unpriced delivery still counts as
-- volume — and summing a JSON field cannot be written once for SQLite and
-- Postgres both. The column is written from the usage report (unit=image);
-- this migration backfills it from the snapshot so historical priced rows
-- join it.

-- +goose Up
ALTER TABLE request_logs ADD COLUMN image_count INTEGER NOT NULL DEFAULT 0;
-- IFNULL mirrors the Postgres twin's COALESCE: a snapshot that parses but
-- lacks actual_n (only hand-edited rows — the writer always sets it) must
-- backfill as 0, not abort the migration on the NOT NULL column.
UPDATE request_logs
SET image_count = IFNULL(CAST(json_extract(image_pricing_snapshot, '$.actual_n') AS INTEGER), 0)
WHERE image_pricing_snapshot IS NOT NULL AND image_pricing_snapshot != '';

-- +goose Down
ALTER TABLE request_logs DROP COLUMN image_count;
