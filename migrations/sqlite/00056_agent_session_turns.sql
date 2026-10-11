-- migrations/sqlite/00056_agent_session_turns.sql
--
-- SQLite mirror of migrations/postgres/00056_agent_session_turns.sql (TEXT
-- stands in for the twin's wider columns; the payloads are the same
-- strings). See the postgres twin's header note for the full column
-- contract.

-- +goose Up
CREATE TABLE agent_session_turns (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_session_id   TEXT NOT NULL,
    request_id         TEXT NOT NULL,
    request_pk         INTEGER NOT NULL,
    request_created_at DATETIME NOT NULL,
    seq                INTEGER NOT NULL,
    event_type         TEXT NOT NULL,
    content            TEXT NOT NULL DEFAULT '',
    content_bytes      INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX idx_agent_session_turns_request_seq ON agent_session_turns(request_id, seq);
CREATE INDEX idx_agent_session_turns_session_order ON agent_session_turns(agent_session_id, request_created_at, request_pk, seq);

-- +goose Down
DROP INDEX IF EXISTS idx_agent_session_turns_session_order;
DROP INDEX IF EXISTS idx_agent_session_turns_request_seq;
DROP TABLE IF EXISTS agent_session_turns;
