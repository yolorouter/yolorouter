-- migrations/sqlite/00038_oauth_dingtalk_knobs.sql
--
-- Three more orthogonal protocol knobs for oauth_providers, completing the
-- DingTalk absorption: the header name userinfo is authorized
-- with (NULL = the standard Authorization: Bearer; DingTalk uses
-- x-acs-dingtalk-access-token), whether the PKCE challenge/verifier pair
-- is used at all (DingTalk's authorize endpoint has no code_challenge
-- parameter; default on preserves every existing provider), and extra
-- parameters appended to the authorization URL (DingTalk requires
-- prompt=consent; reserved keys are rejected at write time).

-- +goose Up
ALTER TABLE oauth_providers ADD COLUMN userinfo_token_header TEXT;
ALTER TABLE oauth_providers ADD COLUMN pkce_enabled BOOLEAN NOT NULL DEFAULT 1;
ALTER TABLE oauth_providers ADD COLUMN extra_authorize_params TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE oauth_providers DROP COLUMN userinfo_token_header;
ALTER TABLE oauth_providers DROP COLUMN pkce_enabled;
ALTER TABLE oauth_providers DROP COLUMN extra_authorize_params;
