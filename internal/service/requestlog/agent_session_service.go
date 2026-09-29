// Tool-session view composition: the list and detail endpoints the
// agent_session_id aggregate page renders. Same strict layering as the
// request-log views — the SQL lives in repository/agent_session_query.go,
// this layer owns the wire DTOs, the first-row attribution flattening, and
// reuse of toListItems so a session's rows serialize exactly like the
// request-log list rows the frontend already renders (five-class
// status_class included).
package requestlog

import (
	"time"

	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// AgentSessionListItem is one row of the tool-session list: the per-session
// aggregate the repository computed, with the nullable attribution column
// flattened to "" (the wire convention for "absent" — see derefString).
// Field semantics mirror repository.AgentSessionAggregate; the
// micros→major-currency conversion is a display concern and stays in the
// frontend.
type AgentSessionListItem struct {
	AgentSessionID string `json:"agent_session_id"`
	AgentClient    string `json:"agent_client"`
	RequestCount   int64  `json:"request_count"`
	// SuccessCount is the success bucket of the shared five-class status
	// taxonomy, nothing broader (no new classification).
	SuccessCount int64 `json:"success_count"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	// KnownCostMicros sums cost_micros over the session's cost_known=true
	// rows only; UnknownCostCount is how many rows could not be priced, so
	// the UI shows the sum plus an "N rows of unknown cost" marker instead
	// of a silently wrong total.
	KnownCostMicros  int64 `json:"known_cost_micros"`
	UnknownCostCount int64 `json:"unknown_cost_count"`
	// FirstSeenAt / LastSeenAt are the session's earliest and latest row
	// timestamps (MIN/MAX created_at), serialized RFC3339 by encoding/json's
	// time.Time marshaler like every other timestamp on the wire.
	FirstSeenAt time.Time `json:"first_seen_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}

// AgentSessionDetail is the session detail payload: the session's identity
// and attribution, plus EVERY request of the session in chronological order.
// Requests reuses RequestLogListItem verbatim — the session detail is a
// timeline of the same rows the log list shows, so the frontend renders one
// row shape and the :requestId jump target needs no translation.
type AgentSessionDetail struct {
	AgentSessionID string               `json:"agent_session_id"`
	AgentClient    string               `json:"agent_client"`
	Requests       []RequestLogListItem `json:"requests"`
}

// ListAgentSessions returns one page of session aggregates plus the total
// session count, converting the repository rows to the wire DTO. Pure
// passthrough otherwise — the aggregation semantics live in the repository
// where the SQL is.
func (s *RequestLogService) ListAgentSessions(filter *repository.AgentSessionFilter) ([]AgentSessionListItem, int64, error) {
	rows, total, err := repository.ListAgentSessions(s.db, filter)
	if err != nil {
		return nil, 0, err
	}
	items := make([]AgentSessionListItem, 0, len(rows))
	for i := range rows {
		items = append(items, AgentSessionListItem{
			AgentSessionID:   rows[i].AgentSessionID,
			AgentClient:      rows[i].AgentClient,
			RequestCount:     rows[i].RequestCount,
			SuccessCount:     rows[i].SuccessCount,
			InputTokens:      rows[i].InputTokens,
			OutputTokens:     rows[i].OutputTokens,
			KnownCostMicros:  rows[i].KnownCostMicros,
			UnknownCostCount: rows[i].UnknownCostCount,
			FirstSeenAt:      rows[i].FirstSeenAt,
			LastSeenAt:       rows[i].LastSeenAt,
		})
	}
	return items, total, nil
}

// GetAgentSessionDetail returns the session's identity, its attribution, and
// all of its requests in chronological order. Attribution is read off the
// FIRST row of the ordered result — the repository orders by (created_at,
// id) ASC, and the attribution rule is "the session's earliest row's
// agent_client", so rows[0] is that row by construction and no second query
// is needed. An empty session id or one that matches no rows maps to
// errcode.ErrAgentSessionNotFound, which the handler renders as a 404
// envelope.
func (s *RequestLogService) GetAgentSessionDetail(sessionID string) (*AgentSessionDetail, error) {
	if sessionID == "" {
		return nil, errcode.ErrAgentSessionNotFound
	}
	rows, err := repository.ListRequestLogsByAgentSession(s.db, sessionID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errcode.ErrAgentSessionNotFound
	}
	items, err := s.toListItems(rows)
	if err != nil {
		return nil, err
	}
	return &AgentSessionDetail{
		AgentSessionID: sessionID,
		AgentClient:    derefString(rows[0].AgentClient),
		Requests:       items,
	}, nil
}
