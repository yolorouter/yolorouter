// The turn store's storage engine: the live write path the gateway's
// request-log recorder calls once the audit rows land, and the read queries
// the replay transcript endpoints page through. Everything here is
// append-only — a request's own batch at write time, its successor's resend
// supplement right after, the pending marker when derivation fails — with
// UNIQUE(request_id, seq) as the structural guard that the live write and
// the startup backfill racing on the same request can never double-write
// (one batch wins atomically, the loser reads the violation as "already
// written").
package agentturns

import (
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
)

// RequestInput is what the recorder hands the write path: the identity of
// the request whose audit rows just landed (session, id, request_logs pk,
// created_at), the captured request body, and the two reply-capture
// presences off the body row. The reply TEXTS never enter the turn store —
// inline and stream replies are read from request_log_bodies and the
// capture files per page, by reference.
type RequestInput struct {
	SessionID        string
	RequestID        string
	RequestPK        uint
	CreatedAt        time.Time
	RequestBody      string
	HasInlineReply   bool
	HasStreamCapture bool
}

// prevRequest is the session's chronologically previous request as one
// joined read: the prefix-diff base body, whether its reply was recorded
// inline (the supplement guard), and the identity the supplement rows
// carry.
type prevRequest struct {
	RequestID        string
	RequestPK        uint
	RequestCreatedAt time.Time
	RequestBody      string
	HasInlineReply   bool
}

// RecordTurnsForRequest derives and persists one request's turn events.
// It is the audit write path's LAST step: a failure here is returned for
// logging and degrades to a pending marker row (or, if even that fails,
// to nothing) — the request_logs and request_log_bodies rows are already
// committed and must never be rolled back because the turn classification
// could not be stored. Idempotent under the UNIQUE(request_id, seq) guard:
// a batch that races the startup backfill onto an already-written request
// returns nil.
func RecordTurnsForRequest(db *gorm.DB, in RequestInput) error {
	if in.SessionID == "" {
		// Requests the recognizer never attributed to a tool session have
		// no turn store half — nothing to derive, nothing to write.
		return nil
	}
	e := &turnEngine{db: db}
	return e.writeForRequest(in)
}

// turnEngine is one write pass's bookkeeping. The read/derive seams exist
// as fields so the package's white-box tests can count the incremental
// reads (the previous body is fetched exactly once per write) and inject
// failures; production always runs the shipped functions.
type turnEngine struct {
	db *gorm.DB
	// fetchPrev reads the session's previous request (nil when this is the
	// first). One joined SELECT — the incremental cost the architecture
	// pays per request.
	fetchPrev func(sessionID string, before time.Time, beforePK uint) (*prevRequest, error)
	// fetchBaseline reads the session's last recorded system value and the
	// ordinal the next change takes. One indexed SELECT; skipped requests
	// (no system value) still pay it — it is microscopic next to the
	// previous body's parse.
	fetchBaseline func(sessionID string) (SystemBaseline, error)
	// derive is the pure derivation (DeriveTurnEvents); a field so tests
	// can force the degraded pending path.
	derive func(own OwnBody, prev *PrevBody, baseline SystemBaseline) []TurnEvent
}

// writeForRequest drives one request's write: fetch the diff base and
// system baseline, derive (with the panic guard — a derivation bug must
// degrade, not take the audit path down), insert the batch, and degrade to
// a pending marker when any step fails.
func (e *turnEngine) writeForRequest(in RequestInput) error {
	if e.fetchPrev == nil {
		e.fetchPrev = func(sessionID string, before time.Time, beforePK uint) (*prevRequest, error) {
			return fetchPrevRequest(e.db, sessionID, before, beforePK)
		}
	}
	if e.fetchBaseline == nil {
		e.fetchBaseline = func(sessionID string) (SystemBaseline, error) {
			return fetchSystemBaseline(e.db, sessionID)
		}
	}
	if e.derive == nil {
		e.derive = DeriveTurnEvents
	}

	prev, err := e.fetchPrev(in.SessionID, in.CreatedAt, in.RequestPK)
	if err != nil {
		return e.degradeToPending(in, fmt.Errorf("agent turns: read previous request: %w", err))
	}
	baseline, err := e.fetchBaseline(in.SessionID)
	if err != nil {
		return e.degradeToPending(in, fmt.Errorf("agent turns: read system baseline: %w", err))
	}

	own := OwnBody{
		RequestBody:      in.RequestBody,
		HasInlineReply:   in.HasInlineReply,
		HasStreamCapture: in.HasStreamCapture,
	}
	var diffBase *PrevBody
	if prev != nil {
		diffBase = &PrevBody{RequestBody: prev.RequestBody, HasInlineReply: prev.HasInlineReply}
	}

	events, derr := e.safeDerive(own, diffBase, baseline)
	if derr != nil {
		return e.degradeToPending(in, fmt.Errorf("agent turns: derive: %w", derr))
	}

	rows := make([]model.AgentSessionTurn, 0, len(events))
	for _, ev := range events {
		row := model.AgentSessionTurn{
			AgentSessionID:   in.SessionID,
			RequestID:        in.RequestID,
			RequestPK:        in.RequestPK,
			RequestCreatedAt: in.CreatedAt,
			Seq:              ev.Seq,
			EventType:        ev.EventType,
			Content:          ev.Content,
			ContentBytes:     ev.ContentBytes,
		}
		if ev.ForPrev {
			if prev == nil {
				continue // unreachable: supplements are only derived when prev exists
			}
			row.RequestID = prev.RequestID
			row.RequestPK = prev.RequestPK
			row.RequestCreatedAt = prev.RequestCreatedAt
		}
		rows = append(rows, row)
	}
	if err := e.db.Create(&rows).Error; err != nil {
		if repository.IsUniqueViolation(err) {
			// The startup backfill (or a retried finalize) already wrote
			// this request's batch under the same deterministic seq
			// assignment — the constraint did its job, there is nothing
			// left to write.
			return nil
		}
		return e.degradeToPending(in, fmt.Errorf("agent turns: insert: %w", err))
	}
	return nil
}

// safeDerive runs the pure derivation behind the panic guard. A panic in
// the derivation is a bug in this package; the guard turns it into the
// same degraded pending path any other failure takes, so the audit rows
// stay committed and the request stays a backfill candidate.
func (e *turnEngine) safeDerive(own OwnBody, prev *PrevBody, baseline SystemBaseline) (events []TurnEvent, err error) {
	defer func() {
		if r := recover(); r != nil {
			events = nil
			err = fmt.Errorf("derivation panicked: %v", r)
		}
	}()
	return e.derive(own, prev, baseline), nil
}

// degradeToPending writes the request's pending marker — the in-database
// note that the turn classification failed and the startup backfill should
// retry it — and returns the original cause for the caller's log. The
// marker insert itself is best-effort ON CONFLICT DO NOTHING (a second
// failure lands on the same row): when even the database refuses it, the
// request simply has no rows, which the backfill's candidate query treats
// exactly like a pending marker.
func (e *turnEngine) degradeToPending(in RequestInput, cause error) error {
	pending := model.AgentSessionTurn{
		AgentSessionID:   in.SessionID,
		RequestID:        in.RequestID,
		RequestPK:        in.RequestPK,
		RequestCreatedAt: in.CreatedAt,
		Seq:              PendingSeq,
		EventType:        EventPending,
	}
	if err := e.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "request_id"}, {Name: "seq"}},
		DoNothing: true,
	}).Create(&pending).Error; err != nil {
		return fmt.Errorf("agent turns: pending marker also failed: %w (cause: %w)", err, cause)
	}
	return cause
}

// fetchPrevRequest reads the session's chronologically previous request
// with its body row — the prefix-diff base and the supplement target in
// one joined SELECT, ordered by the same (created_at, id) key every other
// session read uses. Nil when the request is the session's first (or the
// previous row's audit trail vanished, which reads the same way).
func fetchPrevRequest(db *gorm.DB, sessionID string, before time.Time, beforePK uint) (*prevRequest, error) {
	// A raw query because the projection spans two tables and neither
	// model alone carries it.
	var rows []struct {
		RequestID        string    `gorm:"column:request_id"`
		RequestPK        uint      `gorm:"column:request_pk"`
		RequestCreatedAt time.Time `gorm:"column:request_created_at"`
		RequestBody      string    `gorm:"column:request_body"`
		ResponseBody     string    `gorm:"column:response_body"`
	}
	if err := db.Raw(`
		SELECT rl.request_id AS request_id,
		       rl.id AS request_pk,
		       rl.created_at AS request_created_at,
		       COALESCE(rb.request_body, '') AS request_body,
		       COALESCE(rb.response_body, '') AS response_body
		FROM request_logs rl
		LEFT JOIN request_log_bodies rb ON rb.request_id = rl.request_id
		WHERE rl.agent_session_id = ?
		  AND (rl.created_at < ? OR (rl.created_at = ? AND rl.id < ?))
		ORDER BY rl.created_at DESC, rl.id DESC
		LIMIT 1`,
		sessionID, before, before, beforePK).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &prevRequest{
		RequestID:        rows[0].RequestID,
		RequestPK:        rows[0].RequestPK,
		RequestCreatedAt: rows[0].RequestCreatedAt,
		RequestBody:      rows[0].RequestBody,
		HasInlineReply:   rows[0].ResponseBody != "",
	}, nil
}

// fetchSystemBaseline reads the session's last recorded system prompt —
// the value the next request's system is deduplicated against, and the
// ordinal source for the next change. A session with no recorded system
// value yet returns the zero baseline (first change takes ordinal 0).
func fetchSystemBaseline(db *gorm.DB, sessionID string) (SystemBaseline, error) {
	var rows []struct {
		Content string `gorm:"column:content"`
	}
	if err := db.Raw(`
		SELECT content FROM agent_session_turns
		WHERE agent_session_id = ? AND event_type = ?
		ORDER BY request_created_at DESC, request_pk DESC, seq DESC
		LIMIT 1`, sessionID, EventSystemPrompt).Scan(&rows).Error; err != nil {
		return SystemBaseline{}, err
	}
	if len(rows) == 0 {
		return SystemBaseline{}, nil
	}
	var envelope systemEventContent
	if err := json.Unmarshal([]byte(rows[0].Content), &envelope); err != nil {
		// An unreadable envelope means a corrupted row, not a missing
		// baseline: treat it as "nothing recorded" so the write path still
		// produces a (fresh) system row rather than skipping the timeline
		// update — re-recording a value is the recoverable direction.
		return SystemBaseline{}, nil
	}
	return SystemBaseline{Raw: envelope.Raw, Has: true, NextOrdinal: envelope.Ordinal + 1}, nil
}

// ── Read queries ─────────────────────────────────────────────────────────

// SystemTurnMeta is the metadata half of one system timeline entry —
// everything the transcript's per-page metadata list needs, without the
// raw prompt text (that ships only from the /transcript/system endpoint's
// full read).
type SystemTurnMeta struct {
	SeenAtRequest string
	ContentBytes  int64
}

// SystemTurnEntry is one full system timeline entry for the
// /transcript/system endpoint: the request the value was first seen on and
// the parsed raw value.
type SystemTurnEntry struct {
	SeenAtRequest string
	Raw           any
}

// ListTurnsByRequestIDs returns the turns of the named requests grouped by
// request id, each group in event order ((seq, id) ascending — own batch
// before the successor's supplement). Pending markers are included; the
// transcript reader skips them, the backfill reader keys on them.
func ListTurnsByRequestIDs(db *gorm.DB, requestIDs []string) (map[string][]model.AgentSessionTurn, error) {
	out := make(map[string][]model.AgentSessionTurn, len(requestIDs))
	if len(requestIDs) == 0 {
		return out, nil
	}
	var rows []model.AgentSessionTurn
	if err := db.Where("request_id IN ?", requestIDs).
		Order("seq ASC, id ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for i := range rows {
		out[rows[i].RequestID] = append(out[rows[i].RequestID], rows[i])
	}
	return out, nil
}

// ListSystemTurnMeta returns the session's system timeline metadata in
// timeline order — (request_created_at, request_pk, seq) ascending, the
// same order the whole-session pass appended entries in.
func ListSystemTurnMeta(db *gorm.DB, sessionID string) ([]SystemTurnMeta, error) {
	var rows []struct {
		RequestID    string `gorm:"column:request_id"`
		ContentBytes int64  `gorm:"column:content_bytes"`
	}
	if err := db.Model(&model.AgentSessionTurn{}).
		Select("request_id", "content_bytes").
		Where("agent_session_id = ? AND event_type = ?", sessionID, EventSystemPrompt).
		Order("request_created_at ASC, request_pk ASC, seq ASC").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	meta := make([]SystemTurnMeta, 0, len(rows))
	for i := range rows {
		meta = append(meta, SystemTurnMeta{SeenAtRequest: rows[i].RequestID, ContentBytes: rows[i].ContentBytes})
	}
	return meta, nil
}

// ListSystemTurnEntries returns the session's full system timeline with
// parsed raw values, index-aligned with ListSystemTurnMeta's output by
// construction (both walk the same rows in the same order).
func ListSystemTurnEntries(db *gorm.DB, sessionID string) ([]SystemTurnEntry, error) {
	var rows []model.AgentSessionTurn
	if err := db.Where("agent_session_id = ? AND event_type = ?", sessionID, EventSystemPrompt).
		Order("request_created_at ASC, request_pk ASC, seq ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	entries := make([]SystemTurnEntry, 0, len(rows))
	for i := range rows {
		var envelope systemEventContent
		if err := json.Unmarshal([]byte(rows[i].Content), &envelope); err != nil {
			return nil, fmt.Errorf("agent session %s: system turn %s: %w", sessionID, rows[i].RequestID, err)
		}
		entries = append(entries, SystemTurnEntry{SeenAtRequest: rows[i].RequestID, Raw: envelope.Raw})
	}
	return entries, nil
}
