// Package requestlog composes the request-log list + detail views.
// Strict 3-layer: handler → service → repository. The service
// owns the business-DTO shape (RequestLogListItem / RequestLogDetail), the
// provider_name / username JOIN post-fetch, the attempts_detail JSON
// parse, the status-class derivation at the row level, and the CSV stream
// assembly. The repository owns the SQL filter / pagination / aggregate
// helpers (request_log_query.go).
package requestlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/gateway"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/csvutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// RequestLogService is the stateless composition layer over
// request_log_query.go. It has no caching, masking, or permission post-
// processing — those concerns will hang off this struct in later milestones
// (per-role masking is a likely add).
type RequestLogService struct {
	db *gorm.DB
}

// NewRequestLogService returns a RequestLogService bound to db. db is
// captured by reference; callers must not close it before this service
// stops being used (same lifecycle convention as every other service in
// internal/).
func NewRequestLogService(db *gorm.DB) *RequestLogService {
	return &RequestLogService{db: db}
}

// RequestLogListItem is the list-row DTO. It carries no
// plaintext key material (only id/prefix are stored anyway) and no
// attempts_detail blob — only the attempt count, with full attempts reserved
// for the detail endpoint. Username and ProviderName are JOIN'd from
// api_keys / providers at this layer. StatusClass mirrors the list-filter
// buckets so a row and its filter use one definition of "success".
type RequestLogListItem struct {
	RequestID string `json:"request_id"`
	APIKeyID  *uint  `json:"api_key_id"`
	// Username of the account that owns the request's key (request_logs.
	// user_id), batch-resolved post-fetch. Empty for auth-rejected
	// rows with no attributed key.
	Username  string `json:"username"`
	ModelName string `json:"model_name"`
	// RequestPath is the endpoint the caller hit (e.g. /v1/chat/completions);
	// empty for rows rejected before relay and for rows predating the field.
	RequestPath string `json:"request_path"`
	// Source is "" for a caller request, "vision_fallback" for a describe
	// sub-call working for ParentRequestID.
	Source           string  `json:"source"`
	ParentRequestID  string  `json:"parent_request_id"`
	ProviderID       *uint   `json:"provider_id"`
	ProviderName     string  `json:"provider_name"`
	IsStream         bool    `json:"is_stream"`
	StatusCode       int     `json:"status_code"`
	StatusClass      string  `json:"status_class"`
	InputTokens      int     `json:"input_tokens"`
	OutputTokens     int     `json:"output_tokens"`
	CacheWriteTokens int     `json:"cache_write_tokens"`
	CacheReadTokens  int     `json:"cache_read_tokens"`
	CostMicros       int64   `json:"cost_micros"`
	CostKnown        bool    `json:"cost_known"`
	FailReason       *string `json:"fail_reason"`
	Attempts         int     `json:"attempts"`
	// KeySwitches / Failovers split the row's retries into their two kinds —
	// rotating keys within one provider versus moving to another provider —
	// derived from the per-attempt detail with the same walk the provider
	// report uses (attempts that never reached a provider are skipped).
	KeySwitches int `json:"key_switches"`
	Failovers   int `json:"failovers"`
	// FinalProviderModel is the provider-side model name of the attempt that
	// produced this row's outcome; empty when no attempt reached a provider.
	FinalProviderModel string    `json:"final_provider_model"`
	DurationMs         int64     `json:"duration_ms"`
	CreatedAt          time.Time `json:"created_at"`
}

// RequestLogDetail is the single-row detail DTO. AttemptsDetail is parsed
// from the stored JSON string into []gateway.AttemptRecord so the frontend
// can render failover order directly without re-parsing. The 7 body fields
// are sourced from the 1:1 request_log_bodies row
// via repository.GetRequestLogBodyByRequestID — when that row is absent
// (pre-migration rows or capture failure) they degrade to zero values and
// the detail page shows "not recorded" rather than erroring.
type RequestLogDetail struct {
	RequestID string `json:"request_id"`
	APIKeyID  *uint  `json:"api_key_id"`
	// Username of the owning account — same resolution as the list rows.
	Username         string `json:"username"`
	ModelName        string `json:"model_name"`
	RequestPath      string `json:"request_path"`
	Source           string `json:"source"`
	ParentRequestID  string `json:"parent_request_id"`
	UpstreamURL      string `json:"upstream_url"`
	ProviderID       *uint  `json:"provider_id"`
	ProviderName     string `json:"provider_name"`
	IsStream         bool   `json:"is_stream"`
	StatusCode       int    `json:"status_code"`
	StatusClass      string `json:"status_class"`
	InputTokens      int    `json:"input_tokens"`
	OutputTokens     int    `json:"output_tokens"`
	CacheWriteTokens int    `json:"cache_write_tokens"`
	CacheReadTokens  int    `json:"cache_read_tokens"`
	CostMicros       int64  `json:"cost_micros"`
	CostKnown        bool   `json:"cost_known"`
	// Price snapshot passthrough: the four unit prices the row was billed
	// with, or all nil for rows that predate the snapshot or could not be
	// priced — the detail page renders that absence as "no snapshot".
	SettledInputPrice      *float64 `json:"settled_input_price"`
	SettledOutputPrice     *float64 `json:"settled_output_price"`
	SettledCacheWritePrice *float64 `json:"settled_cache_write_price"`
	SettledCacheReadPrice  *float64 `json:"settled_cache_read_price"`
	FailReason             *string  `json:"fail_reason"`
	// ImagePricingSnapshot is the per-image settlement's own account of
	// itself (mode, request axes, requested vs delivered count, unit price)
	// as JSON, empty on every row not billed per image. Rendered parsed, not
	// raw: it is an explanation, not a payload.
	ImagePricingSnapshot string `json:"image_pricing_snapshot"`
	Attempts             int    `json:"attempts"`
	// Same split the list rows carry — the detail must not narrow the type
	// contract the shared frontend row shape promises.
	KeySwitches          int                     `json:"key_switches"`
	Failovers            int                     `json:"failovers"`
	FinalProviderModel   string                  `json:"final_provider_model"`
	AttemptsDetail       []gateway.AttemptRecord `json:"attempts_detail"`
	DurationMs           int64                   `json:"duration_ms"`
	CreatedAt            time.Time               `json:"created_at"`
	RequestHeaders       string                  `json:"request_headers"`
	RequestBody          string                  `json:"request_body"`
	UpstreamRequestBody  string                  `json:"upstream_request_body"`
	ResponseBody         string                  `json:"response_body"`
	UpstreamResponseBody string                  `json:"upstream_response_body"`
	StreamBodyPath       string                  `json:"stream_body_path"`
	StreamBodyTruncated  bool                    `json:"stream_body_truncated"`
	HasStreamBody        bool                    `json:"has_stream_body"`
	// Input-compression audit fields. TokensSaved/CostSavedMicros are the
	// estimated reduction (zero when compression was off or the request was
	// rejected pre-relay). SkipReason is '' when compression ran; otherwise a
	// short code. CompressorsApplied is a comma-joined list. CompressedBody is
	// the post-compression request body (same truncation guard as request_body).
	CompressEstimatedTokensSaved int    `json:"compress_estimated_tokens_saved"`
	CompressEstimatedCostMicros  int64  `json:"compress_estimated_cost_saved_micros"`
	CompressSkipReason           string `json:"compress_skip_reason"`
	CompressorsApplied           string `json:"compressors_applied"`
	CompressedRequestBody        string `json:"compressed_request_body"`
}

// maxInlineBodyBytes caps each request/response body embedded inline in the
// detail DTO. 1 MiB is far more than any real request/response needs to be
// auditable, while keeping a pathological body from being shipped whole and
// rendered into the admin's DOM (unlike the stream body, these have no ranged
// preview endpoint). Larger bodies are truncated with a visible marker — never
// silently, matching the stream body's truncation-flag convention.
const maxInlineBodyBytes = 1 << 20 // 1 MiB

// truncateInlineBody returns s unchanged when it fits under maxInlineBodyBytes,
// otherwise the first maxInlineBodyBytes (trimmed to a UTF-8 rune boundary)
// plus a human-readable truncation marker stating the original size.
func truncateInlineBody(s string) string {
	if len(s) <= maxInlineBodyBytes {
		return s
	}
	cut := truncateBodyRuneSafe(s, maxInlineBodyBytes)
	return cut + fmt.Sprintf("\n\n… [truncated: showing first %d of %d bytes]", len(cut), len(s))
}

// truncateBodyRuneSafe returns s truncated to at most maxBytes bytes, backing
// off any partial UTF-8 rune left at the cut so the result never ends in a
// broken multi-byte sequence.
func truncateBodyRuneSafe(s string, maxBytes int) string {
	if len(s) > maxBytes {
		s = s[:maxBytes]
	}
	for len(s) > 0 {
		if r, size := utf8.DecodeLastRuneInString(s); r != utf8.RuneError || size > 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// ListRequestLogs returns one page of rows (newest first) plus the total
// count for pagination. It filters via repository.RequestLogFilter — the
// ONE shared query shape for the dashboard, analytics, and request-log
// list, filled from query params by the handler and passed straight
// through. Each row is enriched with its owning account username and
// providers.name via a post-fetch batch lookup (one SELECT per related
// table) rather than a SQL JOIN, so the shared repository.ListRequestLogs
// stays the single source of truth for filtering and pagination.
func (s *RequestLogService) ListRequestLogs(filter *repository.RequestLogFilter) ([]RequestLogListItem, int64, error) {
	rows, total, err := repository.ListRequestLogs(s.db, filter)
	if err != nil {
		return nil, 0, err
	}
	items, err := s.toListItems(rows)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// toListItems converts raw request_log rows into the wire DTO, batch-loading
// provider_name / username and deriving status_class. Shared by the
// paginated list endpoint and the keyset-based CSV export so both render
// identical rows.
func (s *RequestLogService) toListItems(rows []model.RequestLog) ([]RequestLogListItem, error) {
	if len(rows) == 0 {
		return []RequestLogListItem{}, nil
	}
	providerNames, userNames, err := s.fetchRelatedNames(rows)
	if err != nil {
		return nil, err
	}
	items := make([]RequestLogListItem, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		keySwitches, failovers, finalModel := deriveAttemptBreakdown(r.AttemptsDetail, r.ProviderID)
		items = append(items, RequestLogListItem{
			RequestID:          r.RequestID,
			APIKeyID:           r.APIKeyID,
			Username:           ownerUsernameFor(r.APIKeyID, r.UserID, userNames),
			ModelName:          r.ModelName,
			RequestPath:        r.RequestPath,
			Source:             r.Source,
			ParentRequestID:    r.ParentRequestID,
			ProviderID:         r.ProviderID,
			ProviderName:       lookupName(r.ProviderID, providerNames),
			IsStream:           r.IsStream,
			StatusCode:         r.StatusCode,
			StatusClass:        DeriveStatusClass(r.StatusCode, r.FailReason),
			InputTokens:        r.InputTokens,
			OutputTokens:       r.OutputTokens,
			CacheWriteTokens:   r.CacheWriteTokens,
			CacheReadTokens:    r.CacheReadTokens,
			CostMicros:         r.CostMicros,
			CostKnown:          r.CostKnown,
			FailReason:         r.FailReason,
			Attempts:           r.Attempts,
			KeySwitches:        keySwitches,
			Failovers:          failovers,
			FinalProviderModel: finalModel,
			DurationMs:         r.DurationMs,
			CreatedAt:          r.CreatedAt,
		})
	}
	return items, nil
}

// deriveAttemptBreakdown splits a row's retries into key switches (same
// provider, different key) and failovers (a different provider), and names
// the provider-side model the row's final provider answered with. Derived per page from the
// stored per-attempt JSON rather than persisted: the walk is cheap at page
// size, and a stored copy would be one more thing the write path can get
// out of sync. Attempts that never reached a provider are skipped, matching
// the provider report's failover accounting; a row whose detail this build
// cannot parse reports zeros here, and the detail page degrades that row
// to an empty attempts list.
func deriveAttemptBreakdown(detail *string, finalProviderID *uint) (keySwitches, failovers int, finalProviderModel string) {
	if detail == nil || *detail == "" {
		return 0, 0, ""
	}
	var attempts []gateway.AttemptRecord
	if err := json.Unmarshal([]byte(*detail), &attempts); err != nil {
		return 0, 0, ""
	}
	return breakdownFromAttempts(attempts, finalProviderID)
}

// breakdownFromAttempts is the walk itself, shared by the list (which parses
// the stored JSON first) and the detail (which already holds the parsed
// slice).
func breakdownFromAttempts(attempts []gateway.AttemptRecord, finalProviderID *uint) (keySwitches, failovers int, finalProviderModel string) {
	var last *gateway.AttemptRecord
	for i := range attempts {
		a := &attempts[i]
		if a.ProviderID == 0 {
			continue
		}
		if last != nil {
			if a.ProviderID != last.ProviderID {
				failovers++
			} else if a.KeyID != last.KeyID {
				keySwitches++
			}
		}
		last = a
		// "Served by" means the provider actually answered, and the model
		// shown must belong to the SAME provider the row is attributed to:
		// records that never got a response carry status 0, and an earlier
		// provider's answer must not be displayed under a later provider's
		// name. A row without a provider (nothing was ever attributed) falls
		// back to the last answer from anyone.
		if a.StatusCode != 0 && (finalProviderID == nil || a.ProviderID == *finalProviderID) {
			finalProviderModel = a.ProviderModelName
		}
	}
	return keySwitches, failovers, finalProviderModel
}

// GetRequestLogDetail returns the single row for requestID
// (locate a single request precisely by its request identifier), with attempts_detail parsed into
// []gateway.AttemptRecord. Returns errcode.ErrRequestLogNotFound when the
// row is absent; the handler maps that to a 404 envelope.
func (s *RequestLogService) GetRequestLogDetail(requestID string) (*RequestLogDetail, error) {
	row, err := repository.GetRequestLogByRequestID(s.db, requestID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrRequestLogNotFound
		}
		return nil, err
	}
	providerNames, userNames, err := s.fetchRelatedNames([]model.RequestLog{*row})
	if err != nil {
		return nil, err
	}
	// Always return a non-nil slice (never null) so the frontend's
	// attempts-detail renderer doesn't need a separate nil-check — empty
	// array reads as "no attempts recorded" (e.g. pre-check failure before
	// any candidate was tried, see gateway/log.go). A row whose stored
	// detail does not parse degrades to the same empty array instead of
	// failing the whole detail request — the rest of the row is still
	// worth showing, matching how a missing body row is handled below.
	attempts := []gateway.AttemptRecord{}
	if row.AttemptsDetail != nil && *row.AttemptsDetail != "" {
		if err := json.Unmarshal([]byte(*row.AttemptsDetail), &attempts); err != nil {
			attempts = []gateway.AttemptRecord{}
		}
	}

	// bodyRow == nil (not-found, or a lookup error we choose to swallow)
	// leaves every body field at its zero value — the detail page renders
	// that as "not recorded" rather than failing the whole request.
	bodyRow, bErr := repository.GetRequestLogBodyByRequestID(s.db, requestID)
	if bErr != nil {
		logger.Warn("request log detail: fetch body failed",
			zap.String("request_id", requestID), zap.Error(bErr))
	}

	keySwitches, failovers, finalModel := breakdownFromAttempts(attempts, row.ProviderID)
	detail := &RequestLogDetail{
		RequestID:              row.RequestID,
		APIKeyID:               row.APIKeyID,
		Username:               ownerUsernameFor(row.APIKeyID, row.UserID, userNames),
		ModelName:              row.ModelName,
		RequestPath:            row.RequestPath,
		Source:                 row.Source,
		ParentRequestID:        row.ParentRequestID,
		UpstreamURL:            row.UpstreamURL,
		ProviderID:             row.ProviderID,
		ProviderName:           lookupName(row.ProviderID, providerNames),
		IsStream:               row.IsStream,
		StatusCode:             row.StatusCode,
		StatusClass:            DeriveStatusClass(row.StatusCode, row.FailReason),
		InputTokens:            row.InputTokens,
		OutputTokens:           row.OutputTokens,
		CacheWriteTokens:       row.CacheWriteTokens,
		CacheReadTokens:        row.CacheReadTokens,
		CostMicros:             row.CostMicros,
		CostKnown:              row.CostKnown,
		SettledInputPrice:      row.SettledInputPrice,
		SettledOutputPrice:     row.SettledOutputPrice,
		SettledCacheWritePrice: row.SettledCacheWritePrice,
		SettledCacheReadPrice:  row.SettledCacheReadPrice,
		ImagePricingSnapshot:   row.ImagePricingSnapshot,
		FailReason:             row.FailReason,
		Attempts:               row.Attempts,
		KeySwitches:            keySwitches,
		Failovers:              failovers,
		FinalProviderModel:     finalModel,
		AttemptsDetail:         attempts,
		DurationMs:             row.DurationMs,
		CreatedAt:              row.CreatedAt,
		// Compress audit fields from the request_logs row.
		CompressEstimatedTokensSaved: row.CompressEstimatedTokensSaved,
		CompressEstimatedCostMicros:  row.CompressEstimatedCostSavedMicros,
		CompressSkipReason:           row.CompressSkipReason,
		CompressorsApplied:           row.CompressorsApplied,
	}
	if bodyRow != nil {
		detail.RequestHeaders = bodyRow.RequestHeaders // small (masked headers), never capped
		// The four bodies are returned INLINE in this JSON response and
		// rendered whole by BodyViewer (no ranged endpoint like the stream
		// body has), so a pathological multi-MiB body — e.g. a 20 MiB non-JSON
		// request body captured before the parse — would otherwise be shipped
		// in full and frozen into the admin's DOM. Cap each with a visible
		// marker.
		detail.RequestBody = truncateInlineBody(bodyRow.RequestBody)
		detail.UpstreamRequestBody = truncateInlineBody(bodyRow.UpstreamRequestBody)
		detail.ResponseBody = truncateInlineBody(bodyRow.ResponseBody)
		detail.UpstreamResponseBody = truncateInlineBody(bodyRow.UpstreamResponseBody)
		detail.StreamBodyPath = bodyRow.StreamBodyPath
		detail.StreamBodyTruncated = bodyRow.StreamBodyTruncated
		detail.HasStreamBody = bodyRow.StreamBodyPath != ""
		// Compressed body uses the SAME truncation guard as request_body so a
		// pathological compressed body can't freeze the admin's tab.
		detail.CompressedRequestBody = truncateInlineBody(bodyRow.CompressedRequestBody)
	}
	return detail, nil
}

// GetStreamBodyPath returns the stream_body_path stored on the
// request_log_bodies row for requestID (relative to the bodies dir, e.g.
// "req_x.stream"), or "" when there is no body row or no stream file was
// recorded for it (a non-streaming request, or one that failed before any
// SSE chunk was sent). The handler (GetRequestLogBodyStream) treats both a
// lookup error and an empty path as "no file to serve" — a 404, not a 500 —
// since a missing stream body is an expected shape for most rows, not a
// server fault.
func (s *RequestLogService) GetStreamBodyPath(requestID string) (string, error) {
	return repository.GetStreamBodyPathByRequestID(s.db, requestID)
}

// ExportRequestLogsCSV walks every page of the filter (PageSize=200, the
// repository's clamp ceiling) and streams each row as CSV. The UTF-8 BOM is
// written first so Excel/Sheets auto-detect the encoding and render CJK
// columns correctly (username / provider_name / model_name may all carry
// CJK). The csv.Writer is flushed after every page so the HTTP response
// streams incrementally rather than buffering the whole export in memory.
//
// Page-walking — instead of a single un-paginated SELECT — keeps the
// service's read path on the shared repository.ListRequestLogs, the same
// code the list endpoint uses. v0.1 admin exports are time-windowed and
// small enough that the COUNT + N-page overhead is negligible; a streaming
// cursor can replace this later if export volumes ever justify it.
// BuildExportRows pulls every row matching filter (keyset pagination so
// concurrent inserts can't drift the result set) and converts it to the wire
// DTO. Split from WriteCSVRows so the handler can fail BEFORE committing the
// HTTP 200 / CSV headers — a mid-pull DB error returns a JSON envelope, not a
// truncated CSV reported as success.
func (s *RequestLogService) BuildExportRows(filter *repository.RequestLogFilter) ([]RequestLogListItem, error) {
	const pageSize = 200
	var cursor *repository.RequestLogCursor
	items := make([]RequestLogListItem, 0)
	for {
		rows, err := repository.ListRequestLogsKeyset(s.db, filter, cursor, pageSize)
		if err != nil {
			return nil, err
		}
		page, err := s.toListItems(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, page...)
		if len(rows) < pageSize {
			return items, nil // last page
		}
		last := rows[len(rows)-1]
		cursor = &repository.RequestLogCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
}

// WriteCSVRows writes the UTF-8 BOM + header + rows to w. Call BuildExportRows
// first and only call this after it succeeds, so a build failure never
// produces a partial CSV on the wire. A failure HERE is a write-time error
// (client disconnect, broken pipe) and is unavoidable once headers are sent.
func (s *RequestLogService) WriteCSVRows(items []RequestLogListItem, w io.Writer) error {
	return csvutil.WriteCSV(w, csvHeaderRow(), buildCSVRecords(items))
}

// buildCSVRecords renders each list-row DTO as a sanitized CSV record. Split
// from WriteCSVRows so the row-building work can happen BEFORE the HTTP
// response is committed (see BuildExportRows).
func buildCSVRecords(items []RequestLogListItem) [][]string {
	records := make([][]string, len(items))
	for i := range items {
		records[i] = csvRowFromItem(items[i])
	}
	return records
}

// fetchRelatedNames batch-loads provider_name / username for every
// provider_id / user_id referenced in rows, via the shared namelookup
// helpers. Returns two lookup maps keyed by id; rows whose provider/user
// row is gone (or whose FK is NULL) surface as empty strings in the DTO
// via lookupName.
func (s *RequestLogService) fetchRelatedNames(rows []model.RequestLog) (providerNames, userNames map[uint]string, err error) {
	providerNames, err = repository.FindProviderNamesByIDs(s.db,
		repository.CollectPtrIDs(rows, func(r *model.RequestLog) *uint { return r.ProviderID }))
	if err != nil {
		return nil, nil, err
	}
	userNames, err = repository.FindUsernamesByIDs(s.db,
		repository.CollectPtrIDs(rows, func(r *model.RequestLog) *uint { return r.UserID }))
	if err != nil {
		return nil, nil, err
	}
	return providerNames, userNames, nil
}

// ownerUsernameFor resolves a row's display username only when the row has
// an attributed key. The multi-user backfill stamped user_id onto EVERY
// historical row — including auth-rejected ones that never resolved a key —
// so user_id alone cannot prove ownership; a NULL api_key_id means "nobody's
// traffic" regardless of what user_id says.
func ownerUsernameFor(apiKeyID, userID *uint, userNames map[uint]string) string {
	if apiKeyID == nil {
		return ""
	}
	return lookupName(userID, userNames)
}

// lookupName returns names[*id], or "" when id is nil or absent from the
// map (e.g. a missing users row, NULL provider_id on a pre-route reject).
// Centralizes the nil-check + map-lookup pair repeated for username and
// provider_name.
func lookupName(id *uint, names map[uint]string) string {
	if id == nil {
		return ""
	}
	return names[*id]
}

// DeriveStatusClass mirrors repository.applyStatusClass's SQL WHERE
// buckets at the row level: the same status_code + fail_reason inputs
// select the same bucket in both the list filter's WHERE clause and the
// row's status_class output. Exported so handler tests + future masking
// layers can call it without re-deriving. Keep these two in sync — if
// applyStatusClass changes, update DeriveStatusClass too.
func DeriveStatusClass(statusCode int, failReason *string) string {
	hasFail := failReason != nil && *failReason != ""
	switch {
	case statusCode >= 200 && statusCode < 300:
		if hasFail {
			return repository.StatusPartial
		}
		return repository.StatusSuccess
	case statusCode == 499:
		return repository.StatusCancelled
	case statusCode == 401, statusCode == 403, statusCode == 429:
		return repository.StatusRejected
	case statusCode >= 400:
		return repository.StatusFailed
	}
	// 1xx / 3xx / unknown — the gateway writes 2xx/4xx/5xx only, so this
	// branch is unreachable in practice; report "failed" rather than ""
	// so a future status_code source can't silently produce an empty
	// status_class.
	return repository.StatusFailed
}

// csvHeaderRow is the column order for the CSV export: a curated subset of
// the list-row DTO in that DTO's field order (request_path, source and
// parent_request_id stay out — the export targets spreadsheet analysis of
// traffic and cost, and rows are filterable by those dimensions before
// exporting).
func csvHeaderRow() []string {
	return []string{
		"request_id", "created_at", "status_class", "status_code",
		"username", "model_name", "provider_name",
		"is_stream", "key_switches", "failovers", "final_provider_model", "duration_ms",
		"input_tokens", "output_tokens",
		"cache_write_tokens", "cache_read_tokens",
		"cost_micros", "cost_known", "fail_reason",
	}
}

// csvRowFromItem renders one list-row DTO as a CSV record. time.Time uses
// RFC3339 for round-trippability. cost_known=false is rendered as the
// literal "false" rather than being conflated with cost_micros=0 so a
// spreadsheet doesn't silently display a price-unknown row as "free".
func csvRowFromItem(it RequestLogListItem) []string {
	failReason := ""
	if it.FailReason != nil {
		failReason = *it.FailReason
	}
	return []string{
		it.RequestID,
		it.CreatedAt.Format(time.RFC3339),
		it.StatusClass,
		strconv.Itoa(it.StatusCode),
		it.Username,
		it.ModelName,
		it.ProviderName,
		strconv.FormatBool(it.IsStream),
		strconv.Itoa(it.KeySwitches),
		strconv.Itoa(it.Failovers),
		it.FinalProviderModel,
		strconv.FormatInt(it.DurationMs, 10),
		strconv.Itoa(it.InputTokens),
		strconv.Itoa(it.OutputTokens),
		strconv.Itoa(it.CacheWriteTokens),
		strconv.Itoa(it.CacheReadTokens),
		strconv.FormatInt(it.CostMicros, 10),
		strconv.FormatBool(it.CostKnown),
		failReason,
	}
}
