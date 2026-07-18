-- migrations/postgres/00002_create_admin_auth.sql
-- Mirrors migrations/sqlite/00002_create_admin_auth.sql for the same reasons (design doc §4).

-- +goose Up
CREATE TABLE admins (
    id                  BIGSERIAL PRIMARY KEY,
    username            VARCHAR(32) NOT NULL UNIQUE,
    password_hash       VARCHAR(255) NOT NULL,
    failed_login_count  INTEGER NOT NULL DEFAULT 0,
    locked_until        TIMESTAMPTZ NULL,
    -- Every row defaults to the same value under a UNIQUE constraint, so
    -- the database itself rejects a second row regardless of username —
    -- v0.1 is single-admin only (PRD §3.1), and an app-level "count
    -- admins, then insert" check alone is a check-then-act race under
    -- concurrent first-run setup requests (design doc §4 / §9).
    singleton_guard     SMALLINT NOT NULL DEFAULT 1 UNIQUE,
    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL
);

CREATE TABLE admin_sessions (
    id          VARCHAR(64) PRIMARY KEY,
    admin_id    BIGINT NOT NULL REFERENCES admins(id),
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_admin_sessions_admin_id ON admin_sessions(admin_id);

-- +goose Down
DROP TABLE admin_sessions;
DROP TABLE admins;
