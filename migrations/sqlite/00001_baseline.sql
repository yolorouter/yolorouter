-- migrations/sqlite/00001_baseline.sql
-- There are no business tables yet; this is a no-op placeholder migration
-- that exists solely to satisfy the //go:embed sqlite/*.sql wildcard in
-- migrations/embed.go, which must match at least one file to compile. Real
-- business-table migrations are appended after this one.

-- +goose Up

-- +goose Down
