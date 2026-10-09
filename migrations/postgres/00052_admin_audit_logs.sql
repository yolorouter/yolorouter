-- The admin audit log: one row per console write request (and, later, per
-- explicit security event the automatic layer cannot see), captured at the
-- /api/admin group root and persisted asynchronously so auditing never
-- delays a request. Rows are append-only evidence: there is deliberately
-- no UPDATE or DELETE path anywhere in the API surface — the only removal
-- is the retention sweep, driven by its own setting. admin_username is a
-- snapshot taken at write time so history stays readable after the account
-- is renamed or deleted; admin_user_id is NULL for requests that carried
-- no resolved identity (public auth endpoints, unauthenticated probes).
-- detail is a JSON document supplied by the explicit-event layer (field
-- level diffs, event context); it passes a redaction pipeline before it
-- gets here. request_id is the bare UUID the request-id middleware issued
-- and echoed back as that response's X-Request-Id header — the row's
-- reconciliation key against a specific console request, not a link into
-- request_logs (admin requests never write those). Column semantics are
-- shared with the sqlite twin.

-- +goose Up
CREATE TABLE admin_audit_logs (
    id             BIGSERIAL PRIMARY KEY,
    created_at     TIMESTAMPTZ NOT NULL,
    admin_user_id  BIGINT NULL,
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
