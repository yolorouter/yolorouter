package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yolorouter/yolorouter/internal/capability/requestlog"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
	ycrypto "github.com/yolorouter/yolorouter/pkg/crypto"
)

// The recorder-level arm of the agent-attribution write path, in the same
// posture as TestTheAuditRowCarriesTheCallerTraceID: the recognizer
// matrices in agent_client_test.go stop at the pure function, this file
// proves the whole kernel side of the chain — Handle entry captures the
// masked snapshot, the recognizer reads it, the View reports the results,
// and the recorder persists them as nullable columns over a real SQLite
// database.

// fireAgentProbeExchange runs one exchange through the kernel the same way
// fireTracingProbeExchange does: a body that fails validation, so no
// upstream or candidate is involved, and the recorder reads the finished
// exchange exactly the way production assembly wires it. userAgent and the
// sessionHeader name/value pair are set as request headers when non-empty
// and left unset when empty — those are the only inputs that differ between
// the arms below.
func fireAgentProbeExchange(t *testing.T, userAgent, sessionHeader, sessionID string) *model.RequestLog {
	t.Helper()

	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called for a body that fails validation")
	}))
	defer upstream.Close()

	masterKey := bytes.Repeat([]byte{0x42}, 32)
	svc := NewService(testStoreFrom(db), stubVideoTasks{}, ycrypto.NewSecretBox(masterKey), false, stubSettingsProvider{}, testGatewayConfig())
	svc.client.httpClient.Transport = &http.Transport{}
	RegisterRecorder(svc, requestlog.New(db), func(e *Exchange) requestlog.View { return e })

	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "claude-3-5-sonnet", "claude-3-5-sonnet-real", true, true, 1)
	apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

	// Passes the cheap meta check (non-empty messages, positive
	// max_tokens), fails the full decoder (content is an object, not a
	// string or block array) — a 400 that never reaches a candidate, so
	// the row it leaves is about capture, not about relaying.
	body := []byte(`{"model":"claude-3-5-sonnet","max_tokens":1024,"messages":[{"role":"user","content":{"foo":"bar"}}]}`)
	c, w := newCtxPath("/v1/messages", body)
	if userAgent != "" {
		c.Request.Header.Set("User-Agent", userAgent)
	}
	if sessionHeader != "" && sessionID != "" {
		c.Request.Header.Set(sessionHeader, sessionID)
	}
	svc.Handle(c, apiKey)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the validation 400 unchanged by agent attribution; body = %s", w.Code, w.Body.String())
	}
	var row model.RequestLog
	if err := db.First(&row).Error; err != nil {
		t.Fatalf("no request_log row: %v", err)
	}
	return &row
}

// TestTheAuditRowCarriesTheAgentAttribution is the write-half data-layer
// arm: a request carrying a claude-cli User-Agent and a tool session header
// comes out with agent_client naming the tool and agent_session_id holding
// the caller's own id — the real value, not the sanitizer's sentinel, which
// the allowlist kept out of the way. SQL NULL, never the empty string, is
// what an unattributed request stores.
func TestTheAuditRowCarriesTheAgentAttribution(t *testing.T) {
	const sessionID = "3f9d2c81-6a54-4d0f-9a3e-2b1c8d7e5a4f"
	t.Run("a claude-cli caller with a session header", func(t *testing.T) {
		row := fireAgentProbeExchange(t, "claude-cli/2.1.1 (external, cli)", "x-claude-code-session-id", sessionID)
		if row.AgentClient == nil {
			t.Fatal("request_log.agent_client is NULL, want claude-code from the User-Agent")
		}
		if *row.AgentClient != "claude-code" {
			t.Errorf("request_log.agent_client = %q, want %q", *row.AgentClient, "claude-code")
		}
		if row.AgentSessionID == nil {
			t.Fatal("request_log.agent_session_id is NULL, want the session id the tool sent")
		}
		if *row.AgentSessionID != sessionID {
			t.Errorf("request_log.agent_session_id = %q, want %q", *row.AgentSessionID, sessionID)
		}
		if *row.AgentSessionID == redactedHeaderValue {
			t.Errorf("request_log.agent_session_id holds the redaction sentinel %q — the allowlist must keep the real value flowing", redactedHeaderValue)
		}
	})
	t.Run("a caller with no agent signature", func(t *testing.T) {
		row := fireAgentProbeExchange(t, "", "", "")
		if row.AgentClient != nil {
			t.Errorf("request_log.agent_client = %q, want NULL: no tool identified itself", *row.AgentClient)
		}
		if row.AgentSessionID != nil {
			t.Errorf("request_log.agent_session_id = %q, want NULL: no tool identified itself", *row.AgentSessionID)
		}
	})
	t.Run("a session header without a recognizable tool", func(t *testing.T) {
		// A generic x-session-id alone must not attribute anything: the
		// columns mean "a recognized tool identified itself", so a session
		// id without a tool is not written either.
		row := fireAgentProbeExchange(t, "", "x-session-id", sessionID)
		if row.AgentClient != nil {
			t.Errorf("request_log.agent_client = %q, want NULL: a session header is not a tool signature", *row.AgentClient)
		}
		if row.AgentSessionID != nil {
			t.Errorf("request_log.agent_session_id = %q, want NULL: no tool was recognized for it", *row.AgentSessionID)
		}
	})
}

// TestTheAuditRowSnapshotKeepsTheToolSessionReadable pins the capture the
// attribution was derived from: the persisted request_log_bodies snapshot
// for an attributed request still carries the session id in plaintext (the
// allowlist's doing) while the masking sentinel never appears in the
// agent's own headers — the column above is re-derivable from the capture
// stored beside it, which is the whole reason the recognizer reads the
// snapshot and not the live request.
func TestTheAuditRowSnapshotKeepsTheToolSessionReadable(t *testing.T) {
	const sessionID = "1a2b3c4d-5e6f-4a5b-8c9d-0e1f2a3b4c5d"
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called for a body that fails validation")
	}))
	defer upstream.Close()

	masterKey := bytes.Repeat([]byte{0x42}, 32)
	svc := NewService(testStoreFrom(db), stubVideoTasks{}, ycrypto.NewSecretBox(masterKey), false, stubSettingsProvider{}, testGatewayConfig())
	svc.client.httpClient.Transport = &http.Transport{}
	RegisterRecorder(svc, requestlog.New(db), func(e *Exchange) requestlog.View { return e })

	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "claude-3-5-sonnet", "claude-3-5-sonnet-real", true, true, 1)
	apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

	body := []byte(`{"model":"claude-3-5-sonnet","max_tokens":1024,"messages":[{"role":"user","content":{"foo":"bar"}}]}`)
	c, w := newCtxPath("/v1/messages", body)
	c.Request.Header.Set("User-Agent", "claude-cli/2.1.1 (external, cli)")
	c.Request.Header.Set("x-claude-code-session-id", sessionID)
	svc.Handle(c, apiKey)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the validation 400; body = %s", w.Code, w.Body.String())
	}

	var row model.RequestLog
	if err := db.First(&row).Error; err != nil {
		t.Fatalf("no request_log row: %v", err)
	}
	var bodyRow model.RequestLogBody
	if err := db.First(&bodyRow).Error; err != nil {
		t.Fatalf("no request_log_bodies row: %v", err)
	}
	if !strings.Contains(bodyRow.RequestHeaders, sessionID) {
		t.Fatalf("the stored header snapshot lost the tool session id — the attribution column would no longer be re-derivable from the capture: %s", bodyRow.RequestHeaders)
	}
}
