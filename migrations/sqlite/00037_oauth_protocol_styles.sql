-- migrations/sqlite/00037_oauth_protocol_styles.sql
--
-- Two orthogonal columns absorb the token-endpoint protocol differences of
-- non-standard IdPs as data instead of code: how the exchange request
-- body is encoded (standard providers take form-encoded; Feishu and
-- DingTalk take JSON), and how those JSON fields are named (snake_case for
-- Feishu, camelCase for DingTalk — the camel style also implies DingTalk's
-- trimmed field set). Defaults preserve the behavior of every existing
-- provider, so new columns are inert until configured.

-- +goose Up
ALTER TABLE oauth_providers ADD COLUMN token_request_style TEXT NOT NULL DEFAULT 'form';
ALTER TABLE oauth_providers ADD COLUMN token_field_style TEXT NOT NULL DEFAULT 'snake';

-- +goose Down
ALTER TABLE oauth_providers DROP COLUMN token_request_style;
ALTER TABLE oauth_providers DROP COLUMN token_field_style;
