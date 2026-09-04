--
-- The audio settlement's request_logs columns, on their own number like
-- the image batch's log columns (00041): usage_characters is the volume
-- half of a character-unit usage report (written whenever the report
-- carries one, priced or not, in the settling candidate's own billing
-- meter); audio_pricing_snapshot stores what a priced audio settlement
-- used, written from the same numbers the cost was computed from so a
-- billed row can always explain itself. Column semantics are shared with
-- the sqlite twin.
--

-- +goose Up
ALTER TABLE request_logs ADD COLUMN usage_characters BIGINT NOT NULL DEFAULT 0;
ALTER TABLE request_logs ADD COLUMN audio_pricing_snapshot TEXT NULL;

-- +goose Down
ALTER TABLE request_logs DROP COLUMN IF EXISTS usage_characters;
ALTER TABLE request_logs DROP COLUMN IF EXISTS audio_pricing_snapshot;
