--
-- SQLite mirror of migrations/postgres/00055_outbound_proxy.sql.
--
-- Outbound proxy configuration, two layers. Globally: a proxy_mode /
-- proxy_url settings pair in system_settings (off | system | custom; the
-- URL is used by custom mode). Per provider: a proxy_mode ('' = inherit
-- the global setting, the column default so every existing row keeps its
-- behaviour, | off | custom) and proxy_url override pair on providers,
-- maintained from the provider edit dialog. The shipped global default is
-- off, so an existing deployment changes nothing on upgrade. Both
-- settings rows share one version (CAS on save, same contract as the
-- custom-system-prompt pair seeded by 00015).

-- +goose Up
INSERT INTO system_settings (key, value) VALUES
    ('proxy_mode', 'off'),
    ('proxy_url', '');

ALTER TABLE providers ADD COLUMN proxy_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE providers ADD COLUMN proxy_url TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE providers DROP COLUMN proxy_url;
ALTER TABLE providers DROP COLUMN proxy_mode;
DELETE FROM system_settings WHERE key IN ('proxy_mode', 'proxy_url');
