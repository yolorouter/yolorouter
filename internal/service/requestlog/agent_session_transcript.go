// Transcript composition for tool sessions: the read endpoints that page
// one session's PERSISTED turn store (agent_session_turns, written by the
// request-log write path via internal/agentturns) into the replay payloads
// the frontend renders. This is the storage-rework read path: no
// whole-session assembly happens at view time — the session's request rows
// give the block index and the aggregate, the turn rows give each block's
// added messages and reply attribution, and the reply ORIGINALS (inline
// response bodies, stream captures) are fetched per page by reference from
// request_log_bodies and the capture files, under the same inline caps the
// detail endpoint holds. The whole-session pass in internal/agentturns
// remains as the specification the write path is parity-tested against;
// nothing here calls it.
//
// The output splits in two over the same store: the transcript endpoint
// returns the aggregate + system METADATA + one cursor-paginated page of
// blocks (sliced by agent_session_transcript_page.go over the lazy
// per-request materializer in this file), and the /transcript/system
// endpoint returns the system timeline's raw text on demand.
package requestlog

import (
	"encoding/json"

	"go.uber.org/zap"

	"github.com/yolorouter/yolorouter/internal/agentturns"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// Type aliases over the replay vocabulary internal/agentturns now owns
// (moved there when the write path and the read path started sharing it).
// They keep this package's historical names compiling — the wire DTOs, the
// handler layer, and the tests all speak them — while the single
// definition lives with the assembly semantics.
type (
	AgentSessionReplayInput     = agentturns.AgentSessionReplayInput
	AgentSessionReplayAssistant = agentturns.AgentSessionReplayAssistant
	AgentSessionReplayBlock     = agentturns.AgentSessionReplayBlock
	AgentSessionReplaySystem    = agentturns.AgentSessionReplaySystem
)

// Replay-source tags, aliased for the same reason as the types above.
const (
	ReplaySourceInline = agentturns.ReplaySourceInline
	ReplaySourceStream = agentturns.ReplaySourceStream
	ReplaySourceResend = agentturns.ReplaySourceResend
	ReplaySourceNone   = agentturns.ReplaySourceNone
)

// AssembleAgentSessionReplay is the whole-session pass, aliased for the
// same reason as the types above. The read path never calls it; it stays
// reachable from here as the parity oracle the storage rework's tests (and
// any future one) diff the turns-backed read against.
var AssembleAgentSessionReplay = agentturns.AssembleAgentSessionReplay

// AgentSessionReplaySystemMeta is the metadata-only half of one system
// timeline entry, what the transcript response ships so the first screen
// carries no raw system-prompt text (a long session can hold dozens of
// kilobytes × N of it). RawLength is the raw value's JSON serialization size
// in UTF-8 bytes — exactly the bytes the entry adds to the
// /transcript/system response — so the frontend can budget the on-expand
// fetch. It is served from the system_prompt turn row's content_bytes
// sidecar, measured at write time by the same rule.
type AgentSessionReplaySystemMeta struct {
	SeenAtRequest string `json:"seen_at_request"`
	Truncated     bool   `json:"truncated"`
	RawLength     int    `json:"raw_length"`
}

// AgentSessionTranscript is the cursor-paginated transcript payload: the
// aggregate and system timeline METADATA stay whole on every page (the header
// bar's numbers are session-wide by contract, the system raw text is fetched
// separately on expand), while Blocks is one cursor-paginated slice carrying
// the direction-wise continuation metadata. Session reuses
// AgentSessionListItem verbatim — the replay header bar answers the same
// "what did this session do and what did it cost" question the session list
// row does, so the two must never drift into two shapes (or two numbers).
type AgentSessionTranscript struct {
	Session AgentSessionListItem           `json:"session"`
	System  []AgentSessionReplaySystemMeta `json:"system"`
	Blocks  []AgentSessionReplayBlock      `json:"blocks"`
	// HasMoreBefore/NextBefore: whether older blocks exist beyond this page,
	// and the upscroll cursor (the page's oldest block id, exclusive).
	HasMoreBefore bool   `json:"has_more_before"`
	NextBefore    string `json:"next_before"`
	// HasMoreAfter/NextAfter: whether newer blocks exist beyond this page,
	// and the toward-newer cursor — normally the page's newest block id
	// (inclusive: the anchor entry and the continuation share the after
	// parameter). When a page carries nothing beyond its anchor block (the
	// limit or byte budget stopped the walk there), the cursor names the
	// first block beyond the page instead, so continuing always advances —
	// re-anchoring on such a page's own newest block would return that one
	// block forever.
	HasMoreAfter bool   `json:"has_more_after"`
	NextAfter    string `json:"next_after"`
}

// AgentSessionTranscriptSystem is the /transcript/system payload: the full
// deduplicated system-prompt timeline with its raw values, fetched when the
// collapsed system group is expanded. Index-aligned with the transcript's
// System metadata list by construction — both walk the session's
// system_prompt turn rows in the same order.
type AgentSessionTranscriptSystem struct {
	System []AgentSessionReplaySystem `json:"system"`
}

// GetAgentSessionTranscript returns one cursor-paginated page of the replay
// transcript for a tool session (see AgentSessionTranscriptPage for the
// cursor semantics and sliceTranscriptBlocks for the page caps). The block
// sequence is one block per request row — the request rows are read whole
// (narrow columns, the same read the session detail does) so the block
// index, the session aggregate, and the direction-wise has_more flags are
// session-wide on every page, while each block's ADDED messages and reply
// attribution materialize from the turn rows only for the blocks the walk
// actually takes, and the reply originals (inline bodies, stream captures)
// are fetched per page by reference.
//
// The session aggregate is derived in memory from the same ordered rows,
// mirroring the list endpoint's per-session GROUP BY
// (repository.agentSessionSelectSQL): success_count is the five-class
// taxonomy's success bucket via DeriveStatusClass, known_cost_micros sums
// cost_micros over cost_known=true rows only, unknown_cost_count counts the
// rest, and first/last_seen_at are the earliest/latest row timestamps — rows
// arrive (created_at, id) ASC, so those are the first and last row. The same
// sync duty DeriveStatusClass already carries applies: if the list SQL's
// buckets change, this derivation changes with them.
//
// Read-path conventions are the detail endpoint's: bodies come from the 1:1
// request_log_bodies rows under the shared inline cap with their truncation
// flags, and the stream capture is inlined from bodiesDir with the same
// containment and missing-file degradation (empty bodiesDir means stream is
// always empty — the replay then attributes those turns via inline/resend/
// none instead of erroring). A body-batch or turn lookup failure degrades
// the same way the detail endpoint treats a missing body row — blocks keep
// their metadata and read as "not recorded" — rather than failing the whole
// transcript.
func (s *RequestLogService) GetAgentSessionTranscript(sessionID string, page AgentSessionTranscriptPage) (*AgentSessionTranscript, error) {
	rows, err := s.loadSessionRows(sessionID)
	if err != nil {
		return nil, err
	}
	systemMeta, err := s.loadSystemMeta(sessionID)
	if err != nil {
		return nil, err
	}
	seq, err := s.turnBlockSequence(sessionID, rows)
	if err != nil {
		return nil, err
	}
	sliced := sliceTranscriptBlocks(seq, page)
	return &AgentSessionTranscript{
		Session:       agentSessionSummaryFromRows(sessionID, rows),
		System:        systemMeta,
		Blocks:        sliced.Blocks,
		HasMoreBefore: sliced.HasMoreBefore,
		NextBefore:    sliced.NextBefore,
		HasMoreAfter:  sliced.HasMoreAfter,
		NextAfter:     sliced.NextAfter,
	}, nil
}

// GetAgentSessionTranscriptSystem returns the full deduplicated system-prompt
// timeline with its raw values (the expansion fetch behind the collapsed
// system group), read whole from the session's system_prompt turn rows. An
// unknown session id answers the domain not-found error, the same convention
// the transcript endpoint holds.
func (s *RequestLogService) GetAgentSessionTranscriptSystem(sessionID string) (*AgentSessionTranscriptSystem, error) {
	// The session-rows read is the not-found guard (the same convention the
	// transcript endpoint holds); the values themselves come from the
	// session's system_prompt turn rows.
	if _, err := s.loadSessionRows(sessionID); err != nil {
		return nil, err
	}
	entries, err := agentturns.ListSystemTurnEntries(s.db, sessionID)
	if err != nil {
		return nil, err
	}
	system := make([]AgentSessionReplaySystem, 0, len(entries))
	for i := range entries {
		system = append(system, AgentSessionReplaySystem{
			SeenAtRequest: entries[i].SeenAtRequest,
			Truncated:     false, // a capped body parses to no system row at all — a recorded value was never cut
			Raw:           entries[i].Raw,
		})
	}
	return &AgentSessionTranscriptSystem{System: system}, nil
}

// loadSessionRows reads the session's request rows chronologically and maps
// the empty result to the domain not-found error (the same convention the
// detail endpoint holds).
func (s *RequestLogService) loadSessionRows(sessionID string) ([]model.RequestLog, error) {
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
	return rows, nil
}

// loadSystemMeta maps the session's system_prompt turn rows onto the
// metadata-only wire shape: seen_at_request and the write-time-measured
// raw_length, with no raw text. Truncated is false by construction — a
// capped request body parses to no system row at all, so a recorded value
// was never cut (the field exists for shape compatibility with the raw
// entries). A turn-store read failure degrades to the empty timeline
// rather than failing the transcript — the same never-fail-the-whole
// contract the per-block lookups hold.
func (s *RequestLogService) loadSystemMeta(sessionID string) ([]AgentSessionReplaySystemMeta, error) {
	meta, err := agentturns.ListSystemTurnMeta(s.db, sessionID)
	if err != nil {
		logger.Warn("agent session transcript: fetch system timeline failed",
			zap.String("agent_session_id", sessionID), zap.Error(err))
		return []AgentSessionReplaySystemMeta{}, nil
	}
	out := make([]AgentSessionReplaySystemMeta, 0, len(meta))
	for i := range meta {
		out = append(out, AgentSessionReplaySystemMeta{
			SeenAtRequest: meta[i].SeenAtRequest,
			RawLength:     int(meta[i].ContentBytes),
		})
	}
	return out, nil
}

// agentSessionSummaryFromRows folds the session's ordered rows into the
// header-bar aggregate. Attribution is the FIRST row's agent_client — rows
// are (created_at, id) ASC, the same "earliest row names the session" rule
// the list endpoint's first_row CTE applies.
func agentSessionSummaryFromRows(sessionID string, rows []model.RequestLog) AgentSessionListItem {
	summary := AgentSessionListItem{
		AgentSessionID: sessionID,
		AgentClient:    derefString(rows[0].AgentClient),
		RequestCount:   int64(len(rows)),
		FirstSeenAt:    rows[0].CreatedAt,
		LastSeenAt:     rows[len(rows)-1].CreatedAt,
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
	}
	return summary
}

// turnBlockSequence builds the lazy block sequence over one session's
// request rows: block i materializes request i's turns + body row on first
// touch and memoizes, so the slicer's walk pays for exactly the blocks it
// takes and the cursor lookups cost one id comparison each. Provider names
// resolve once for the whole session (a bounded distinct set), the same
// batched read the list endpoints use.
func (s *RequestLogService) turnBlockSequence(sessionID string, rows []model.RequestLog) (transcriptBlockSequence, error) {
	providerNames, err := repository.FindProviderNamesByIDs(s.db,
		repository.CollectPtrIDs(rows, func(r *model.RequestLog) *uint { return r.ProviderID }))
	if err != nil {
		return transcriptBlockSequence{}, err
	}
	src := &turnsBlockSource{svc: s, providerNames: providerNames, cache: make([]*AgentSessionReplayBlock, len(rows))}
	return transcriptBlockSequence{
		length:  len(rows),
		idAt:    func(i int) string { return rows[i].RequestID },
		blockAt: func(i int) AgentSessionReplayBlock { return src.block(&rows[i], i) },
	}, nil
}

// turnsBlockSource is the per-request materializer behind the lazy
// sequence. Each block fetches its request's turn rows and body row once
// (two indexed point reads), so a page of N blocks costs ~2N microscopic
// queries plus the bytes of the page itself — bounded by the page, not by
// the session.
type turnsBlockSource struct {
	svc           *RequestLogService
	providerNames map[uint]string
	cache         []*AgentSessionReplayBlock // one slot per row; nil = not materialized
}

// block materializes row i's replay block. Every lookup failure degrades
// to the same "not recorded" shape the whole-session assembly produced for
// a missing body row — the block keeps its row metadata and never fails
// the transcript.
func (src *turnsBlockSource) block(row *model.RequestLog, i int) AgentSessionReplayBlock {
	if b := src.cache[i]; b != nil {
		return *b
	}
	b := src.materialize(row)
	src.cache[i] = &b
	return b
}

// materialize builds one block from the request row, its turn rows, and its
// body row.
func (src *turnsBlockSource) materialize(row *model.RequestLog) AgentSessionReplayBlock {
	turns, turnsErr := agentturns.ListTurnsByRequestIDs(src.svc.db, []string{row.RequestID})
	if turnsErr != nil {
		logger.Warn("agent session transcript: fetch turns failed",
			zap.String("agent_session_id", derefString(row.AgentSessionID)), zap.String("request_id", row.RequestID), zap.Error(turnsErr))
		turns = map[string][]model.AgentSessionTurn{}
	}
	bodyRows, bodiesErr := repository.GetRequestLogBodiesByRequestIDs(src.svc.db, []string{row.RequestID})
	if bodiesErr != nil {
		logger.Warn("agent session transcript: fetch bodies failed",
			zap.String("agent_session_id", derefString(row.AgentSessionID)), zap.String("request_id", row.RequestID), zap.Error(bodiesErr))
		bodyRows = map[string]*model.RequestLogBody{}
	}

	block := AgentSessionReplayBlock{
		RequestID:    row.RequestID,
		CreatedAt:    row.CreatedAt,
		ModelName:    row.ModelName,
		StatusClass:  DeriveStatusClass(row.StatusCode, row.FailReason),
		IsStream:     row.IsStream,
		InputTokens:  row.InputTokens,
		OutputTokens: row.OutputTokens,
		DurationMs:   row.DurationMs,
		ProviderName: lookupName(row.ProviderID, src.providerNames),
		Added:        []any{},
	}

	var body *model.RequestLogBody
	if b := bodyRows[row.RequestID]; b != nil {
		body = b
		// Same inline-cap contract as the detail endpoint: the stored
		// column is the full text, the flag says the inline copy was cut
		// (len over the cap is exactly truncateInlineBody's cut
		// condition, so text and flag cannot disagree).
		block.AddedTruncated = len(b.RequestBody) > maxInlineBodyBytes
	}

	// Added: this request's new caller-side messages, in event order — the
	// write-time classification of exactly the messages the whole-session
	// pass kept in added (user_input + tool_result + injected concatenate
	// back to it). Pending and assistant rows are not input messages.
	for _, t := range turns[row.RequestID] {
		switch t.EventType {
		case agentturns.EventUserInput, agentturns.EventToolResult, agentturns.EventInjected:
			var m any
			if err := json.Unmarshal([]byte(t.Content), &m); err != nil {
				// A content blob that no longer parses is a corrupt row, not
				// a lost message the reader can reconstruct — skip it; the
				// block keeps its other halves.
				continue
			}
			block.Added = append(block.Added, m)
		}
	}

	// Assistant: the reply attribution, re-derived live from the body row
	// under the pass's priority inline > stream > resend > none. Inline and
	// stream read the captured ORIGINALS by reference (never copied into
	// the turn store); resend lifts the copies the successor's supplement
	// row carries; none is the explicit "not recorded" placeholder.
	block.Assistant = src.assistantFor(row.RequestID, turns[row.RequestID], body)
	return block
}

// assistantFor attributes one block's reply from its turn rows and body
// row. The live derivation of the inline/stream arms is what keeps the
// store self-healing: a capture that landed after the turn batch (a
// retried finalize) or a stream file that no longer reads both resolve to
// what the captured bytes actually say, exactly as the whole-session
// assembly resolved them by re-reading everything.
func (src *turnsBlockSource) assistantFor(requestID string, turns []model.AgentSessionTurn, body *model.RequestLogBody) AgentSessionReplayAssistant {
	var responseBody, streamPath string
	var streamWriteTruncated bool
	if body != nil {
		responseBody = body.ResponseBody
		streamPath = body.StreamBodyPath
		streamWriteTruncated = body.StreamBodyTruncated
	}
	if responseBody != "" {
		return AgentSessionReplayAssistant{
			Source:    ReplaySourceInline,
			Truncated: len(responseBody) > maxInlineBodyBytes,
			Raw:       truncateInlineBody(responseBody),
		}
	}
	if streamPath != "" {
		stream, readCapped := readStreamBodyInlineCapped(src.svc.bodiesDir, streamPath)
		if stream != "" {
			return AgentSessionReplayAssistant{
				Source: ReplaySourceStream,
				// Either cut — the read-side inline cap here, or the
				// write-side disk backstop the body row recorded — means the
				// capture shown is not the full original.
				Truncated: readCapped || streamWriteTruncated,
				Raw:       stream,
			}
		}
		// A recorded path whose file reads empty falls through to the
		// resend supplement below — the assembly's degradation for an
		// unreadable capture, kept verbatim.
	}
	for _, t := range turns {
		if t.EventType != agentturns.EventAssistantReply || t.Seq < agentturns.SupplementSeqBase {
			continue
		}
		var envelope struct {
			Source string `json:"source"`
			Raw    []any  `json:"raw"`
		}
		if err := json.Unmarshal([]byte(t.Content), &envelope); err != nil || envelope.Source != ReplaySourceResend {
			continue
		}
		return AgentSessionReplayAssistant{Source: ReplaySourceResend, Raw: envelope.Raw}
	}
	return AgentSessionReplayAssistant{Source: ReplaySourceNone}
}
