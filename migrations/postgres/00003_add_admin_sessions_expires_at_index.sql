-- migrations/postgres/00003_add_admin_sessions_expires_at_index.sql
-- Mirrors migrations/sqlite/00003_add_admin_sessions_expires_at_index.sql for the same reasons.

-- +goose Up
CREATE INDEX IF NOT EXISTS idx_admin_sessions_expires_at ON admin_sessions(expires_at);

-- +goose Down
DROP INDEX IF EXISTS idx_admin_sessions_expires_at;
