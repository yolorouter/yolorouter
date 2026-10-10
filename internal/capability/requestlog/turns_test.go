// The recorder's turn-store half over a real migrated database: the audit
// write path derives the session's turn rows at record time, and a turn
// failure must never take the audit rows down with it — the degraded
// pending path (or, when even the table is gone, nothing) leaves the
// request_logs and request_log_bodies rows committed.
package requestlog

import (
	"context"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// turnsView is the minimal View stub: identity, attribution, and the
// captured bodies the recorder persists.
type turnsView struct {
	requestID  string
	sessionID  string
	requestBod string
	responseBd string
}

func (v *turnsView) RequestID() string             { return v.requestID }
func (v *turnsView) APIKeyID() uint                { return 1 }
func (v *turnsView) UserID() uint                  { return 1 }
func (v *turnsView) OriginalModel() string         { return "gpt-test" }
func (v *turnsView) ProviderID() *uint             { return nil }
func (v *turnsView) IsStream() bool                { return false }
func (v *turnsView) IngressPath() string           { return "/v1/chat/completions" }
func (v *turnsView) CallSource() string            { return "" }
func (v *turnsView) ParentRequestID() string       { return "" }
func (v *turnsView) UpstreamURL() string           { return "" }
func (v *turnsView) W3CTraceID() string            { return "" }
func (v *turnsView) AgentClient() string           { return "claude-code" }
func (v *turnsView) AgentSessionID() string        { return v.sessionID }
func (v *turnsView) RequestHeaders() []byte        { return nil }
func (v *turnsView) RequestBody() []byte           { return []byte(v.requestBod) }
func (v *turnsView) CompressedRequestBody() []byte { return nil }
func (v *turnsView) UpstreamRequestBody() []byte   { return nil }
func (v *turnsView) ResponseBody() []byte          { return []byte(v.responseBd) }
func (v *turnsView) UpstreamResponseBody() []byte  { return nil }
func (v *turnsView) StreamBodyPath() string        { return "" }
func (v *turnsView) StreamBodyTruncated() bool     { return false }
func (v *turnsView) ImagePricingSnapshot() string  { return "" }
func (v *turnsView) AudioPricingSnapshot() string  { return "" }

// TestRecordDerivesTurnRowsAtWriteTime pins the live wiring: Record writes
// the audit triple AND the request's turn batch — the session's replay
// structure exists the moment the log does.
func TestRecordDerivesTurnRowsAtWriteTime(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	rec := New(db)
	base := time.Date(2026, 10, 11, 14, 0, 0, 0, time.UTC)
	_ = base

	v1 := &turnsView{
		requestID:  "req-rec-1",
		sessionID:  "sess-rec",
		requestBod: `{"system":"S","messages":[{"role":"user","content":"hello"}]}`,
		responseBd: `{"content":[{"type":"text","text":"hi"}]}`,
	}
	rec.Record(context.Background(), v1, fact.Outcome{StatusCode: 200, Delivered: true}, fact.Timeline{})

	var turns []model.AgentSessionTurn
	if err := db.Where("request_id = ?", "req-rec-1").Find(&turns).Error; err != nil {
		t.Fatalf("read turns: %v", err)
	}
	var haveSystem, haveInput, haveAssistant bool
	for _, row := range turns {
		switch row.EventType {
		case "system_prompt":
			haveSystem = true
		case "user_input":
			haveInput = true
		case "assistant_reply":
			haveAssistant = true
		}
	}
	if !haveSystem || !haveInput || !haveAssistant {
		t.Fatalf("turn batch = %+v, want system_prompt + user_input + assistant_reply rows", turns)
	}
}

// TestRecordTurnFailureNeverBlocksTheAuditWrite pins the ticket's degrade
// rule: with the turn store unreachable (table dropped), Record still
// commits the request_logs and request_log_bodies rows — the failure is a
// logged error on the turn half alone.
func TestRecordTurnFailureNeverBlocksTheAuditWrite(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	rec := New(db)
	testutil.DropTable(t, db, "agent_session_turns")

	v := &turnsView{
		requestID:  "req-rec-2",
		sessionID:  "sess-rec",
		requestBod: `{"messages":[{"role":"user","content":"hello"}]}`,
		responseBd: `{"content":[{"type":"text","text":"hi"}]}`,
	}
	rec.Record(context.Background(), v, fact.Outcome{StatusCode: 200, Delivered: true}, fact.Timeline{})

	var count int64
	if err := db.Model(&model.RequestLog{}).Where("request_id = ?", "req-rec-2").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("request_logs rows = %d (err %v), want 1 — the audit row must survive the turn failure", count, err)
	}
	var bodies int64
	if err := db.Model(&model.RequestLogBody{}).Where("request_id = ?", "req-rec-2").Count(&bodies).Error; err != nil || bodies != 1 {
		t.Fatalf("request_log_bodies rows = %d (err %v), want 1", bodies, err)
	}
}
