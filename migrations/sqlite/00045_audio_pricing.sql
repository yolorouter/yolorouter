--
-- SQLite mirror of migrations/postgres/00045_audio_pricing.sql.
--
-- Character pricing (TTS): a candidate in audio billing mode carries one
-- per-million-characters price — speech has no input/output or tier axes.
-- audio_unit_price is NULL when unset, which is deliberately not the same
-- as free: an unpriced audio candidate settles as unknown, never as zero.
--

-- +goose Up
ALTER TABLE model_candidates ADD COLUMN audio_unit_price REAL NULL;

-- +goose Down
ALTER TABLE model_candidates DROP COLUMN audio_unit_price;
