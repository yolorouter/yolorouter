-- migrations/postgres/00044_request_logs_usage_seconds.sql
--
-- usage_seconds on request_logs is the video settlement digest's usage
-- half: the delivered seconds a completed video job actually ran, stamped
-- by the same settlement write that back-fills cost_micros (migration
-- 00043 carried the request_id that links the two). The image settlement
-- has its digest in image_count + the pricing snapshot; video prices per
-- second, so its unit of delivery is a second. Rows that settle in tokens
-- (or predate the column) keep 0 and the usage figures stay the token
-- counts — one row always reads in a single billing unit.
--
-- NOT NULL DEFAULT 0 keeps every existing row valid without a backfill:
-- completed video jobs from before the column settled with a cost but no
-- usage projection, exactly the asymmetric shape the write allows.

-- +goose Up
ALTER TABLE request_logs
    ADD COLUMN usage_seconds BIGINT NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE request_logs
    DROP COLUMN IF EXISTS usage_seconds;
