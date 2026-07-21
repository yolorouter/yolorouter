// RequestLogBody — the request/response
// bodies for one gateway request. Schema lives
// in migrations/{sqlite,postgres}/00011_create_request_log_bodies.sql — goose
// owns DDL, GORM here is query-only (no AutoMigrate), mirroring RequestLog.
package model

import "time"

// RequestLogBody is the 1:1 body row joined to request_logs by request_id
// (UNIQUE — enforced 1:1 + idempotent UPSERT). Empty strings = not captured
// (early failure before body read, or capture failure) — the detail page
// degrades to "not recorded". For stream requests, response_body and
// upstream_response_body are empty and the sent SSE lives at stream_body_path.
//
// RequestHeaders is the caller's request headers as a JSON object with
// sensitive headers already masked (gateway.SanitizeHeaders) — only the
// auth headers themselves are excluded from logging, not all headers.
type RequestLogBody struct {
	ID                   uint      `gorm:"column:id;primaryKey" json:"id"`
	RequestID            string    `gorm:"column:request_id;uniqueIndex" json:"request_id"`
	RequestHeaders       string    `gorm:"column:request_headers" json:"request_headers"`
	RequestBody          string    `gorm:"column:request_body" json:"request_body"`
	UpstreamRequestBody  string    `gorm:"column:upstream_request_body" json:"upstream_request_body"`
	ResponseBody         string    `gorm:"column:response_body" json:"response_body"`
	UpstreamResponseBody string    `gorm:"column:upstream_response_body" json:"upstream_response_body"`
	StreamBodyPath       string    `gorm:"column:stream_body_path" json:"stream_body_path"`
	StreamBodyTruncated  bool      `gorm:"column:stream_body_truncated" json:"stream_body_truncated"`
	CreatedAt            time.Time `gorm:"column:created_at" json:"created_at"`
}

func (RequestLogBody) TableName() string { return "request_log_bodies" }
