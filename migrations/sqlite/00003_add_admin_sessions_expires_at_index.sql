-- migrations/sqlite/00003_add_admin_sessions_expires_at_index.sql
-- admin_sessions has the same unbounded-growth issue as login_attempts in
-- other projects: every login inserts a row, but nothing ever deletes an
-- expired one — FindValidSessionByID's expires_at > ? filter just hides
-- them from queries, the rows themselves stay forever. This index
-- supports the cleanup query (repository.DeleteExpiredSessions, called
-- from service.Login's transaction) so the DELETE isn't a full table scan.

-- +goose Up
CREATE INDEX IF NOT EXISTS idx_admin_sessions_expires_at ON admin_sessions(expires_at);

-- +goose Down
DROP INDEX IF EXISTS idx_admin_sessions_expires_at;
