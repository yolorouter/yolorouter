-- migrations/sqlite/00042_video_tasks.sql
--
-- SQLite mirror of migrations/postgres/00042_video_tasks.sql (TEXT stands
-- in for the twin's wider columns; the payloads are the same strings).
--
-- The video task domain: the durable half of a POST /v1/videos create
-- call. The submit request ends the moment it returns a job id; status,
-- results, and settlement all hang off this row afterwards, so it carries
-- its own snapshots (model name, provider model name, destination
-- version) rather than joining rows an edit could change under it. Rows
-- are never deleted — the row is the billing evidence, in the spirit
-- request_logs rows are kept forever; the reaper only moves non-terminal
-- rows to 'expired' once the upstream window has closed.
--
-- video_pricing_tiers on model_candidates is the per-second price table
-- a video-mode candidate requires, stored in the JSON shape the shared
-- admin frontend's editor produces.

-- +goose Up
CREATE TABLE video_tasks (
    id                     TEXT PRIMARY KEY,
    api_key_id             INTEGER NOT NULL,
    model_id               INTEGER NOT NULL,
    model_name             TEXT NOT NULL DEFAULT '',
    candidate_id           INTEGER NOT NULL,
    provider_id            INTEGER NOT NULL,
    provider_model_name    TEXT NOT NULL DEFAULT '',
    provider_task_id       TEXT NOT NULL DEFAULT '',
    destination_version    INTEGER NOT NULL DEFAULT 1,
    status                 TEXT NOT NULL DEFAULT 'pending',
    error_code             TEXT NOT NULL DEFAULT '',
    error_message          TEXT NOT NULL DEFAULT '',
    request_snapshot       TEXT NOT NULL DEFAULT '',
    size                   TEXT NOT NULL DEFAULT '',
    seconds                INTEGER NOT NULL DEFAULT 0,
    result_url             TEXT NOT NULL DEFAULT '',
    cover_url              TEXT NOT NULL DEFAULT '',
    usage_seconds          INTEGER NOT NULL DEFAULT 0,
    estimated_micros       INTEGER NOT NULL DEFAULT 0,
    billed                 BOOLEAN NOT NULL DEFAULT 0,
    billed_micros          INTEGER NOT NULL DEFAULT 0,
    expires_at             DATETIME NULL,
    last_polled_at         DATETIME NULL,
    upstream_submitted_at  DATETIME NOT NULL,
    upstream_completed_at  DATETIME NULL,
    created_at             DATETIME NOT NULL,
    updated_at             DATETIME NOT NULL
);
CREATE INDEX idx_video_tasks_api_key_id ON video_tasks(api_key_id);
CREATE INDEX idx_video_tasks_status ON video_tasks(status);
CREATE INDEX idx_video_tasks_expires_at ON video_tasks(expires_at);
CREATE INDEX idx_video_tasks_provider_id ON video_tasks(provider_id);

ALTER TABLE model_candidates ADD COLUMN video_pricing_tiers TEXT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_video_tasks_provider_id;
DROP INDEX IF EXISTS idx_video_tasks_expires_at;
DROP INDEX IF EXISTS idx_video_tasks_status;
DROP INDEX IF EXISTS idx_video_tasks_api_key_id;
DROP TABLE IF EXISTS video_tasks;
ALTER TABLE model_candidates DROP COLUMN video_pricing_tiers;
