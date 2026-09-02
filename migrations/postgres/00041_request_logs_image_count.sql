-- migrations/postgres/00041_request_logs_image_count.sql
--
-- The delivered-image count of a per-image usage report, as its own column.
-- image_pricing_snapshot already carries actual_n, but a snapshot is only
-- written when a price RESOLVED — an unpriced delivery still counts as
-- volume — and summing a JSON field cannot be written once for SQLite and
-- Postgres both. The column is written from the usage report (unit=image);
-- this migration backfills it from the snapshot so historical priced rows
-- join it. Column semantics are shared with the sqlite twin.

-- +goose Up
ALTER TABLE request_logs ADD COLUMN image_count BIGINT NOT NULL DEFAULT 0;
UPDATE request_logs
SET image_count = COALESCE((image_pricing_snapshot::jsonb ->> 'actual_n')::bigint, 0)
WHERE image_pricing_snapshot IS NOT NULL AND image_pricing_snapshot != '';

-- +goose Down
ALTER TABLE request_logs DROP COLUMN IF EXISTS image_count;
