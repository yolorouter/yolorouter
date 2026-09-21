-- The key-auto-recovery settings pair: a global switch plus the probe
-- interval in whole minutes, one system_settings row each. The feature
-- ships enabled with a 30-minute interval, so existing deployments pick
-- it up on upgrade without any manual step; an admin who does not want
-- it turns it off from the console. The two rows share one version (CAS
-- on save, same contract as the custom-system-prompt pair seeded by
-- 00015). Column semantics are shared with the sqlite twin.

-- +goose Up
INSERT INTO system_settings (key, value) VALUES
    ('key_auto_recovery_enabled', 'true'),
    ('key_auto_recovery_interval_minutes', '30');

-- +goose Down
DELETE FROM system_settings WHERE key IN ('key_auto_recovery_enabled', 'key_auto_recovery_interval_minutes');
