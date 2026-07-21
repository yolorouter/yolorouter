// Package service additions for M6.1: request-log list + detail composition
// per PRD §6.8. Strict 3-layer: handler → service → repository. The service
// owns the business-DTO shape (RequestLogListItem / RequestLogDetail), the
// owner_label / provider_name JOIN post-fetch, the attempts_detail JSON
// parse, the status-class derivation at the row level, and the CSV stream
// assembly. The repository owns the SQL filter / pagination / aggregate
// helpers (request_log_query.go). See design doc
// .claude/docs/2026-07-20-m6-analytics-design.md §4.4/§4.5.
package service

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

	"github.com/yolorouter/yolorouter-ce/internal/gateway"
	"github.com/yolorouter/yolorouter-ce/internal/model"
	"github.com/yolorouter/yolorouter-ce/internal/repository"
	"github.com/yolorouter/yolorouter-ce/pkg/csvutil"
	"github.com/yolorouter/yolorouter-ce/pkg/errcode"
	"github.com/yolorouter/yolorouter-ce/pkg/logger"
)

// RequestLogService is the stateless composition layer over
// request_log_query.go. M6.1 has no caching, masking, or permission post-
// processing — those concerns will hang off this struct in later milestones
// (PRD §6.8.6 owner_label masking per admin role is a likely M6.2 add).
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

// RequestLogListFilter is the service-layer filter shape. The handler fills
// it from query params; the service maps it to repository.RequestLogFilter
// internally. Kept as a separate type so the handler never imports
// repository — same layering convention as APIKeyService's input structs.
type RequestLogListFilter struct {
	RequestID   string
	APIKeyID    *uint
	ModelName   string
	ProviderID  *uint
	StatusClass string
	IsStream    *bool
	StartTime   *time.Time // inclusive
	EndTime     *time.Time // exclusive
	Page        int
	PageSize    int
}

// RequestLogListItem is the list-row DTO. Per PRD §6.8.6 it carries no
// plaintext key material (M5 only stores id/prefix anyway) and no
// attempts_detail blob — only the attempt count, with full attempts reserved
// for the detail endpoint. OwnerLabel and ProviderName are JOIN'd from
// api_keys / providers at this layer. StatusClass mirrors the list-filter
// buckets so a row and its filter use one definition of "success".
type RequestLogListItem struct {
	RequestID    string    `json:"request_id"`
	APIKeyID     *uint     `json:"api_key_id"`
	OwnerLabel   string    `json:"owner_label"`
	ModelName    string    `json:"model_name"`
	ProviderID   *uint     `json:"provider_id"`
	ProviderName string    `json:"provider_name"`
	IsStream     bool      `json:"is_stream"`
	StatusCode   int       `json:"status_code"`
	StatusClass  string    `json:"status_class"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	CostCents    int64     `json:"cost_cents"`
	CostKnown    bool      `json:"cost_known"`
	FailReason   *string   `json:"fail_reason"`
	Attempts     int       `json:"attempts"`
	DurationMs   int64     `json:"duration_ms"`
	CreatedAt    time.Time `json:"created_at"`
}

// RequestLogDetail is the single-row detail DTO. AttemptsDetail is parsed
// from the stored JSON string into []gateway.AttemptRecord so the frontend
// can render failover order directly without re-parsing. The 7 body fields
// (M6.2, PRD §6.8.4/§6.8.6) are sourced from the 1:1 request_log_bodies row
// via repository.GetRequestLogBodyByRequestID — when that row is absent
// (pre-migration rows or capture failure) they degrade to zero values and
// the detail page shows "not recorded" rather than erroring.
type RequestLogDetail struct {
	RequestID            string                  `json:"request_id"`
	APIKeyID             *uint                   `json:"api_key_id"`
	OwnerLabel           string                  `json:"owner_label"`
	ModelName            string                  `json:"model_name"`
	ProviderID           *uint                   `json:"provider_id"`
	ProviderName         string                  `json:"provider_name"`
	IsStream             bool                    `json:"is_stream"`
	StatusCode           int                     `json:"status_code"`
	StatusClass          string                  `json:"status_class"`
	InputTokens          int                     `json:"input_tokens"`
	OutputTokens         int                     `json:"output_tokens"`
	CostCents            int64                   `json:"cost_cents"`
	CostKnown            bool                    `json:"cost_known"`
	FailReason           *string                 `json:"fail_reason"`
	Attempts             int                     `json:"attempts"`
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
	cut := s[:maxInlineBodyBytes]
	// Trim any trailing bytes left by slicing a multi-byte rune in half at the
	// cap boundary (an incomplete encoding decodes as RuneError with size 1).
	for len(cut) > 0 {
		if r, size := utf8.DecodeLastRuneInString(cut); r != utf8.RuneError || size > 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut + fmt.Sprintf("\n\n… [truncated: showing first %d of %d bytes]", len(cut), len(s))
}

// ListRequestLogs returns one page of rows (newest first) plus the total
// count for pagination. Each row is enriched with its api_keys.owner_label
// and providers.name via a post-fetch batch lookup (one SELECT per related
// table) rather than a SQL JOIN, so the shared repository.ListRequestLogs
// stays the single source of truth for filtering and pagination.
func (s *RequestLogService) ListRequestLogs(filter RequestLogListFilter) ([]RequestLogListItem, int64, error) {
	rows, total, err := repository.ListRequestLogs(s.db, toRepoFilterFromList(filter))
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
// owner_label / provider_name and deriving status_class. Shared by the
// paginated list endpoint and the keyset-based CSV export so both render
// identical rows.
func (s *RequestLogService) toListItems(rows []model.RequestLog) ([]RequestLogListItem, error) {
	if len(rows) == 0 {
		return []RequestLogListItem{}, nil
	}
	ownerLabels, providerNames, err := s.fetchRelatedNames(rows)
	if err != nil {
		return nil, err
	}
	items := make([]RequestLogListItem, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		items = append(items, RequestLogListItem{
			RequestID:    r.RequestID,
			APIKeyID:     r.APIKeyID,
			OwnerLabel:   lookupName(r.APIKeyID, ownerLabels),
			ModelName:    r.ModelName,
			ProviderID:   r.ProviderID,
			ProviderName: lookupName(r.ProviderID, providerNames),
			IsStream:     r.IsStream,
			StatusCode:   r.StatusCode,
			StatusClass:  DeriveStatusClass(r.StatusCode, r.FailReason),
			InputTokens:  r.InputTokens,
			OutputTokens: r.OutputTokens,
			CostCents:    r.CostCents,
			CostKnown:    r.CostKnown,
			FailReason:   r.FailReason,
			Attempts:     r.Attempts,
			DurationMs:   r.DurationMs,
			CreatedAt:    r.CreatedAt,
		})
	}
	return items, nil
}

// GetRequestLogDetail returns the single row for requestID (PRD §6.8.7:
// "可通过请求标识精确找到单次请求"), with attempts_detail parsed into
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
	ownerLabels, providerNames, err := s.fetchRelatedNames([]model.RequestLog{*row})
	if err != nil {
		return nil, err
	}
	// Always return a non-nil slice (never null) so the frontend's
	// attempts-detail renderer doesn't need a separate nil-check — empty
	// array reads as "no attempts recorded" (e.g. pre-check failure before
	// any candidate was tried, see gateway/log.go).
	attempts := []gateway.AttemptRecord{}
	if row.AttemptsDetail != nil && *row.AttemptsDetail != "" {
		if err := json.Unmarshal([]byte(*row.AttemptsDetail), &attempts); err != nil {
			return nil, err
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

	detail := &RequestLogDetail{
		RequestID:      row.RequestID,
		APIKeyID:       row.APIKeyID,
		OwnerLabel:     lookupName(row.APIKeyID, ownerLabels),
		ModelName:      row.ModelName,
		ProviderID:     row.ProviderID,
		ProviderName:   lookupName(row.ProviderID, providerNames),
		IsStream:       row.IsStream,
		StatusCode:     row.StatusCode,
		StatusClass:    DeriveStatusClass(row.StatusCode, row.FailReason),
		InputTokens:    row.InputTokens,
		OutputTokens:   row.OutputTokens,
		CostCents:      row.CostCents,
		CostKnown:      row.CostKnown,
		FailReason:     row.FailReason,
		Attempts:       row.Attempts,
		AttemptsDetail: attempts,
		DurationMs:     row.DurationMs,
		CreatedAt:      row.CreatedAt,
	}
	if bodyRow != nil {
		detail.RequestHeaders = bodyRow.RequestHeaders // small (masked headers), never capped
		// The four bodies are returned INLINE in this JSON response and
		// rendered whole by BodyViewer (no ranged endpoint like the stream
		// body has), so a pathological multi-MiB body — e.g. a 20 MiB non-JSON
		// request body captured before the parse — would otherwise be shipped
		// in full and frozen into the admin's DOM. Cap each with a visible
		// marker (code-review finding).
		detail.RequestBody = truncateInlineBody(bodyRow.RequestBody)
		detail.UpstreamRequestBody = truncateInlineBody(bodyRow.UpstreamRequestBody)
		detail.ResponseBody = truncateInlineBody(bodyRow.ResponseBody)
		detail.UpstreamResponseBody = truncateInlineBody(bodyRow.UpstreamResponseBody)
		detail.StreamBodyPath = bodyRow.StreamBodyPath
		detail.StreamBodyTruncated = bodyRow.StreamBodyTruncated
		detail.HasStreamBody = bodyRow.StreamBodyPath != ""
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
// columns correctly (owner_label / provider_name / model_name may all carry
// CJK). The csv.Writer is flushed after every page so the HTTP response
// streams incrementally rather than buffering the whole export in memory.
//
// Page-walking — instead of a single un-paginated SELECT — keeps the
// service's read path on the shared repository.ListRequestLogs, the same
// code the list endpoint uses. v0.1 admin exports are time-windowed and
// small enough that the COUNT + N-page overhead is negligible; a streaming
// cursor can replace this in M6.2 if export volumes ever justify it.
// BuildExportRows pulls every row matching filter (keyset pagination so
// concurrent inserts can't drift the result set) and converts it to the wire
// DTO. Split from WriteCSVRows so the handler can fail BEFORE committing the
// HTTP 200 / CSV headers — a mid-pull DB error returns a JSON envelope, not a
// truncated CSV reported as success (Codex adversarial finding).
func (s *RequestLogService) BuildExportRows(filter RequestLogListFilter) ([]RequestLogListItem, error) {
	const pageSize = 200
	rf := toRepoFilterFromList(filter)
	var cursor *repository.RequestLogCursor
	items := make([]RequestLogListItem, 0)
	for {
		rows, err := repository.ListRequestLogsKeyset(s.db, rf, cursor, pageSize)
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

// fetchRelatedNames batch-loads owner_label / provider_name for every
// api_key_id / provider_id referenced in rows. Returns two lookup maps
// keyed by id; rows whose key/provider has been soft-deleted (or whose FK
// is NULL) surface as empty strings in the DTO via lookupName. Two small
// SELECTs rather than one JOIN keeps the read path on the shared
// repository.ListRequestLogs — adding batch-by-id repository helpers
// purely for this display-name lookup isn't worth the layering overhead
// at M6.1.
func (s *RequestLogService) fetchRelatedNames(rows []model.RequestLog) (ownerLabels map[uint]string, providerNames map[uint]string, err error) {
	apiKeyIDs := make([]uint, 0)
	providerIDs := make([]uint, 0)
	seenKey := make(map[uint]struct{})
	seenProv := make(map[uint]struct{})
	for i := range rows {
		if rows[i].APIKeyID != nil {
			if _, ok := seenKey[*rows[i].APIKeyID]; !ok {
				seenKey[*rows[i].APIKeyID] = struct{}{}
				apiKeyIDs = append(apiKeyIDs, *rows[i].APIKeyID)
			}
		}
		if rows[i].ProviderID != nil {
			if _, ok := seenProv[*rows[i].ProviderID]; !ok {
				seenProv[*rows[i].ProviderID] = struct{}{}
				providerIDs = append(providerIDs, *rows[i].ProviderID)
			}
		}
	}

	ownerLabels = make(map[uint]string, len(apiKeyIDs))
	if len(apiKeyIDs) > 0 {
		var keys []model.APIKey
		if qErr := s.db.Select("id", "owner_label").Where("id IN ?", apiKeyIDs).Find(&keys).Error; qErr != nil {
			return nil, nil, qErr
		}
		for i := range keys {
			ownerLabels[keys[i].ID] = keys[i].OwnerLabel
		}
	}

	providerNames = make(map[uint]string, len(providerIDs))
	if len(providerIDs) > 0 {
		var provs []model.Provider
		if qErr := s.db.Select("id", "name").Where("id IN ?", providerIDs).Find(&provs).Error; qErr != nil {
			return nil, nil, qErr
		}
		for i := range provs {
			providerNames[provs[i].ID] = provs[i].Name
		}
	}
	return ownerLabels, providerNames, nil
}

// toRepoFilterFromList maps the service-layer list filter to repository's
// pointer-typed filter. Named toRepoFilterFromList (rather than toRepoFilter)
// because analytics_service.go already owns that name for AnalyticsFilter —
// the repository filter is the shared query shape used by the dashboard,
// analytics, and request-log list, and each service file keeps its own
// mapper so the handler layer doesn't take a repository import.
func toRepoFilterFromList(f RequestLogListFilter) *repository.RequestLogFilter {
	return &repository.RequestLogFilter{
		RequestID:   f.RequestID,
		APIKeyID:    f.APIKeyID,
		ModelName:   f.ModelName,
		ProviderID:  f.ProviderID,
		StatusClass: f.StatusClass,
		IsStream:    f.IsStream,
		StartTime:   f.StartTime,
		EndTime:     f.EndTime,
		Page:        f.Page,
		PageSize:    f.PageSize,
	}
}

// lookupName returns names[*id], or "" when id is nil or absent from the
// map (e.g. soft-deleted api_key, NULL provider_id on a pre-route reject).
// Centralizes the nil-check + map-lookup pair repeated for owner_label and
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

// csvHeaderRow is the column order for the CSV export. Mirrors the
// list-row DTO field order so the exported file matches what the UI table
// shows — same column selection, just renamed to snake_case for spreadsheet
// readability.
func csvHeaderRow() []string {
	return []string{
		"request_id", "created_at", "status_class", "status_code",
		"owner_label", "model_name", "provider_name",
		"is_stream", "attempts", "duration_ms",
		"input_tokens", "output_tokens",
		"cost_cents", "cost_known", "fail_reason",
	}
}

// csvRowFromItem renders one list-row DTO as a CSV record. time.Time uses
// RFC3339 for round-trippability. cost_known=false is rendered as the
// literal "false" rather than being conflated with cost_cents=0 so a
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
		it.OwnerLabel,
		it.ModelName,
		it.ProviderName,
		strconv.FormatBool(it.IsStream),
		strconv.Itoa(it.Attempts),
		strconv.FormatInt(it.DurationMs, 10),
		strconv.Itoa(it.InputTokens),
		strconv.Itoa(it.OutputTokens),
		strconv.FormatInt(it.CostCents, 10),
		strconv.FormatBool(it.CostKnown),
		failReason,
	}
}
