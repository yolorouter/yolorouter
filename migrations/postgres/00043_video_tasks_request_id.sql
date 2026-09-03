-- migrations/postgres/00043_video_tasks_request_id.sql
--
-- request_id on video_tasks names the request_logs row the submit request
-- wrote, so settlement can back-fill that row's cost_micros minutes or
-- days later — the join the two tables previously lacked, with the
-- linkage carried only inside the logged response body. The column earns
-- its place now that the video bill has to reach the same analytics the
-- per-request bills already feed: without it, a completed task's charge
-- lands in api_keys.budget_spent_micros and nowhere a dashboard reads.
--
-- NOT NULL DEFAULT '' keeps tasks created before this migration valid:
-- they settle without a projection, exactly as they always did.

-- +goose Up
ALTER TABLE video_tasks
    ADD COLUMN request_id TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_video_tasks_request_id ON video_tasks (request_id);

-- +goose Down
DROP INDEX IF EXISTS idx_video_tasks_request_id;

ALTER TABLE video_tasks
    DROP COLUMN IF EXISTS request_id;
