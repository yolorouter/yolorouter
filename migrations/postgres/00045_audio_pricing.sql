--
-- Character pricing (TTS): a candidate in audio billing mode carries one
-- per-million-characters price — speech has no input/output or tier axes.
-- audio_unit_price is NULL when unset, which is deliberately not the same
-- as free: an unpriced audio candidate settles as unknown, never as zero.
-- Column semantics are shared with the sqlite twin.
--

-- +goose Up
ALTER TABLE model_candidates ADD COLUMN audio_unit_price NUMERIC(20,10) NULL;

-- +goose Down
ALTER TABLE model_candidates DROP COLUMN IF EXISTS audio_unit_price;
