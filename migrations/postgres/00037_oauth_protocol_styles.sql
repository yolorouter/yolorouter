-- migrations/postgres/00037_oauth_protocol_styles.sql
--
-- Token-endpoint protocol styles for oauth_providers; see the sqlite twin
-- for the column semantics (request-body encoding and JSON field naming,
-- defaults preserving the standard form + snake_case behavior).

-- +goose Up
ALTER TABLE oauth_providers ADD COLUMN token_request_style TEXT NOT NULL DEFAULT 'form';
ALTER TABLE oauth_providers ADD COLUMN token_field_style TEXT NOT NULL DEFAULT 'snake';

-- +goose Down
ALTER TABLE oauth_providers DROP COLUMN IF EXISTS token_request_style;
ALTER TABLE oauth_providers DROP COLUMN IF EXISTS token_field_style;
