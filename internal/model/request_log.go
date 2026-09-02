// RequestLog — one row per gateway business
// request. Schema lives in
// migrations/{sqlite,postgres}/00007_create_request_logs.sql — goose owns
// DDL, GORM here is query-only (no AutoMigrate).
package model

import "time"

// RequestLog is the gateway's per-request audit/cost row. A failover is still
// ONE row — Attempts records how many candidates were tried. The
// query/filter UI is a separate module; this layer only writes rows.
//
// CostKnown=false means price or token info was missing for this request —
// CostMicros is 0 but must NOT be displayed as "free".
type RequestLog struct {
	ID        uint   `gorm:"column:id;primaryKey" json:"id"`
	RequestID string `gorm:"column:request_id" json:"request_id"`
	APIKeyID  *uint  `gorm:"column:api_key_id" json:"api_key_id"`
	// UserID is the owner of the API key that made the request, denormalized
	// at write time so per-user statistics never need a JOIN through
	// api_keys. New unauthenticated audit rows (APIKeyID NULL) carry NULL —
	// they have no owner. Rows that predate ownership were backfilled to
	// the local admin wholesale (migration 00024), audit rows included, so
	// NULL-ness is not a reliable pre/post-auth signal on historical data.
	UserID           *uint  `gorm:"column:user_id" json:"user_id"`
	ModelName        string `gorm:"column:model_name" json:"model_name"`
	ProviderID       *uint  `gorm:"column:provider_id" json:"provider_id"`
	IsStream         bool   `gorm:"column:is_stream" json:"is_stream"`
	StatusCode       int    `gorm:"column:status_code" json:"status_code"`
	InputTokens      int    `gorm:"column:input_tokens" json:"input_tokens"`
	OutputTokens     int    `gorm:"column:output_tokens" json:"output_tokens"`
	CacheWriteTokens int    `gorm:"column:cache_write_tokens" json:"cache_write_tokens"`
	CacheReadTokens  int    `gorm:"column:cache_read_tokens" json:"cache_read_tokens"`
	CostMicros       int64  `gorm:"column:cost_micros" json:"cost_micros"`
	CostKnown        bool   `gorm:"column:cost_known" json:"cost_known"`
	// Price snapshot: the four unit prices (per million tokens) this row was
	// actually billed with, copied from the settling candidate at write time so
	// the row stays re-priceable after the candidate's prices change. The cache
	// prices are the EFFECTIVE ones — a candidate without a configured cache
	// price bills cache tokens at the input price, and that fallback value is
	// what lands here. All four are nil together on rows that predate the
	// snapshot and on unpriced rows (CostKnown=false); history is never
	// backfilled, and readers must render the absence as "no snapshot" rather
	// than zero.
	SettledInputPrice      *float64 `gorm:"column:settled_input_price" json:"settled_input_price"`
	SettledOutputPrice     *float64 `gorm:"column:settled_output_price" json:"settled_output_price"`
	SettledCacheWritePrice *float64 `gorm:"column:settled_cache_write_price" json:"settled_cache_write_price"`
	SettledCacheReadPrice  *float64 `gorm:"column:settled_cache_read_price" json:"settled_cache_read_price"`
	// Cache economics, computed at write time against the candidate's prices.
	// CacheReadSavedMicros is what the cache-read tokens saved versus the input
	// price; CacheWriteExtraMicros is the premium paid to write the cache. Both
	// non-negative; net saving = read saved − write extra (derived by readers).
	CacheReadSavedMicros  int64 `gorm:"column:cache_read_saved_micros" json:"cache_read_saved_micros"`
	CacheWriteExtraMicros int64 `gorm:"column:cache_write_extra_micros" json:"cache_write_extra_micros"`
	// Input-compression accounting, computed when the request passes through
	// the compress stage. TokensSaved/CostSavedMicros are non-negative
	// estimates of the reduction achieved; SkipReason is '' when compression
	// ran, otherwise a short stable code explaining why it was bypassed
	// (e.g. 'too_small', 'unsupported_mime'); CompressorsApplied is a
	// comma-joined list of compressor names that actually fired.
	CompressEstimatedTokensSaved     int    `gorm:"column:compress_estimated_tokens_saved" json:"compress_estimated_tokens_saved"`
	CompressEstimatedCostSavedMicros int64  `gorm:"column:compress_estimated_cost_saved_micros" json:"compress_estimated_cost_saved_micros"`
	CompressSkipReason               string `gorm:"column:compress_skip_reason" json:"compress_skip_reason"`
	CompressorsApplied               string `gorm:"column:compressors_applied" json:"compressors_applied"`
	// RequestPath is the caller's request path (e.g. /v1/chat/completions),
	// captured at Handle entry from the ingress URL. UpstreamURL is the full
	// URL the gateway dispatched to for the final attempt (e.g.
	// https://api.openai.com/v1/chat/completions). Both are empty when no
	// upstream request was sent (pre-relay rejection) and for rows predating
	// this column — the detail UI renders the placeholder for empty values.
	RequestPath string `gorm:"column:request_path" json:"request_path"`
	UpstreamURL string `gorm:"column:upstream_url" json:"upstream_url"`
	// ImagePricingSnapshot is what a per-image settlement priced by: the
	// billing mode, the request's quality/size, requested vs delivered
	// count, and the unit price the tier table resolved to. Written from
	// the same numbers the cost was computed from, so a billed row can
	// always explain itself. Empty on every row not billed per image.
	ImagePricingSnapshot string `gorm:"column:image_pricing_snapshot" json:"image_pricing_snapshot"`
	// ImageCount is how many images the request actually delivered, written
	// whenever the usage report counted images — priced or not. It is the
	// volume metric reports sum; the snapshot above is the bill's
	// explanation, and only exists when a price resolved.
	ImageCount int `gorm:"column:image_count" json:"image_count"`
	// Source marks who initiated the request: "" = a normal caller request,
	// "vision_fallback" = an image-description sub-call the gateway made on
	// behalf of the request named by ParentRequestID. Sub-calls are separate
	// rows so each row's tokens describe exactly one model; the description
	// cost reaches the key's totals through the sub-call's own row.
	Source          string  `gorm:"column:source" json:"source"`
	ParentRequestID string  `gorm:"column:parent_request_id" json:"parent_request_id"`
	FailReason      *string `gorm:"column:fail_reason" json:"fail_reason"`
	Attempts        int     `gorm:"column:attempts" json:"attempts"`
	AttemptsDetail  *string `gorm:"column:attempts_detail" json:"attempts_detail"`
	DurationMs      int64   `gorm:"column:duration_ms" json:"duration_ms"`
	// FactsJSON holds observations that have no column of their own, verbatim,
	// keyed by their stable names. Empty when everything reported was
	// recognised. It exists so an observation from a capability this build has
	// no column for is stored rather than dropped — a dropped one is
	// indistinguishable from one that never happened.
	FactsJSON string    `gorm:"column:facts_json" json:"facts_json"`
	CreatedAt time.Time `gorm:"column:created_at" json:"created_at"`
}

func (RequestLog) TableName() string { return "request_logs" }

// RequestLogSourceVisionFallback marks a request_logs row created by the
// gateway describing images on behalf of another request (vision fallback
// loopback sub-call). The empty string marks a normal caller request.
const RequestLogSourceVisionFallback = "vision_fallback"

// RequestLogSourceCallerFilter is the FILTER-ONLY sentinel meaning "normal
// caller rows" (stored source = ”). It is never stored: the stored value
// for a caller request is the empty string, but a query param needs a
// non-empty way to say "only those" since "" already means "no constraint".
const RequestLogSourceCallerFilter = "caller"
