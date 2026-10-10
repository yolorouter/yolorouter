// Transcript composition for tool sessions: the read endpoint that turns one
// session's rows + captured bodies into the full replay payload the frontend
// renders in one request. This file is the assembly seam between the
// read paths the detail endpoint already owns (repository rows, batched body
// rows, 1 MiB inline caps, bodiesDir stream inlining) and the pure dedup pass
// in agent_session_replay.go — everything storage-shaped happens here, so the
// pass itself stays table-testable (its table tests sit in
// agent_session_replay_test.go) and this layer stays a fat-free mapping.
package requestlog

import (
	"go.uber.org/zap"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// AgentSessionTranscript is the whole replay in one payload: the session-level
// aggregate, the deduplicated system-prompt timeline, and one replay block
// per request in chronological order. Session reuses AgentSessionListItem
// verbatim — the replay header bar answers the same "what did this session
// do and what did it cost" question the session list row does, so the two
// must never drift into two shapes (or two numbers).
type AgentSessionTranscript struct {
	Session AgentSessionListItem       `json:"session"`
	System  []AgentSessionReplaySystem `json:"system"`
	Blocks  []AgentSessionReplayBlock  `json:"blocks"`
}

// GetAgentSessionTranscript returns the full replay transcript for one tool
// session: the aggregate, the system timeline, and the per-request blocks.
//
// The session aggregate is derived in memory from the SAME ordered rows the
// blocks are built from, mirroring the list endpoint's per-session GROUP BY
// (repository.agentSessionSelectSQL): success_count is the five-class
// taxonomy's success bucket via DeriveStatusClass, known_cost_micros sums
// cost_micros over cost_known=true rows only, unknown_cost_count counts the
// rest, and first/last_seen_at are the earliest/latest row timestamps — rows
// arrive (created_at, id) ASC, so those are the first and last row. The same
// sync duty DeriveStatusClass already carries applies: if the list SQL's
// buckets change, this derivation changes with them.
//
// Read-path conventions are the detail endpoint's: bodies come from the 1:1
// request_log_bodies rows (batched — one IN query for the whole session)
// under the shared inline cap with their truncation flags, and the stream
// capture is inlined from bodiesDir with the same containment and
// missing-file degradation (empty bodiesDir means stream is always empty —
// the replay then attributes those turns via inline/resend/none instead of
// erroring). A body-batch lookup failure degrades the same way the detail
// endpoint treats a missing body row — blocks keep their metadata and read
// as "not recorded" — rather than failing the whole transcript.
func (s *RequestLogService) GetAgentSessionTranscript(sessionID string) (*AgentSessionTranscript, error) {
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

	providerNames, err := repository.FindProviderNamesByIDs(s.db,
		repository.CollectPtrIDs(rows, func(r *model.RequestLog) *uint { return r.ProviderID }))
	if err != nil {
		return nil, err
	}

	requestIDs := make([]string, len(rows))
	for i := range rows {
		requestIDs[i] = rows[i].RequestID
	}
	bodyRows, err := repository.GetRequestLogBodiesByRequestIDs(s.db, requestIDs)
	if err != nil {
		logger.Warn("agent session transcript: fetch bodies failed",
			zap.String("agent_session_id", sessionID), zap.Error(err))
		bodyRows = map[string]*model.RequestLogBody{}
	}

	inputs := make([]AgentSessionReplayInput, 0, len(rows))
	summary := AgentSessionListItem{
		AgentSessionID: sessionID,
		// Attribution is the FIRST row's agent_client — rows are (created_at,
		// id) ASC, the same "earliest row names the session" rule the list
		// endpoint's first_row CTE applies.
		AgentClient:  derefString(rows[0].AgentClient),
		RequestCount: int64(len(rows)),
		FirstSeenAt:  rows[0].CreatedAt,
		LastSeenAt:   rows[len(rows)-1].CreatedAt,
	}
	for i := range rows {
		r := &rows[i]
		summary.InputTokens += int64(r.InputTokens)
		summary.OutputTokens += int64(r.OutputTokens)
		if r.CostKnown {
			summary.KnownCostMicros += r.CostMicros
		} else {
			summary.UnknownCostCount++
		}
		if DeriveStatusClass(r.StatusCode, r.FailReason) == repository.StatusSuccess {
			summary.SuccessCount++
		}

		in := AgentSessionReplayInput{
			RequestID:    r.RequestID,
			CreatedAt:    r.CreatedAt,
			ModelName:    r.ModelName,
			StatusClass:  DeriveStatusClass(r.StatusCode, r.FailReason),
			IsStream:     r.IsStream,
			InputTokens:  r.InputTokens,
			OutputTokens: r.OutputTokens,
			DurationMs:   r.DurationMs,
			ProviderName: lookupName(r.ProviderID, providerNames),
		}
		if b := bodyRows[r.RequestID]; b != nil {
			// Same inline-cap contract as the detail endpoint: the stored
			// column is the full text, the flag says the inline copy was cut
			// (len over the cap is exactly truncateInlineBody's cut
			// condition, so text and flag cannot disagree).
			in.RequestBody = truncateInlineBody(b.RequestBody)
			in.RequestBodyTruncated = len(b.RequestBody) > maxInlineBodyBytes
			in.ResponseBody = truncateInlineBody(b.ResponseBody)
			in.ResponseBodyTruncated = len(b.ResponseBody) > maxInlineBodyBytes
			stream, streamCapped := readStreamBodyInlineCapped(s.bodiesDir, b.StreamBodyPath)
			in.StreamBody = stream
			// Either cut — the read-side inline cap here, or the write-side
			// disk backstop the body row recorded — means the capture shown
			// is not the full original.
			in.StreamBodyTruncated = streamCapped || b.StreamBodyTruncated
		}
		inputs = append(inputs, in)
	}

	blocks, system := AssembleAgentSessionReplay(inputs)
	return &AgentSessionTranscript{
		Session: summary,
		System:  system,
		Blocks:  blocks,
	}, nil
}
