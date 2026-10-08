-- The admin-audit retention setting: how many days of admin_audit_logs rows
-- the background cleanup keeps, one system_settings row. Unlike the
-- request-log retention (seeded at 0 = keep forever), audit trails ship with
-- a bounded default of 180 days: console audit volume is tiny, and an audit
-- log nobody prunes slowly becomes a liability nobody reads. 0 stays legal
-- and means keep forever for deployments under a stricter retention mandate.
-- Column semantics are shared with the sqlite twin.

-- +goose Up
INSERT INTO system_settings (key, value) VALUES
    ('admin_audit_retention_days', '180');

-- +goose Down
DELETE FROM system_settings WHERE key = 'admin_audit_retention_days';
