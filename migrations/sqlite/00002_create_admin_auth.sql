-- migrations/sqlite/00002_create_admin_auth.sql
-- First-run setup + admin login

-- +goose Up
CREATE TABLE admins (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    username            VARCHAR(32) NOT NULL UNIQUE,
    password_hash       VARCHAR(255) NOT NULL,
    failed_login_count  INTEGER NOT NULL DEFAULT 0,
    locked_until        DATETIME NULL,
    -- Every row defaults to the same value under a UNIQUE constraint, so
    -- the database itself rejects a second row regardless of username —
    -- v0.1 is single-admin only, and an app-level "count
    -- admins, then insert" check alone is a check-then-act race under
    -- concurrent first-run setup requests.
    singleton_guard     SMALLINT NOT NULL DEFAULT 1 UNIQUE,
    created_at          DATETIME NOT NULL,
    updated_at          DATETIME NOT NULL
);

CREATE TABLE admin_sessions (
    id          VARCHAR(64) PRIMARY KEY,
    admin_id    INTEGER NOT NULL REFERENCES admins(id),
    expires_at  DATETIME NOT NULL,
    created_at  DATETIME NOT NULL
);

CREATE INDEX idx_admin_sessions_admin_id ON admin_sessions(admin_id);

-- +goose Down
DROP TABLE admin_sessions;
DROP TABLE admins;
