-- The persisted turn store for tool sessions: one row per replay "event"
-- (system prompt change, human input, tool call, tool result, injected
-- context, assistant-reply attribution, pending marker), written by the
-- request-log write path at the moment the request's audit rows land and
-- read by the replay transcript endpoints instead of re-assembling the
-- whole session on every view.
--
-- agent_session_id / request_id name the session and the request the event
-- belongs to; request_pk + request_created_at copy the request_logs row's
-- identity so a page of events orders by the SAME (created_at, id) key the
-- session read paths use, without a join. Supplemental rows (the resend
-- attribution a request writes for its predecessor) deliberately carry the
-- PREDECESSOR's ids — the row belongs to the turn it describes.
--
-- seq is the per-request event ordinal. A request's own batch occupies
-- [0, 1024); the resend supplement its successor may append occupies
-- [1024, 2048); the degraded pending marker sits at 2048. The disjoint
-- ranges keep commit-order races between the two writers from interleaving
-- a request's own events with its supplement, and UNIQUE(request_id, seq)
-- makes a duplicated batch (live write racing the startup backfill) fail
-- atomically instead of double-writing.
--
-- content holds the event's JSON payload: the message object for
-- user_input / tool_result / injected / tool_call rows, {ordinal, raw} for
-- system_prompt rows, {source[, raw]} for assistant_reply rows. Large
-- ORIGINALS are never copied here — inline replies and stream captures
-- stay in request_log_bodies / the capture files and are fetched per page
-- by reference. content_bytes is filled only on system_prompt rows: the
-- JSON-serialized size of the raw value, exactly the bytes the entry adds
-- to the /transcript/system response, so the metadata list needs no
-- content read. Column semantics are shared with the sqlite twin.

-- +goose Up
CREATE TABLE agent_session_turns (
    id                 BIGSERIAL PRIMARY KEY,
    agent_session_id   TEXT NOT NULL,
    request_id         TEXT NOT NULL,
    request_pk         BIGINT NOT NULL,
    request_created_at TIMESTAMPTZ NOT NULL,
    seq                BIGINT NOT NULL,
    event_type         TEXT NOT NULL,
    content            TEXT NOT NULL DEFAULT '',
    content_bytes      BIGINT NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX idx_agent_session_turns_request_seq ON agent_session_turns(request_id, seq);
CREATE INDEX idx_agent_session_turns_session_order ON agent_session_turns(agent_session_id, request_created_at, request_pk, seq);

-- +goose Down
DROP INDEX IF EXISTS idx_agent_session_turns_session_order;
DROP INDEX IF EXISTS idx_agent_session_turns_request_seq;
DROP TABLE IF EXISTS agent_session_turns;
