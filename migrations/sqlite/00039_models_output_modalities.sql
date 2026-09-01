-- migrations/sqlite/00039_models_output_modalities.sql
--
-- SQLite mirror of migrations/postgres/00039_models_output_modalities.sql
-- (TEXT stands in for JSONB; the payload is a JSON array either way).
--
-- models.output_modalities declares what a model produces: a JSON array of
-- modality ids ("text", "image"). The gateway refuses a request whose
-- endpoint serves a modality the model does not declare, so an image model
-- cannot be reached through the chat endpoints and a text model cannot be
-- reached through the images endpoint — the refusal is fast (before any
-- candidate is walked) and names the mismatch.
--
-- Every existing row is a text model, so the column backfills to '["text"]':
-- existing deployments keep routing exactly as they did.

-- +goose Up
ALTER TABLE models ADD COLUMN output_modalities TEXT NOT NULL DEFAULT '["text"]';

-- +goose Down
ALTER TABLE models DROP COLUMN output_modalities;
