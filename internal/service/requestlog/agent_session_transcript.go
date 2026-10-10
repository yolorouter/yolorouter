// Transcript composition for tool sessions: the read endpoints that turn one
// session's rows + captured bodies into the replay payloads the frontend
// renders. This file is the assembly seam between the read paths the detail
// endpoint already owns (repository rows, batched body rows, 1 MiB inline
// caps, bodiesDir stream inlining) and the pure dedup pass in
// agent_session_replay.go — everything storage-shaped happens here, so the
// pass itself stays table-testable (its table tests sit in
// agent_session_replay_test.go) and this layer stays a fat-free mapping.
// The output splits in two over the same assembly: the transcript endpoint
// returns the aggregate + system METADATA + one cursor-paginated page of
// blocks (sliced by agent_session_transcript_page.go), and the
// /transcript/system endpoint returns the system timeline's raw text on
// demand. The assembly itself runs behind the shared LRU cache in
// agent_session_transcript_cache.go — both endpoints hit it, so walking a
// session's pages reads the session once.
package requestlog

import (
	"encoding/json"

	"go.uber.org/zap"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// AgentSessionReplaySystemMeta is the metadata-only half of one system
// timeline entry, what the transcript response ships so the first screen
// carries no raw system-prompt text (a long session can hold dozens of
// kilobytes × N of it). RawLength is the raw value's JSON serialization size
// in UTF-8 bytes — exactly the bytes the entry adds to the
// /transcript/system response — so the frontend can budget the on-expand
// fetch. Truncated system entries always measure the value actually emitted
// (a capped request body parses to no system record at all, so there is no
// "pre-truncation length" to over-report).
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
// System metadata list by construction — both come from the same assembly.
type AgentSessionTranscriptSystem struct {
	System []AgentSessionReplaySystem `json:"system"`
}

// GetAgentSessionTranscript returns one cursor-paginated page of the replay
// transcript for a tool session (see AgentSessionTranscriptPage for the
// cursor semantics and SliceAgentSessionTranscriptBlocks for the page caps).
// The session aggregate and system timeline metadata are derived from the
// whole session on every call — pagination slices only the blocks, so the
// header numbers never depend on how much is loaded. The assembly itself
// comes from the shared LRU cache (agent_session_transcript_cache.go): the
// first page of a session pays the full read, later pages probe + slice.
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
func (s *RequestLogService) GetAgentSessionTranscript(sessionID string, page AgentSessionTranscriptPage) (*AgentSessionTranscript, error) {
	assembled, err := s.assemblyCache.get(sessionID)
	if err != nil {
		return nil, err
	}
	sliced := SliceAgentSessionTranscriptBlocks(assembled.blocks, page)
	return &AgentSessionTranscript{
		Session:       assembled.summary,
		System:        systemTimelineMetadata(assembled.system),
		Blocks:        sliced.Blocks,
		HasMoreBefore: sliced.HasMoreBefore,
		NextBefore:    sliced.NextBefore,
		HasMoreAfter:  sliced.HasMoreAfter,
		NextAfter:     sliced.NextAfter,
	}, nil
}

// GetAgentSessionTranscriptSystem returns the full deduplicated system-prompt
// timeline with its raw values (the expansion fetch behind the collapsed
// system group). The whole replay is assembled exactly as the transcript
// endpoint assembles it — from the SAME cache, so an expand after a replay
// walk pays only the fingerprint probe — and the block half is simply not
// returned.
func (s *RequestLogService) GetAgentSessionTranscriptSystem(sessionID string) (*AgentSessionTranscriptSystem, error) {
	assembled, err := s.assemblyCache.get(sessionID)
	if err != nil {
		return nil, err
	}
	return &AgentSessionTranscriptSystem{System: assembled.system}, nil
}

// loadAgentSessionReplay is the load-and-assemble compute behind the
// transcript cache: the session's rows and captured bodies in, the full
// aggregate + block sequence + system timeline out, under the fingerprint of
// the rows actually read (see agent_session_transcript_cache.go for why the
// fingerprint follows the read, not the probe). See GetAgentSessionTranscript
// for the read-path conventions this seam holds.
func (s *RequestLogService) loadAgentSessionReplay(sessionID string) (*assembledAgentSessionReplay, repository.AgentSessionAssemblyFingerprint, error) {
	if sessionID == "" {
		return nil, repository.AgentSessionAssemblyFingerprint{}, errcode.ErrAgentSessionNotFound
	}
	rows, err := repository.ListRequestLogsByAgentSession(s.db, sessionID)
	if err != nil {
		return nil, repository.AgentSessionAssemblyFingerprint{}, err
	}
	if len(rows) == 0 {
		return nil, repository.AgentSessionAssemblyFingerprint{}, errcode.ErrAgentSessionNotFound
	}

	providerNames, err := repository.FindProviderNamesByIDs(s.db,
		repository.CollectPtrIDs(rows, func(r *model.RequestLog) *uint { return r.ProviderID }))
	if err != nil {
		return nil, repository.AgentSessionAssemblyFingerprint{}, err
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
	return &assembledAgentSessionReplay{summary: summary, blocks: blocks, system: system},
		repository.AgentSessionAssemblyFingerprint{
			RequestCount:  int64(len(rows)),
			LastRequestID: rows[len(rows)-1].RequestID,
		}, nil
}

// systemTimelineMetadata maps the assembled system timeline onto its
// metadata-only shape (index-preserving — entry i of the metadata list
// describes entry i of the raw list).
func systemTimelineMetadata(system []AgentSessionReplaySystem) []AgentSessionReplaySystemMeta {
	meta := make([]AgentSessionReplaySystemMeta, 0, len(system))
	for i := range system {
		meta = append(meta, AgentSessionReplaySystemMeta{
			SeenAtRequest: system[i].SeenAtRequest,
			Truncated:     system[i].Truncated,
			RawLength:     systemRawLength(system[i].Raw),
		})
	}
	return meta
}

// systemRawLength measures one system value's JSON serialization in bytes.
// encoding/json sorts map keys, so the length is deterministic and equals
// the bytes the entry contributes to the /transcript/system response the
// frontend will fetch.
func systemRawLength(raw any) int {
	data, err := json.Marshal(raw)
	if err != nil {
		// Unreachable for parsed-JSON values; zero would only under-report.
		return 0
	}
	return len(data)
}
