-- migrations/postgres/00056_models_capability_metadata.sql
--
-- Five admin-declared capability metadata columns on models, none of which
-- affects routing in this phase — they record what the admin knows so the
-- console can show it:
--
--   context_length     declared context window in tokens (NULL = undeclared)
--   input_modalities   JSON array of input modality ids ("text", "image", ...)
--   supports_tools     tri-state: can be called with tools (NULL = undeclared)
--   supports_reasoning tri-state: produces reasoning output (NULL = undeclared)
--   temperature        declared recommended default temperature (NULL = undeclared)
--
-- input_modalities supersedes the existing supports_image_input boolean. That
-- column is deprecated but kept in mirror — the vision-fallback hot path
-- still reads it — so the migration backfills the new array from it: a row
-- declared able to read images becomes ["text","image"] (an image-reading
-- model still reads text; that is what the fallback feature was built
-- around), a row declared unable becomes ["text"], and an undeclared row
-- stays NULL, because an undeclared input must never be narrowed into a
-- declaration. From this version on, every write of input_modalities derives
-- supports_image_input in the same statement, so the pair cannot drift.
-- Column semantics are shared with the sqlite twin.

-- +goose Up
ALTER TABLE models ADD COLUMN context_length INTEGER NULL;
ALTER TABLE models ADD COLUMN input_modalities TEXT NULL;
ALTER TABLE models ADD COLUMN supports_tools BOOLEAN NULL;
ALTER TABLE models ADD COLUMN supports_reasoning BOOLEAN NULL;
-- DOUBLE PRECISION rather than a scaled NUMERIC: the field is a Go float64
-- (model.Model.Temperature) and the sqlite twin stores an IEEE double, so a
-- fixed scale here would silently round a 3-decimal declaration (0.705 →
-- 0.71) on this backend only — the two trees must round-trip the same bits.
ALTER TABLE models ADD COLUMN temperature DOUBLE PRECISION NULL;

UPDATE models SET input_modalities = '["text","image"]' WHERE supports_image_input IS TRUE;
UPDATE models SET input_modalities = '["text"]' WHERE supports_image_input IS FALSE;

-- +goose Down
ALTER TABLE models DROP COLUMN IF EXISTS context_length;
ALTER TABLE models DROP COLUMN IF EXISTS input_modalities;
ALTER TABLE models DROP COLUMN IF EXISTS supports_tools;
ALTER TABLE models DROP COLUMN IF EXISTS supports_reasoning;
ALTER TABLE models DROP COLUMN IF EXISTS temperature;
