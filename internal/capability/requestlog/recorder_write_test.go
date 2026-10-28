package requestlog

// DB-backed tests for the recorder's write half. The recorder writes one audit
// record as several statements — the summary row, the body row, the agent-turn
// rows — and the contract under test here is that no statement can lose its
// turn to a deadline another statement spent.

import (
	"context"
	"testing"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// spentDeadlineView is the smallest View that reaches every write: a request
// id the assertions find the rows by, a key id the summary row carries, and a
// request body the body row stores. Everything else stays zero.
type spentDeadlineView struct {
	keyID uint
}

func (spentDeadlineView) RequestID() string             { return "req-spent-deadline" }
func (v spentDeadlineView) APIKeyID() uint              { return v.keyID }
func (spentDeadlineView) UserID() uint                  { return 0 }
func (spentDeadlineView) OriginalModel() string         { return "spent-model" }
func (spentDeadlineView) ProviderID() *uint             { return nil }
func (spentDeadlineView) IsStream() bool                { return false }
func (spentDeadlineView) IngressPath() string           { return "/v1/chat/completions" }
func (spentDeadlineView) CallSource() string            { return "" }
func (spentDeadlineView) ParentRequestID() string       { return "" }
func (spentDeadlineView) UpstreamURL() string           { return "" }
func (spentDeadlineView) W3CTraceID() string            { return "" }
func (spentDeadlineView) AgentClient() string           { return "" }
func (spentDeadlineView) AgentSessionID() string        { return "" }
func (spentDeadlineView) RequestHeaders() []byte        { return nil }
func (spentDeadlineView) RequestBody() []byte           { return []byte(`{"model":"spent-model"}`) }
func (spentDeadlineView) CompressedRequestBody() []byte { return nil }
func (spentDeadlineView) UpstreamRequestBody() []byte   { return nil }
func (spentDeadlineView) ResponseBody() []byte          { return nil }
func (spentDeadlineView) UpstreamResponseBody() []byte  { return nil }
func (spentDeadlineView) StreamBodyPath() string        { return "" }
func (spentDeadlineView) StreamBodyTruncated() bool     { return false }
func (spentDeadlineView) ImagePricingSnapshot() string  { return "" }
func (spentDeadlineView) AudioPricingSnapshot() string  { return "" }

// TestBothRowsSurviveASpentDeadline pins the CI failure shape of 2026-10-28:
// on a starved Windows runner the recorder pass's shared deadline expired
// between the summary write and the body write, committing the first and
// silently dropping the second — the gateway e2e saw a request log with no
// body row and no error anywhere, because the write is best-effort and the
// test process logs to a nop logger.
//
// The two rows are one audit record, so each write carries its own fresh
// deadline derived from a context that has already been cancelled here: a
// clock spent before or between the writes must not be able to lose either
// half of the record.
func TestBothRowsSurviveASpentDeadline(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	key := model.APIKey{KeyHash: "h-spent", KeyPrefix: "sk-spent", Status: model.APIKeyStatusActive}
	if err := db.Create(&key).Error; err != nil {
		t.Fatalf("seed api key: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // spent: a write inheriting this clock fails before it starts

	New(db).Record(ctx, spentDeadlineView{keyID: key.ID}, fact.Outcome{StatusCode: 200}, fact.Timeline{})

	var row model.RequestLog
	if err := db.Where("request_id = ?", "req-spent-deadline").First(&row).Error; err != nil {
		t.Fatalf("summary row lost to the spent deadline: %v", err)
	}
	var body model.RequestLogBody
	if err := db.Where("request_id = ?", "req-spent-deadline").First(&body).Error; err != nil {
		t.Fatalf("body row lost to the spent deadline: %v", err)
	}
	if body.RequestBody != `{"model":"spent-model"}` {
		t.Errorf("body row stored %q, want the caller's request body", body.RequestBody)
	}
}
