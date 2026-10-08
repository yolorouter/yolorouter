-- migrations/sqlite/00052_admin_audit_logs.sql
--
-- SQLite mirror of migrations/postgres/00052_admin_audit_logs.sql (TEXT
-- stands in for the twin's wider columns; the payloads are the same
-- strings).
--
-- The admin audit log: one row per console write request, captured at the
-- /api/admin group root and persisted asynchronously. Rows are
-- append-only evidence with no UPDATE/DELETE path; admin_username is a
-- write-time snapshot; request_id is the bare request-id UUID echoed as
-- the response's X-Request-Id header. See the postgres twin's header note
-- for the full column contract.

-- +goose Up
CREATE TABLE admin_audit_logs (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at     DATETIME NOT NULL,
    admin_user_id  INTEGER NULL,
    admin_username TEXT NOT NULL DEFAULT '',
    action         TEXT NOT NULL,
    resource_type  TEXT NOT NULL DEFAULT '',
    resource_id    TEXT NOT NULL DEFAULT '',
    detail         TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL DEFAULT 'success',
    error_brief    TEXT NOT NULL DEFAULT '',
    source_ip      TEXT NOT NULL DEFAULT '',
    user_agent     TEXT NOT NULL DEFAULT '',
    request_id     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_admin_audit_logs_created_at ON admin_audit_logs(created_at);
CREATE INDEX idx_admin_audit_logs_admin_user_id ON admin_audit_logs(admin_user_id);
CREATE INDEX idx_admin_audit_logs_action ON admin_audit_logs(action);
CREATE INDEX idx_admin_audit_logs_request_id ON admin_audit_logs(request_id);

-- +goose Down
DROP INDEX IF EXISTS idx_admin_audit_logs_request_id;
DROP INDEX IF EXISTS idx_admin_audit_logs_action;
DROP INDEX IF EXISTS idx_admin_audit_logs_admin_user_id;
DROP INDEX IF EXISTS idx_admin_audit_logs_created_at;
DROP TABLE IF EXISTS admin_audit_logs;
