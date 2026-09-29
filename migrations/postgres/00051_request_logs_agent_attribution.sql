-- The coding-agent attribution of a request: which tool sent it and the
-- tool's own session id, promoted from the captured header snapshot to
-- queryable columns so logs can be filtered and grouped by calling tool.
-- agent_client holds the normalized tool name (claude-code, codex, ...)
-- recognized from the caller's signature — dedicated x- headers first,
-- User-Agent prefixes as the fallback; agent_session_id holds the value
-- from the session-header chain when a tool was recognized.
--
-- NULL on every row whose caller carried no recognizable signature: the
-- gateway never guesses a tool, so a non-NULL value always means "this
-- caller's tool identified itself". Pre-column rows read NULL too — the
-- promotion of recognizable history happens in a separate idempotent task
-- reading the stored snapshots, and it can only ever fill agent_client:
-- the session-header values in pre-allowlist snapshots were masked before
-- capture and cannot be recovered (NULL is kept, never a redaction
-- sentinel). Column semantics are shared with the sqlite twin.

-- +goose Up
ALTER TABLE request_logs ADD COLUMN agent_client TEXT NULL;
ALTER TABLE request_logs ADD COLUMN agent_session_id TEXT NULL;

-- +goose Down
ALTER TABLE request_logs DROP COLUMN IF EXISTS agent_session_id;
ALTER TABLE request_logs DROP COLUMN IF EXISTS agent_client;
