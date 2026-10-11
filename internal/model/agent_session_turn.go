// AgentSessionTurn — one replay event of one tool session's persisted
// turn store. Schema lives in
// migrations/{sqlite,postgres}/00058_agent_session_turns.sql — goose owns
// DDL, GORM here is query-only (no AutoMigrate), mirroring RequestLog.
package model

import "time"

// AgentSessionTurn is one row of agent_session_turns: the write-time
// classification of what one request newly contributed to its session's
// conversation (a system-prompt change, a human input, a tool result, ...),
// or the degraded pending marker when that classification could not be
// written. The replay transcript endpoints read these rows instead of
// re-assembling the session on every view; the event taxonomy and content
// encoding live with the write engine in internal/agentturns.
type AgentSessionTurn struct {
	ID uint `gorm:"column:id;primaryKey" json:"id"`
	// AgentSessionID / RequestID name the session and the request the event
	// belongs to. For the resend supplement request i+1 writes on behalf of
	// request i, both columns carry request i's values — the row belongs to
	// the turn it describes, not the turn whose body carried the copies.
	AgentSessionID string `gorm:"column:agent_session_id" json:"agent_session_id"`
	RequestID      string `gorm:"column:request_id" json:"request_id"`
	// RequestPK / RequestCreatedAt copy the request_logs row's identity so
	// a page of events orders by the same (created_at, id) key every other
	// session read uses, without a join.
	RequestPK        uint      `gorm:"column:request_pk" json:"request_pk"`
	RequestCreatedAt time.Time `gorm:"column:request_created_at" json:"request_created_at"`
	// Seq is the per-request event ordinal: the request's own batch in
	// [0,1024), the resend supplement its successor appends in [1024,2048),
	// the pending marker at 2048. See the migration header for why the
	// ranges are disjoint.
	Seq int64 `gorm:"column:seq" json:"seq"`
	// EventType is the event taxonomy: system_prompt, user_input,
	// tool_call, tool_result, injected, assistant_reply, or pending. The
	// constants live in internal/agentturns (the package that writes and
	// reads them); a string column, not an enum, so the taxonomy can grow
	// without a migration per addition.
	EventType string `gorm:"column:event_type" json:"event_type"`
	// Content is the event's JSON payload (the message object, the system
	// {ordinal, raw} envelope, the assistant {source[, raw]} envelope).
	// Empty on pending rows and rows whose payload lives elsewhere: inline
	// and stream replies are fetched from request_log_bodies / the capture
	// files per page, by reference, never copied here.
	Content string `gorm:"column:content" json:"content"`
	// ContentBytes is filled only on system_prompt rows: the
	// JSON-serialized size of the raw value — exactly the bytes the entry
	// adds to the /transcript/system response — so the transcript's
	// metadata list can report raw_length without reading content. Zero
	// on every other row.
	ContentBytes int64 `gorm:"column:content_bytes" json:"content_bytes"`
}

func (AgentSessionTurn) TableName() string { return "agent_session_turns" }
