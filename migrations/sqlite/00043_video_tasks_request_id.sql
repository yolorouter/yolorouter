-- migrations/sqlite/00043_video_tasks_request_id.sql
--
-- SQLite mirror of migrations/postgres/00043_video_tasks_request_id.sql:
-- request_id on video_tasks names the request_logs row the submit wrote,
-- so settlement can back-fill its cost. Empty on pre-column tasks — they
-- settle without a projection, exactly as before.

-- +goose Up
ALTER TABLE video_tasks
    ADD COLUMN request_id TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_video_tasks_request_id ON video_tasks (request_id);

-- +goose Down
DROP INDEX IF EXISTS idx_video_tasks_request_id;

ALTER TABLE video_tasks DROP COLUMN request_id;
