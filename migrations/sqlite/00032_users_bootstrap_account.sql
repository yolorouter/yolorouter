-- migrations/sqlite/00032_users_bootstrap_account.sql
--
-- Admins can now provision additional local password accounts from the
-- console, so the old "at most one local account" invariant no longer
-- holds. The escape-hatch invariant moves to an explicit marker: the
-- account created by first-run setup is flagged is_bootstrap = 1, and a
-- new partial unique index on that flag takes over both jobs the old
-- is_local index used to do — keeping the bootstrap account unique and
-- settling the concurrent-first-run-setup race at the database level.
-- is_local keeps only its literal meaning: "this account can log in with
-- a password".

-- +goose Up
DROP INDEX idx_users_single_local;
ALTER TABLE users ADD COLUMN is_bootstrap BOOLEAN NOT NULL DEFAULT 0;
UPDATE users SET is_bootstrap = 1 WHERE is_local = 1;
CREATE UNIQUE INDEX idx_users_single_bootstrap ON users(is_bootstrap) WHERE is_bootstrap = 1;

-- +goose Down
-- Only reversible while no admin-created local accounts exist: the restored
-- partial unique index on is_local rejects the downgrade otherwise, loudly
-- rather than silently dropping accounts.
DROP INDEX idx_users_single_bootstrap;
ALTER TABLE users DROP COLUMN is_bootstrap;
CREATE UNIQUE INDEX idx_users_single_local ON users(is_local) WHERE is_local = 1;
