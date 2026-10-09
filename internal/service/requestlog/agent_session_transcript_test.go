// Service-layer tests for the transcript composition seam
// (agent_session_transcript.go): the degradation contracts only this layer
// can inject — a failing batched body lookup must degrade every block to
// "not recorded" rather than failing the whole transcript.
package requestlog

import (
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// TestGetAgentSessionTranscriptDegradesWhenBodyBatchFails pins the
// body-batch failure branch: when the batched request_log_bodies lookup
// errors, the transcript still answers with every block carrying its row
// metadata, the bodies reading as "not recorded" — never a whole-transcript
// error. A recorded body row is seeded first (so the failing query is a
// failing lookup over REAL recordings, not an already-empty world), then the
// table is dropped to force the lookup error.
func TestGetAgentSessionTranscriptDegradesWhenBodyBatchFails(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewRequestLogService(db, "")
	base := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	claude := "claude-code"
	sess := "sess-d"

	for i, id := range []string{"req-d1", "req-d2"} {
		testutil.SeedRequestLog(t, db, id, base.Add(time.Duration(i)*time.Minute), func(r *model.RequestLog) {
			r.AgentClient = &claude
			r.AgentSessionID = &sess
			r.StatusCode = 200
		})
	}
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
		RequestID:    "req-d1",
		RequestBody:  `{"messages":[{"role":"user","content":"q"}]}`,
		ResponseBody: `{"content":[{"type":"text","text":"r"}]}`,
	}); err != nil {
		t.Fatalf("upsert body req-d1: %v", err)
	}
	testutil.DropTable(t, db, "request_log_bodies")

	tr, err := svc.GetAgentSessionTranscript(sess)
	if err != nil {
		t.Fatalf("GetAgentSessionTranscript after a body-batch failure: %v, want the degraded transcript (the whole transcript must not fail)", err)
	}
	if len(tr.Blocks) != 2 {
		t.Fatalf("blocks = %d, want 2 (every block survives the body failure)", len(tr.Blocks))
	}
	b := tr.Blocks[0]
	if b.RequestID != "req-d1" || b.StatusClass != repository.StatusSuccess || b.ModelName != "gpt-4o-mini" {
		t.Fatalf("d1 metadata = (%s, %s, %q), want (req-d1, success, gpt-4o-mini) — row metadata survives", b.RequestID, b.StatusClass, b.ModelName)
	}
	if !b.CreatedAt.Equal(base) {
		t.Fatalf("d1 created_at = %v, want %v", b.CreatedAt, base)
	}
	if b.AddedTruncated {
		t.Fatalf("d1 added_truncated = true, want false (nothing was read, so nothing was cut)")
	}
	if len(b.Added) != 0 {
		t.Fatalf("d1 added = %+v, want empty (the body reads as not recorded)", b.Added)
	}
	if b.Assistant.Source != ReplaySourceNone || b.Assistant.Raw != nil {
		t.Fatalf("d1 assistant = %+v, want (none, nil raw)", b.Assistant)
	}
	if tr.Session.RequestCount != 2 || tr.Session.SuccessCount != 2 {
		t.Fatalf("session counts = (%d, %d), want (2, 2) — the aggregate survives", tr.Session.RequestCount, tr.Session.SuccessCount)
	}
}
