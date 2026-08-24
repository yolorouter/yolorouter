-- migrations/postgres/00032_users_bootstrap_account.sql
--
-- Postgres mirror of migrations/sqlite/00032_users_bootstrap_account.sql;
-- see that file for the full rationale. The single-local-account invariant
-- is relaxed (admins may provision more local password accounts) and the
-- escape-hatch invariant moves to a new is_bootstrap flag guarded by a
-- partial unique index, which also inherits the concurrent-setup race
-- guard from the old is_local index.

-- +goose Up
DROP INDEX idx_users_single_local;
ALTER TABLE users ADD COLUMN is_bootstrap BOOLEAN NOT NULL DEFAULT FALSE;
UPDATE users SET is_bootstrap = TRUE WHERE is_local = TRUE;
CREATE UNIQUE INDEX idx_users_single_bootstrap ON users(is_bootstrap) WHERE is_bootstrap;

-- +goose Down
-- Only reversible while no admin-created local accounts exist: the restored
-- partial unique index on is_local rejects the downgrade otherwise, loudly
-- rather than silently dropping accounts.
DROP INDEX idx_users_single_bootstrap;
ALTER TABLE users DROP COLUMN is_bootstrap;
-- Bare boolean (not "= 1"): Postgres has no boolean/integer implicit cast.
CREATE UNIQUE INDEX idx_users_single_local ON users(is_local) WHERE is_local;
