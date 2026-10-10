// Tool-session aggregate queries over request_logs. A "tool session" here is
// the calling tool's own task marker (the agent_session_id column, promoted
// from the session-header chain) — NOT a gateway login session. This file is
// the pure query layer for the tool-session view: it aggregates existing rows
// (one group per agent_session_id), it never writes, and it introduces no new
// table, migration, or column.
package repository

import (
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
)

// AgentSessionFilter is the query shape for the tool-session list. Rows with
// a NULL agent_session_id are excluded unconditionally (the view shows real
// sessions only — no pseudo-grouping of header-less traffic); every other
// dimension is optional with the same zero-value convention as
// RequestLogFilter.
//
// AgentClient filters by SESSION ATTRIBUTION, not by row: a session belongs
// to the tool named on its first row (earliest created_at — see
// ListAgentSessions), so the filter selects sessions whose first row carries
// that tool while the aggregates still cover ALL of the session's rows. A
// session that merely CONTAINS a row from the filtered tool is not selected
// by that filter. Pointer-shaped for the same reason as RequestLogFilter's
// AgentClient: nil means "filter off" while a pointer to "" is a real
// constraint that matches nothing (attribution is never the empty string for
// a real session — a session row always carries a recognized tool).
type AgentSessionFilter struct {
	AgentClient *string
	Page        int
	PageSize    int
}

// AgentSessionAggregate is one row of the tool-session list: everything the
// "what did this session do and what did it cost" question needs, computed
// in one pass per agent_session_id.
//
// Cost follows the known/unknown doublet the request-log layer already uses
// (AggregateRequestLogMetrics): KnownCostMicros sums cost_micros over
// cost_known=true rows ONLY — the CASE guard keeps a hand-mangled
// cost_known=false row with a nonzero cost_micros out of the total — while
// UnknownCostCount says how many rows could not be priced at all, so the UI
// can render "sum, plus N rows of unknown cost" instead of a silently wrong
// total.
//
// SuccessCount is the success bucket of the five-class status taxonomy and
// nothing else (no new classification): 2xx with no fail_reason. The full
// per-row distribution stays with the session detail's individual rows.
type AgentSessionAggregate struct {
	AgentSessionID   string
	AgentClient      string
	RequestCount     int64
	SuccessCount     int64
	InputTokens      int64
	OutputTokens     int64
	KnownCostMicros  int64
	UnknownCostCount int64
	FirstSeenAt      time.Time
	LastSeenAt       time.Time
}

// agentSessionFirstRowCTE marks each session's earliest row: ROW_NUMBER()
// over (created_at, id) — deterministic because id is unique — and rn = 1
// is the row whose agent_client becomes the session's attribution. A window
// function (rather than a bare group column or a MIN(created_at) self-join)
// is what both SQLite and Postgres support for "the value from the first
// row" without dialect tricks. Shared by the list query and the count
// query so the two cannot drift on what a "session of tool X" is.
const agentSessionFirstRowCTE = `
first_row AS (
	SELECT
		agent_session_id,
		agent_client,
		ROW_NUMBER() OVER (PARTITION BY agent_session_id ORDER BY created_at ASC, id ASC) AS rn
	FROM request_logs
	WHERE agent_session_id IS NOT NULL
)`

// agentSessionSelectSQL is the list query. Two CTEs keep the two concerns
// apart:
//
//   - agg: the per-session GROUP BY over every session row (a bare
//     agent_client here would be an arbitrary/unspecified group member in
//     SQL, so attribution is NOT picked in this CTE);
//   - first_row: the shared attribution CTE above, joined on rn = 1 to
//     attach the first row's agent_client to the aggregate.
//
// The agent_client filter lands on the join result (attribution), keeping
// aggregates whole for mixed-tool sessions. Pagination's total comes from
// the sibling CountAgentSessions query — a COUNT(*) OVER () column would
// vanish on an empty page (a page past the end carries no row to read it
// from) and report the total as 0 exactly when the UI needs it most.
const agentSessionSelectSQL = `
WITH agg AS (
	SELECT
		agent_session_id,
		COUNT(*) AS request_count,
		SUM(CASE WHEN status_code >= 200 AND status_code < 300 AND (fail_reason IS NULL OR fail_reason = '') THEN 1 ELSE 0 END) AS success_count,
		COALESCE(SUM(input_tokens), 0) AS input_tokens,
		COALESCE(SUM(output_tokens), 0) AS output_tokens,
		COALESCE(SUM(CASE WHEN cost_known = ? THEN cost_micros ELSE 0 END), 0) AS known_cost_micros,
		SUM(CASE WHEN cost_known = ? THEN 1 ELSE 0 END) AS unknown_cost_count,
		MIN(created_at) AS first_seen_at,
		MAX(created_at) AS last_seen_at
	FROM request_logs
	WHERE agent_session_id IS NOT NULL
	GROUP BY agent_session_id
),
` + agentSessionFirstRowCTE + `
SELECT
	agg.agent_session_id AS agent_session_id,
	first_row.agent_client AS agent_client,
	agg.request_count AS request_count,
	agg.success_count AS success_count,
	agg.input_tokens AS input_tokens,
	agg.output_tokens AS output_tokens,
	agg.known_cost_micros AS known_cost_micros,
	agg.unknown_cost_count AS unknown_cost_count,
	agg.first_seen_at AS first_seen_at,
	agg.last_seen_at AS last_seen_at
FROM agg
JOIN first_row ON first_row.agent_session_id = agg.agent_session_id AND first_row.rn = 1`

// CountAgentSessions returns the total session count matching the filter
// (ignores Page/PageSize), so a page past the end still knows the real
// total. Mirrors CountRequestLogs's role for the request-log list.
func CountAgentSessions(db *gorm.DB, f *AgentSessionFilter) (int64, error) {
	sql := "WITH " + agentSessionFirstRowCTE + " SELECT COUNT(*) FROM first_row WHERE rn = 1"
	args := []interface{}{}
	if f.AgentClient != nil {
		sql += " AND agent_client = ?"
		args = append(args, *f.AgentClient)
	}
	var total int64
	if err := db.Raw(sql, args...).Scan(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

// ListAgentSessions returns one page of session aggregates plus the total
// session count for the filter. Ordering is most-recent-activity-first
// (MAX(created_at) DESC, the request-log list's "newest first" convention
// applied to a session's last row), with agent_session_id ascending as the
// deterministic tie-break. Page/PageSize default to 1/20 and clamp to 1..200,
// the same contract as ListRequestLogs.
func ListAgentSessions(db *gorm.DB, f *AgentSessionFilter) ([]AgentSessionAggregate, int64, error) {
	page := f.Page
	if page < 1 {
		page = 1
	}
	pageSize := f.PageSize
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 200 {
		pageSize = 200
	}

	sql := agentSessionSelectSQL
	args := []interface{}{true, false}
	if f.AgentClient != nil {
		sql += " WHERE first_row.agent_client = ?"
		args = append(args, *f.AgentClient)
	}
	sql += " ORDER BY agg.last_seen_at DESC, agg.agent_session_id ASC LIMIT ? OFFSET ?"
	args = append(args, pageSize, (page-1)*pageSize)

	var rows []struct {
		AgentSessionID   string  `gorm:"column:agent_session_id"`
		AgentClient      *string `gorm:"column:agent_client"`
		RequestCount     int64   `gorm:"column:request_count"`
		SuccessCount     int64   `gorm:"column:success_count"`
		InputTokens      int64   `gorm:"column:input_tokens"`
		OutputTokens     int64   `gorm:"column:output_tokens"`
		KnownCostMicros  int64   `gorm:"column:known_cost_micros"`
		UnknownCostCount int64   `gorm:"column:unknown_cost_count"`
		// Computed timestamp columns arrive as driver strings (SQLite's
		// MIN/MAX over the stored datetimes renders "2006-01-02
		// 15:04:05+00:00"; database/sql stringifies a Postgres timestamptz
		// as the same shape with an optional fraction), and the raw-struct
		// scan path does not apply the model schema's time parsing — so
		// they are scanned as strings and parsed below, the same contract
		// the analytics daily/hourly buckets use for their date columns.
		FirstSeenAt string `gorm:"column:first_seen_at"`
		LastSeenAt  string `gorm:"column:last_seen_at"`
	}
	if err := db.Raw(sql, args...).Scan(&rows).Error; err != nil {
		return nil, 0, err
	}
	total, err := CountAgentSessions(db, f)
	if err != nil {
		return nil, 0, err
	}
	sessions := make([]AgentSessionAggregate, 0, len(rows))
	for i := range rows {
		firstSeen, err := parseSQLTimestamp(rows[i].FirstSeenAt)
		if err != nil {
			return nil, 0, fmt.Errorf("agent session %s first_seen_at: %w", rows[i].AgentSessionID, err)
		}
		lastSeen, err := parseSQLTimestamp(rows[i].LastSeenAt)
		if err != nil {
			return nil, 0, fmt.Errorf("agent session %s last_seen_at: %w", rows[i].AgentSessionID, err)
		}
		sessions = append(sessions, AgentSessionAggregate{
			AgentSessionID:   rows[i].AgentSessionID,
			AgentClient:      derefAgentClient(rows[i].AgentClient),
			RequestCount:     rows[i].RequestCount,
			SuccessCount:     rows[i].SuccessCount,
			InputTokens:      rows[i].InputTokens,
			OutputTokens:     rows[i].OutputTokens,
			KnownCostMicros:  rows[i].KnownCostMicros,
			UnknownCostCount: rows[i].UnknownCostCount,
			FirstSeenAt:      firstSeen,
			LastSeenAt:       lastSeen,
		})
	}
	return sessions, total, nil
}

// parseSQLTimestamp parses the timestamp text the drivers hand back for
// computed datetime columns. Layouts, first match wins:
//
//   - "2006-01-02 15:04:05.999999999-07:00" — space-separated with a
//     numeric offset and an OPTIONAL fraction: SQLite's MIN/MAX rendering
//     ("... 10:00:00+00:00") and database/sql's time.Time→string
//     conversion for Postgres timestamptz both fit (the fraction layout
//     ".999999999" matches zero or fewer fractional digits).
//   - RFC3339 — the T-separated form some drivers render for stored
//     values, kept as the fallback so a driver swap degrades loudly here
//     rather than silently zeroing a timestamp.
//
// A value matching none of them is an error, never a zero time: a wrong
// session boundary is worse than a failed list request.
func parseSQLTimestamp(v string) (time.Time, error) {
	layouts := []string{
		"2006-01-02 15:04:05.999999999-07:00",
		time.RFC3339,
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, v); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp %q", v)
}

// derefAgentClient flattens the nullable attribution column to "". A real
// session's first row always carries a recognized tool (the recognizer never
// writes a session id without a client), so "" is unreachable in
// gateway-written data — the flattening exists so a hand-mangled row degrades
// to a displayable empty name instead of a nil pointer on the wire.
func derefAgentClient(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// ListRequestLogsByAgentSession returns EVERY row of one tool session in
// chronological order (created_at ASC, id ASC as the deterministic
// tie-break), so the session detail reads as the session's own timeline. No
// pagination: a session is a bounded unit by construction, and the detail
// view's value is completeness. An unknown session id yields an empty slice,
// which the service layer maps to its not-found error.
func ListRequestLogsByAgentSession(db *gorm.DB, sessionID string) ([]model.RequestLog, error) {
	var rows []model.RequestLog
	err := db.Model(&model.RequestLog{}).
		Where("agent_session_id = ?", sessionID).
		Order("created_at ASC, id ASC").
		Find(&rows).Error
	return rows, err
}
