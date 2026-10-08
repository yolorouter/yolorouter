-- Provider key display suffix: the last four characters of the key
-- plaintext, stored in the clear beside the existing key_prefix so a key
-- row is identifiable from both ends (which key it is, by prefix; which of
-- several near-identical keys, by suffix). Four trailing characters of a
-- credential whose accept-time minimum is far longer is display metadata,
-- not secret material — the same reasoning that already exposes the
-- prefix. New keys get the suffix at insert; keys stored before this
-- column existed keep '' until the startup backfill pass decrypts each one
-- in-process and fills it (plaintext never reaches a migration file, a
-- log line, or any other persistence). '' therefore means "not yet
-- derived", and the backfill is the only writer for those rows. Column
-- semantics are shared with the sqlite twin.

-- +goose Up
ALTER TABLE provider_keys ADD COLUMN key_suffix TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE provider_keys DROP COLUMN key_suffix;
