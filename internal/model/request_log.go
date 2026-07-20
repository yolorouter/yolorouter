// Package model additions for M5: RequestLog — one row per gateway business
// request (PRD §6.5 GATE-13/21). Schema lives in
// migrations/{sqlite,postgres}/00007_create_request_logs.sql — goose owns
// DDL, GORM here is query-only (no AutoMigrate). See design doc
// .claude/docs/2026-07-20-m5-gateway-design.md §3.2.
package model

import "time"

// RequestLog is the gateway's per-request audit/cost row. A failover is still
// ONE row — Attempts records how many candidates were tried (GATE-13). The
// query/filter UI is a separate module (PRD §6.8); M5 only writes rows.
//
// CostKnown=false means price or token info was missing for this request —
// CostCents is 0 but must NOT be displayed as "free" (GATE-21 / PRD §6.7.6).
type RequestLog struct {
	ID               uint      `gorm:"column:id;primaryKey" json:"id"`
	RequestID        string    `gorm:"column:request_id" json:"request_id"`
	APIKeyID         *uint     `gorm:"column:api_key_id" json:"api_key_id"`
	ModelName        string    `gorm:"column:model_name" json:"model_name"`
	ProviderID       *uint     `gorm:"column:provider_id" json:"provider_id"`
	IsStream         bool      `gorm:"column:is_stream" json:"is_stream"`
	StatusCode       int       `gorm:"column:status_code" json:"status_code"`
	InputTokens      int       `gorm:"column:input_tokens" json:"input_tokens"`
	OutputTokens     int       `gorm:"column:output_tokens" json:"output_tokens"`
	CacheWriteTokens int       `gorm:"column:cache_write_tokens" json:"cache_write_tokens"`
	CacheReadTokens  int       `gorm:"column:cache_read_tokens" json:"cache_read_tokens"`
	CostCents        int64     `gorm:"column:cost_cents" json:"cost_cents"`
	CostKnown        bool      `gorm:"column:cost_known" json:"cost_known"`
	FailReason       *string   `gorm:"column:fail_reason" json:"fail_reason"`
	Attempts         int       `gorm:"column:attempts" json:"attempts"`
	AttemptsDetail   *string   `gorm:"column:attempts_detail" json:"attempts_detail"`
	DurationMs       int64     `gorm:"column:duration_ms" json:"duration_ms"`
	CreatedAt        time.Time `gorm:"column:created_at" json:"created_at"`
}

func (RequestLog) TableName() string { return "request_logs" }
