-- migrations/postgres/00038_oauth_dingtalk_knobs.sql
--
-- DingTalk protocol knobs for oauth_providers; see the sqlite twin for the
-- column semantics (custom userinfo auth header, PKCE toggle, extra
-- authorize parameters).

-- +goose Up
ALTER TABLE oauth_providers ADD COLUMN userinfo_token_header TEXT NULL;
ALTER TABLE oauth_providers ADD COLUMN pkce_enabled BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE oauth_providers ADD COLUMN extra_authorize_params TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE oauth_providers DROP COLUMN IF EXISTS userinfo_token_header;
ALTER TABLE oauth_providers DROP COLUMN IF EXISTS pkce_enabled;
ALTER TABLE oauth_providers DROP COLUMN IF EXISTS extra_authorize_params;
