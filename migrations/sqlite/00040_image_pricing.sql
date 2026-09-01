-- migrations/sqlite/00040_image_pricing.sql
--
-- SQLite mirror of migrations/postgres/00040_image_pricing.sql (TEXT stands
-- in for JSONB; the payloads are JSON either way).
--
-- Per-image pricing: a candidate declares how it bills (token, the default
-- every row had before, or image) and, when it bills per image, a tier
-- table keyed by the request's quality and size with an optional default
-- price. Settlement prices an image request by resolving the tier and
-- multiplying by the number of images actually delivered.
--
-- request_logs.image_pricing_snapshot stores what that resolution used —
-- mode, request axes, requested vs delivered count, unit price — written
-- from the same numbers the cost was computed from, so a billed row can
-- always explain itself.

-- +goose Up
ALTER TABLE model_candidates ADD COLUMN billing_mode TEXT NOT NULL DEFAULT 'token';
ALTER TABLE model_candidates ADD COLUMN image_pricing_tiers TEXT NULL;
ALTER TABLE request_logs ADD COLUMN image_pricing_snapshot TEXT NULL;

-- +goose Down
ALTER TABLE model_candidates DROP COLUMN billing_mode;
ALTER TABLE model_candidates DROP COLUMN image_pricing_tiers;
ALTER TABLE request_logs DROP COLUMN image_pricing_snapshot;
