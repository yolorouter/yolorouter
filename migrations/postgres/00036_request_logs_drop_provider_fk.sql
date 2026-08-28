-- migrations/postgres/00036_request_logs_drop_provider_fk.sql
--
-- Providers can now be deleted outright; request_logs rows are history that
-- must outlive the provider they point at. See the sqlite twin for the full
-- rationale. Postgres can drop the single constraint in place — the
-- constraint name is the default one generated for the inline REFERENCES
-- clause in 00007.

-- +goose Up
ALTER TABLE request_logs DROP CONSTRAINT request_logs_provider_id_fkey;

-- +goose Down
-- Fails if any row references a since-deleted provider — deliberate: a
-- downgrade is only defined for databases where no provider deletion ever
-- happened, because the old schema cannot represent an orphaned history row.
ALTER TABLE request_logs
    ADD CONSTRAINT request_logs_provider_id_fkey
    FOREIGN KEY (provider_id) REFERENCES providers(id);
