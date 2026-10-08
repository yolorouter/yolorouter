-- migrations/sqlite/00053_provider_key_suffix.sql
--
-- SQLite mirror of migrations/postgres/00053_provider_key_suffix.sql.
--
-- Provider key display suffix: the last four characters of the key
-- plaintext, stored in the clear beside the existing key_prefix so a key
-- row is identifiable from both ends. Four trailing characters of a
-- credential whose accept-time minimum is far longer is display metadata,
-- not secret material — the same reasoning that already exposes the
-- prefix. New keys get the suffix at insert; keys stored before this
-- column existed keep '' until the startup backfill pass decrypts each one
-- in-process and fills it (plaintext never reaches a migration file, a
-- log line, or any other persistence). '' therefore means "not yet
-- derived", and the backfill is the only writer for those rows.

-- +goose Up
ALTER TABLE provider_keys ADD COLUMN key_suffix TEXT NOT NULL DEFAULT '';

-- +goose Down
-- SQLite 3.35+ supports ALTER TABLE DROP COLUMN; goose's modernc driver
-- ships a newer version than that (same precedent as 00010's down).
ALTER TABLE provider_keys DROP COLUMN key_suffix;
