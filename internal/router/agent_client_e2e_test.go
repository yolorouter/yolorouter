package router

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/requestlog"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// The three arms of the agent-attribution persistence contract, each driven
// through the real engine (RequestID → AccessLog → Recovery → Timezone →
// BodySizeLimit → APIKeyAuth → gateway Handle → requestlog recorder)
// against a freshly migrated temp SQLite database, with a loopback httptest
// server standing in for the upstream model provider — the same harness
// traceparent_e2e_test.go drives:
//
//	1. a claude-cli User-Agent + tool session header → business 200,
//	   agent_client = claude-code and agent_session_id = the id the tool
//	   sent (the real value, not the redaction sentinel), the list filter
//	   the logs page compiles to finds the row, and the detail
//	   serialization carries both values verbatim
//	2. no agent signature at all → business 200, both columns NULL, the
//	   filter misses the row, the detail fields flatten to ""
//	3. rejected at the auth gate → 401, the audit row's agent columns NULL
//	   despite the full signature — the second request_logs writer must
//	   never learn about agent attribution
//
// The value under test is the whole chain, not the recognizer: the headers
// go in as an HTTP request, and what comes out are columns, a filter hit,
// and a detail payload. The gateway package's own tests cover the
// recognizer and the recorder in isolation; these three pin the assembled
// router.

// The session id a Claude Code caller stamps on every request, and the UA
// prefix it is recognized from.
const (
	claudeSessionID = "0d51e7b2-9c40-4f8a-a6d3-1e5b7c9f2a48"
	claudeCLIUA     = "claude-cli/2.1.1 (external, cli)"
)

// newAgentE2EDB hands each arm its own freshly migrated temp SQLite
// database — the empty-database full-migration proof that the agent columns
// exist comes free with the schema source.
func newAgentE2EDB(t *testing.T) *gorm.DB {
	t.Helper()
	return testutil.NewSQLiteDB(t)
}

// postAgentChat sends one chat completion through the engine the way a
// coding-agent tool would, optionally carrying a User-Agent and a
// tool-session header ("" sends neither), and returns the recorded response
// together with the request id the router attributed to it — the id shared
// by the X-Request-Id response header, the gateway's audit row, and the
// auth middleware's rejection row.
func postAgentChat(r *gin.Engine, apiKey, userAgent, sessionHeader, sessionID string) (*httptest.ResponseRecorder, string) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	if sessionHeader != "" && sessionID != "" {
		req.Header.Set(sessionHeader, sessionID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w, w.Header().Get("X-Request-Id")
}

// listByAgentClient runs the exact query the logs page's agent_client
// filter compiles to (repository.ListRequestLogs over the shared filter
// shape) and returns the request ids it selected plus the total.
func listByAgentClient(t *testing.T, db *gorm.DB, client string) (ids []string, total int64) {
	t.Helper()
	c := client
	rows, total, err := repository.ListRequestLogs(db, &repository.RequestLogFilter{AgentClient: &c})
	if err != nil {
		t.Fatalf("ListRequestLogs(agent_client=%q): %v", client, err)
	}
	for i := range rows {
		ids = append(ids, rows[i].RequestID)
	}
	return ids, total
}

// agentDetail fetches the detail serialization the detail page renders, the
// same service call the detail endpoint serves.
func agentDetail(t *testing.T, db *gorm.DB, requestID string) *requestlog.RequestLogDetail {
	t.Helper()
	detail, err := requestlog.NewRequestLogService(db).GetRequestLogDetail(requestID)
	if err != nil {
		t.Fatalf("GetRequestLogDetail(%q): %v", requestID, err)
	}
	return detail
}

// countBothAgentColumnsNull counts this request's rows whose both agent
// columns are NULL, so the NULL semantics are asserted in SQL, not just
// through the struct mapping.
func countBothAgentColumnsNull(t *testing.T, db *gorm.DB, requestID string) int64 {
	t.Helper()
	var n int64
	if err := db.Raw(`SELECT COUNT(*) FROM request_logs WHERE request_id = ? AND agent_client IS NULL AND agent_session_id IS NULL`, requestID).Scan(&n).Error; err != nil {
		t.Fatalf("count NULL agent columns: %v", err)
	}
	return n
}

// Arm 1 — a tool-attributed caller's name and session id land in their own
// queryable columns, the filter built on agent_client finds exactly that
// row, and the detail serialization carries both values verbatim.
func TestAgentAttributedRequestPersistsFiltersAndSerializes(t *testing.T) {
	db := newAgentE2EDB(t)
	upstream, hits := newUpstream(t)
	m := seedRelayableModel(t, db, upstream.URL)
	seedCallerKey(t, db, m, "sk-yr-agent-e2e")
	r := newRelayTestRouter(t, db)

	w, requestID := postAgentChat(r, "sk-yr-agent-e2e", claudeCLIUA, "x-claude-code-session-id", claudeSessionID)

	if w.Code != http.StatusOK {
		t.Fatalf("expected the attributed request to relay normally (200), got %d, body: %s", w.Code, w.Body.String())
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("expected the relay to reach the upstream exactly once, got %d", n)
	}
	row := latestRequestLog(t, db)
	if row.RequestID != requestID {
		t.Fatalf("latest request_logs row is not this request's row: row request_id %q, response X-Request-Id %q", row.RequestID, requestID)
	}
	if row.AgentClient == nil || *row.AgentClient != "claude-code" {
		t.Fatalf("expected agent_client = claude-code, got %#v", row.AgentClient)
	}
	if row.AgentSessionID == nil || *row.AgentSessionID != claudeSessionID {
		t.Fatalf("expected agent_session_id = %q, got %#v", claudeSessionID, row.AgentSessionID)
	}

	// The query the logs page's client-tool dropdown compiles to: exact
	// equality on agent_client. It must select exactly this request.
	ids, total := listByAgentClient(t, db, "claude-code")
	if total != 1 || len(ids) != 1 || ids[0] != requestID {
		t.Fatalf("agent_client=claude-code filter = (%v, total=%d), want exactly [%s]", ids, total, requestID)
	}

	// The detail serialization the detail page renders: both fields
	// verbatim, the session id the real value the tool sent.
	detail := agentDetail(t, db, requestID)
	if detail.AgentClient != "claude-code" {
		t.Fatalf("detail agent_client = %q, want claude-code", detail.AgentClient)
	}
	if detail.AgentSessionID != claudeSessionID {
		t.Fatalf("detail agent_session_id = %q, want %q", detail.AgentSessionID, claudeSessionID)
	}
}

// Arm 2 — a caller with no agent signature changes nothing except the two
// columns. The request still relays normally, both columns read NULL in
// SQL, the client-tool filter misses the row, and the detail fields flatten
// to "" (the shape the detail page's v-if hides the rows on).
func TestUnattributedRequestLeavesAgentColumnsNullAndUnmatched(t *testing.T) {
	db := newAgentE2EDB(t)
	upstream, hits := newUpstream(t)
	m := seedRelayableModel(t, db, upstream.URL)
	seedCallerKey(t, db, m, "sk-yr-agent-e2e")
	r := newRelayTestRouter(t, db)

	// No User-Agent, no session header — a plain HTTP caller.
	w, requestID := postAgentChat(r, "sk-yr-agent-e2e", "", "", "")

	if w.Code != http.StatusOK {
		t.Fatalf("expected the unattributed request to relay normally (200), got %d, body: %s", w.Code, w.Body.String())
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("expected the request to reach the upstream, got %d hits", n)
	}
	row := latestRequestLog(t, db)
	if row.RequestID != requestID {
		t.Fatalf("latest request_logs row is not this request's row: row request_id %q, response X-Request-Id %q", row.RequestID, requestID)
	}
	if row.AgentClient != nil {
		t.Fatalf("expected agent_client to be NULL for an unattributed request, got %q", *row.AgentClient)
	}
	if row.AgentSessionID != nil {
		t.Fatalf("expected agent_session_id to be NULL for an unattributed request, got %q", *row.AgentSessionID)
	}
	if n := countBothAgentColumnsNull(t, db, requestID); n != 1 {
		t.Fatalf("expected exactly 1 row with both agent columns NULL, got %d", n)
	}

	// The filter the logs page offers does not match this request, and the
	// detail serialization flattens both fields to "".
	if _, total := listByAgentClient(t, db, "claude-code"); total != 0 {
		t.Fatalf("agent_client=claude-code filter matched %d rows, want 0: an unattributed request must not match", total)
	}
	detail := agentDetail(t, db, requestID)
	if detail.AgentClient != "" || detail.AgentSessionID != "" {
		t.Fatalf("unattributed detail agent fields = (%q, %q), want empty", detail.AgentClient, detail.AgentSessionID)
	}
}

// Arm 3 — the second request_logs writer must never learn about agent
// attribution. A request rejected at the auth gate never reaches the
// gateway kernel; its audit row is written by the API-key middleware's own
// rejection path, which this test pins to NULL on both columns even though
// the request carried the full tool signature — the guard against a future
// change that teaches that path to extract the headers. The upstream must
// stay untouched: rejection happens before any relay.
func TestAuthRejectedRequestAuditRowStaysNullDespiteAgentSignature(t *testing.T) {
	db := newAgentE2EDB(t)
	upstream, hits := newUpstream(t)
	m := seedRelayableModel(t, db, upstream.URL)
	seedCallerKey(t, db, m, "sk-yr-agent-e2e")
	r := newRelayTestRouter(t, db)

	// Full tool signature, key that was never seeded.
	w, requestID := postAgentChat(r, "sk-yr-never-seeded", claudeCLIUA, "x-claude-code-session-id", claudeSessionID)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an unknown API key, got %d, body: %s", w.Code, w.Body.String())
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("an auth-rejected request must never reach the upstream, got %d hits", n)
	}
	row := latestRequestLog(t, db)
	if row.RequestID != requestID {
		t.Fatalf("latest request_logs row is not this request's row: row request_id %q, response X-Request-Id %q", row.RequestID, requestID)
	}
	if row.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected the audit row to record the 401, got %d", row.StatusCode)
	}
	if row.FailReason == nil || *row.FailReason != "invalid API key" {
		t.Fatalf("expected the auth-rejection audit row (fail_reason \"invalid API key\"), got %#v", row.FailReason)
	}
	if row.AgentClient != nil {
		t.Fatalf("the auth-rejection audit row must stay NULL on agent_client even with a full tool signature, got %q", *row.AgentClient)
	}
	if row.AgentSessionID != nil {
		t.Fatalf("the auth-rejection audit row must stay NULL on agent_session_id even with a full tool signature, got %q", *row.AgentSessionID)
	}
	if n := countBothAgentColumnsNull(t, db, requestID); n != 1 {
		t.Fatalf("expected exactly 1 row with both agent columns NULL, got %d", n)
	}
}
