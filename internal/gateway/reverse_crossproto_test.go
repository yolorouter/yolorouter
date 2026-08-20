package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// TestMultiProtocolProvider_BothIngresses_RouteToCorrectPerProtocolUpstream
// is the end-to-end (HTTP-level) counterpart to negotiate_test.go's
// TestNegotiate_MultiProtocolProvider_BothIngressesPassthroughWithProviderBaseURL
// and TestNegotiate_PerProtocolBaseURL_UsedWhenNonEmpty: those tests only
// exercise Negotiate() as a pure function; this one drives the full
// svc.Handle pipeline against ONE openai-type provider that also declares a
// per-protocol Anthropic endpoint (protocol_endpoints), and proves the
// support-set union (T5) actually routes bytes to the right upstream host —
// not just that Negotiate() returns the right decision struct.
//
// One provider, one key, one model/candidate. An OpenAI-ingress request must
// reach the provider's primary BaseURL (the OpenAI upstream); a
// Claude-ingress request (/v1/messages) against the SAME provider must reach
// the per-protocol Anthropic URL from protocol_endpoints instead — proving
// negotiation and dispatch pick the base URL per-request, not once per
// provider.
func TestMultiProtocolProvider_BothIngresses_RouteToCorrectPerProtocolUpstream(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	var (
		openaiHit, claudeHit   bool
		claudeAuthHeader       string
		claudeAuthHeaderPrefix string
	)
	openaiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openaiHit = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"provider-model","choices":[{"message":{"role":"assistant","content":"openai reply"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer openaiUpstream.Close()

	claudeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claudeHit = true
		claudeAuthHeader = r.Header.Get("x-api-key")
		claudeAuthHeaderPrefix = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_01","type":"message","role":"assistant","model":"provider-model","content":[{"type":"text","text":"claude reply"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	defer claudeUpstream.Close()

	svc := newSvc(t, db)
	now := time.Now().UTC()
	p := &model.Provider{
		Name: "multi-protocol-provider", ProviderType: "openai", BaseURL: openaiUpstream.URL,
		ProtocolEndpoints: `{"anthropic":"` + claudeUpstream.URL + `"}`,
		ManagementStatus:  model.ProviderStatusEnabled, DestinationVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(p).Error; err != nil {
		t.Fatalf("seed multi-protocol provider: %v", err)
	}
	createProviderKey(t, db, svc.secrets, p.ID, "sk-multi-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "provider-model", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	// --- OpenAI ingress: must land on the provider's primary BaseURL ---
	openaiReqBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	c, w := newCtx(openaiReqBody)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("openai-ingress status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if !openaiHit {
		t.Error("openai upstream was not hit for an openai-ingress request")
	}
	if claudeHit {
		t.Error("claude upstream was hit for an openai-ingress request, want only the openai upstream")
	}
	var openaiResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &openaiResp); err != nil {
		t.Fatalf("openai-ingress client response not valid JSON: %v; body=%s", err, w.Body.String())
	}
	if len(openaiResp.Choices) != 1 || openaiResp.Choices[0].Message.Content != "openai reply" {
		t.Errorf("openai-ingress client response = %+v, want the openai upstream's reply", openaiResp)
	}

	// Reset hit flags before driving the second ingress against the same
	// provider/model/key.
	openaiHit, claudeHit = false, false

	// --- Claude ingress (/v1/messages): must land on the per-protocol
	// Anthropic URL from protocol_endpoints, NOT the provider's BaseURL. ---
	claudeReqBody := []byte(`{"model":"gpt-4o","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)
	c2, w2 := newCtxPath("/v1/messages", claudeReqBody)
	svc.Handle(c2, apiKey)

	if w2.Code != http.StatusOK {
		t.Fatalf("claude-ingress status = %d, want 200; body = %s", w2.Code, w2.Body.String())
	}
	if !claudeHit {
		t.Error("claude upstream (per-protocol endpoint) was not hit for a claude-ingress request")
	}
	if openaiHit {
		t.Error("openai upstream was hit for a claude-ingress request, want only the per-protocol claude upstream")
	}
	var claudeResp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &claudeResp); err != nil {
		t.Fatalf("claude-ingress client response not valid JSON: %v; body=%s", err, w2.Body.String())
	}
	if len(claudeResp.Content) != 1 || claudeResp.Content[0].Text != "claude reply" {
		t.Errorf("claude-ingress client response = %+v, want the claude upstream's reply", claudeResp)
	}

	// --- Report (do not silently pass) what auth header the claude
	// passthrough upstream actually received. relay.go's tryKeys/attemptOne
	// calls codecsFor(egress.Protocol).RequestEncoder.SetupRequest, and for a
	// claude-passthrough egress that Protocol IS protocols.ProtocolClaude
	// (Negotiate sets Protocol: ingress on the passthrough branch), so the
	// claude RequestEncoder.SetupRequest runs and sets x-api-key -- unlike a
	// naive "passthrough always sends the ingress protocol's own Authorization:
	// Bearer" implementation would. This assertion pins the OBSERVED
	// behavior; see the task report for discussion.
	if claudeAuthHeader != "sk-multi-upstream" {
		t.Errorf("claude passthrough upstream x-api-key = %q, want the provider key %q (observed Authorization header instead = %q)",
			claudeAuthHeader, "sk-multi-upstream", claudeAuthHeaderPrefix)
	}
}

// TestDestinationVersionBump_RevokesStaleKeyUntilReVerified is the
// end-to-end proof of the credential-scope invariant relay.go:472 enforces:
// a provider key is only usable while its authorized_destination_version
// matches the provider's current destination_version. Changing a provider's
// protocol/endpoint configuration (repository.UpdateProviderProtocol) bumps
// destination_version, which immediately makes every existing key
// ineligible -- even though the key's ciphertext and the provider's
// reachable upstream haven't changed -- until an operator re-verifies the
// key (bumping its authorized_destination_version to match). This prevents
// a key that was only ever verified against destination A from silently
// being sent to a newly-configured destination B.
func TestDestinationVersionBump_RevokesStaleKeyUntilReVerified(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_01","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-3-5-sonnet-20241022","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createAnthropicProvider(t, db, "claude-provider", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-claude-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "claude-3-5-sonnet-20241022", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	reqBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	// Sanity: the key is authorized at destination_version=1 and routes
	// successfully before anything changes.
	c, w := newCtx(reqBody)
	svc.Handle(c, apiKey)
	if w.Code != http.StatusOK {
		t.Fatalf("sanity request status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	// Bump destination_version by changing the provider's protocol
	// configuration. The existing key stays at
	// authorized_destination_version=1, now stale relative to the
	// provider's new destination_version.
	newVersion, err := repository.UpdateProviderProtocol(db, p.ID, "anthropic", `{"responses":"https://gw/v1"}`, time.Now().UTC())
	if err != nil {
		t.Fatalf("UpdateProviderProtocol: %v", err)
	}
	if newVersion != 2 {
		t.Fatalf("destination_version after UpdateProviderProtocol = %d, want 2", newVersion)
	}

	// Re-run the SAME request: relay.go:472 skips the stale key
	// (AuthorizedDestinationVersion=1 != provider.DestinationVersion=2), so
	// with no other key on the candidate the whole request fails.
	c2, w2 := newCtx(reqBody)
	svc.Handle(c2, apiKey)
	if w2.Code == http.StatusOK {
		t.Fatalf("expected routing to fail after the destination_version bump (stale key must not be used), got 200; body = %s", w2.Body.String())
	}
	if w2.Code != http.StatusBadGateway {
		t.Errorf("status after destination_version bump = %d, want 502 (all upstream candidates failed -- matches TestRelayAllCandidatesFailed's shape)", w2.Code)
	}
	var log2 model.RequestLog
	if err := db.Order("id DESC").First(&log2).Error; err != nil {
		t.Fatalf("no request_log row for the post-bump request: %v", err)
	}
	if log2.StatusCode != http.StatusBadGateway {
		t.Errorf("post-bump log status_code = %d, want 502", log2.StatusCode)
	}

	// Simulate re-verification: an operator (or the verification worker)
	// re-checks the key against the provider's new configuration and bumps
	// its authorized_destination_version to match.
	var key model.ProviderKey
	if err := db.Where("provider_id = ?", p.ID).First(&key).Error; err != nil {
		t.Fatalf("load provider key: %v", err)
	}
	if err := db.Model(&key).Updates(map[string]any{"authorized_destination_version": 2}).Error; err != nil {
		t.Fatalf("simulate re-verification: %v", err)
	}

	// Routing must succeed again now that the key's authorized destination
	// version matches the provider's current one.
	c3, w3 := newCtx(reqBody)
	svc.Handle(c3, apiKey)
	if w3.Code != http.StatusOK {
		t.Fatalf("status after re-verification = %d, want 200; body = %s", w3.Code, w3.Body.String())
	}
}
