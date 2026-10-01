package agentbackfill

import (
	"encoding/json"
	"strings"
	"testing"
)

// Samples below are real captures, spelled the way the database stored
// them. The claude-code user_id strings and the codex snapshot (including
// the masked Authorization and the unmasked turn-metadata blob) are row
// verbatim from a deployment's history; the surrounding body text is
// abridged to the structurally relevant keys, which is all the extractors
// read. Keeping the real spellings here is the point: these tests are the
// guard against a client-side format tweak silently breaking the backfill.
const (
	// Claude Code stamps metadata.user_id as a JSON-encoded string whose
	// payload carries the session; this is the historical row the session
	// backfill exists for.
	claudeUserIDHistorical = `{"device_id":"0c9693a12185905f79f430d2548e996a769a285407e95b20f8340c1264aff6e7","account_uuid":"","session_id":"64148506-29da-4511-8892-034ac0cd18e9"}`
	claudeSessionLiveRow   = "3ef11404-c212-44eb-a3cb-3ea404cd593f"
	claudeUserIDOther      = `{"device_id":"0c9693a12185905f79f430d2548e996a769a285407e95b20f8340c1264aff6e7","account_uuid":"","session_id":"3ef11404-c212-44eb-a3cb-3ea404cd593f"}`

	// The codex turn-metadata blob: session_id and thread_id both present
	// and equal, plus the fields the merge carries but the session never
	// reads.
	codexThreadID = "01a0d24c-4536-7ba1-ba48-c088bd564016"
	codexTurnBlob = `{"session_id":"01a0d24c-4536-7ba1-ba48-c088bd564016","thread_id":"01a0d24c-4536-7ba1-ba48-c088bd564016","thread_source":"user","turn_id":"01a0d24c-46f5-7b03-bf06-0811bb55941e","sandbox":"none","turn_started_at_unix_ms":1790234609398}`
	// The codex body's client_metadata in the same captures: only the
	// installation id — no blob. This is why the header blob is the
	// extraction's primary source.
	codexInstallationID = "701a6154-3da2-43db-bafa-0a3809c4f149"
	// The client_metadata object exactly as those bodies stored it.
	codexInstallationOnlyMetadata = `{"x-codex-installation-id":"701a6154-3da2-43db-bafa-0a3809c4f149"}`

	// A full masked header snapshot exactly as stored for one of those
	// codex rows: the session-bearing turn-metadata header survived whole
	// while the sensitive headers carry the redaction sentinel.
	codexSnapshot = `{"Accept":["text/event-stream"],"Authorization":["[REDACTED]"],` +
		`"Content-Length":["55933"],"Content-Type":["application/json"],` +
		`"Originator":["codex-tui"],"Session_id":["[REDACTED]"],` +
		`"Thread_id":["01a0d24c-4536-7ba1-ba48-c088bd564016"],` +
		`"User-Agent":["codex-tui/0.130.0 (Windows 10.0.19045; x86_64) unknown (codex-tui; 0.130.0)"],` +
		`"X-Client-Request-Id":["01a0d24c-4536-7ba1-ba48-c088bd564016"],` +
		`"X-Codex-Beta-Features":["terminal_resize_reflow"],` +
		`"X-Codex-Turn-Metadata":["{\"session_id\":\"01a0d24c-4536-7ba1-ba48-c088bd564016\",` +
		`\"thread_id\":\"01a0d24c-4536-7ba1-ba48-c088bd564016\",\"thread_source\":\"user\",` +
		`\"turn_id\":\"01a0d24c-46f5-7b03-bf06-0811bb55941e\",\"sandbox\":\"none\",` +
		`\"turn_started_at_unix_ms\":1790234609398}"],` +
		`"X-Codex-Window-Id":["01a0d24c-4536-7ba1-ba48-c088bd564016:0"]}`
)

// claudeBody builds a request body shaped like a stored Claude Code call
// with the given raw JSON value under metadata.user_id.
func claudeBody(userIDJSON string) []byte {
	return []byte(`{"model":"deepseek-ai/DeepSeek-R1",` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],` +
		`"stream":true,` +
		`"metadata":{"user_id":` + userIDJSON + `}}`)
}

// codexBody builds a request body shaped like a stored Codex call. An
// empty clientMetadata omits the key entirely.
func codexBody(clientMetadata string) []byte {
	if clientMetadata == "" {
		return []byte(`{"model":"gpt-5.2","input":[],"stream":true}`)
	}
	return []byte(`{"model":"gpt-5.2","input":[],"stream":true,` +
		`"client_metadata":` + clientMetadata + `}`)
}

// turnSnapshot builds a masked header snapshot whose turn-metadata header
// carries blob; a snapshot with no such header when blob is empty.
func turnSnapshot(blob string) []byte {
	if blob == "" {
		return []byte(`{"Accept":["text/event-stream"],"User-Agent":["codex-tui/0.130.0"]}`)
	}
	return []byte(`{"Accept":["text/event-stream"],"User-Agent":["codex-tui/0.130.0"],` +
		`"X-Codex-Turn-Metadata":[` + quoted(blob) + `]}`)
}

// quoted wraps s as a JSON string literal, escaping whatever s contains
// (embedded quotes above all — the wrapped payloads are usually JSON
// objects themselves). json.Marshal of a string cannot fail.
func quoted(s string) string {
	encoded, _ := json.Marshal(s)
	return string(encoded)
}

func TestExtractClaudeCodeSession(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want string
	}{
		{
			name: "production shape: user_id JSON carries the session uuid",
			body: claudeBody(quoted(claudeUserIDHistorical)),
			want: "64148506-29da-4511-8892-034ac0cd18e9",
		},
		{
			name: "production shape holds across sessions: second captured row",
			body: claudeBody(quoted(claudeUserIDOther)),
			want: claudeSessionLiveRow,
		},
		{
			name: "empty account_uuid in the payload does not disturb the parse",
			body: claudeBody(quoted(`{"account_uuid":"","session_id":"d023769a-37b7-40ed-b308-1161dfbba2f1"}`)),
			want: "d023769a-37b7-40ed-b308-1161dfbba2f1",
		},
		{
			name: "body without the metadata key",
			body: []byte(`{"model":"deepseek-ai/DeepSeek-R1","messages":[]}`),
			want: "",
		},
		{
			name: "metadata object without user_id",
			body: []byte(`{"metadata":{"other":1}}`),
			want: "",
		},
		{
			name: "user_id is a JSON number, not a string",
			body: []byte(`{"metadata":{"user_id":12345}}`),
			want: "",
		},
		{
			name: "user_id string is not JSON",
			body: claudeBody(quoted(`anonymous`)),
			want: "",
		},
		{
			name: "inner JSON lacks session_id",
			body: claudeBody(quoted(`{"device_id":"abc","account_uuid":""}`)),
			want: "",
		},
		{
			name: "inner JSON is an array, not an object",
			body: claudeBody(quoted(`[1,2,3]`)),
			want: "",
		},
		{
			name: "session_id is the empty string",
			body: claudeBody(quoted(`{"session_id":""}`)),
			want: "",
		},
		{
			name: "session_id longer than 128 characters",
			body: claudeBody(quoted(`{"session_id":"` + strings.Repeat("a", 129) + `"}`)),
			want: "",
		},
		{
			name: "session_id of exactly 128 characters is accepted",
			body: claudeBody(quoted(`{"session_id":"` + strings.Repeat("a", 128) + `"}`)),
			want: strings.Repeat("a", 128),
		},
		{
			name: "the 128 limit counts characters, not bytes",
			body: claudeBody(quoted(`{"session_id":"` + strings.Repeat("界", 100) + `"}`)),
			want: strings.Repeat("界", 100),
		},
		{
			name: "session_id containing a control character",
			body: claudeBody(quoted(`{"session_id":"bad\u0000id"}`)),
			want: "",
		},
		{
			name: "session_id containing a newline",
			body: claudeBody(quoted(`{"session_id":"bad\nid"}`)),
			want: "",
		},
		{
			name: "session_id is the redaction sentinel",
			body: claudeBody(quoted(`{"session_id":"[REDACTED]"}`)),
			want: "",
		},
		{
			name: "session_id is trimmed before being accepted",
			body: claudeBody(quoted(`{"session_id":"  64148506-29da-4511-8892-034ac0cd18e9  "}`)),
			want: "64148506-29da-4511-8892-034ac0cd18e9",
		},
		{
			name: "body is not JSON",
			body: []byte("upstream said no"),
			want: "",
		},
		{
			name: "body is empty",
			body: nil,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractClaudeCodeSession(tt.body); got != tt.want {
				t.Fatalf("ExtractClaudeCodeSession() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractCodexSession(t *testing.T) {
	tests := []struct {
		name           string
		body           []byte
		headerSnapshot []byte
		want           string
	}{
		{
			name:           "captured shape: blob in the header snapshot, installation id only in the body",
			body:           codexBody(codexInstallationOnlyMetadata),
			headerSnapshot: []byte(codexSnapshot),
			want:           codexThreadID,
		},
		{
			name:           "blob only in the body client_metadata",
			body:           codexBody(`{"x-codex-turn-metadata":` + quoted(codexTurnBlob) + `}`),
			headerSnapshot: turnSnapshot(""),
			want:           codexThreadID,
		},
		{
			name: "both sources present and agreeing",
			body: codexBody(`{"x-codex-turn-metadata":` + quoted(codexTurnBlob) + `}`),
			// The full real snapshot carries the same blob as the body.
			headerSnapshot: []byte(codexSnapshot),
			want:           codexThreadID,
		},
		{
			name:           "both sources present and disagreeing: the header blob wins",
			body:           codexBody(`{"x-codex-turn-metadata":` + quoted(`{"session_id":"body-sess","thread_id":"body-thread"}`) + `}`),
			headerSnapshot: turnSnapshot(`{"session_id":"head-sess","thread_id":"head-thread"}`),
			want:           "head-thread",
		},
		{
			name:           "body blob fills a field the header blob left absent",
			body:           codexBody(`{"x-codex-turn-metadata":` + quoted(`{"thread_id":"body-thread"}`) + `}`),
			headerSnapshot: turnSnapshot(`{"session_id":"head-sess"}`),
			want:           "body-thread",
		},
		{
			name:           "installation id only, no blob anywhere",
			body:           codexBody(codexInstallationOnlyMetadata),
			headerSnapshot: turnSnapshot(""),
			want:           "",
		},
		{
			name:           "client_metadata is a string, not an object",
			body:           codexBody(quoted(`nope`)),
			headerSnapshot: turnSnapshot(""),
			want:           "",
		},
		{
			name:           "client_metadata object without any codex key is not the tool's",
			body:           codexBody(`{"session_id":"someone-elses","thread_id":"someone-elses"}`),
			headerSnapshot: turnSnapshot(""),
			want:           "",
		},
		{
			name:           "no blob and no client_metadata",
			body:           codexBody(""),
			headerSnapshot: turnSnapshot(""),
			want:           "",
		},
		{
			name:           "empty snapshot",
			body:           codexBody(codexInstallationOnlyMetadata),
			headerSnapshot: nil,
			want:           "",
		},
		{
			name: "flat session_id never becomes the session",
			body: codexBody(`{"session_id":"flat-session","x-codex-installation-id":"` + codexInstallationID + `"}`),
			// A blob without thread_id, so only the flat keys could speak.
			headerSnapshot: turnSnapshot(`{"session_id":"blob-session","turn_id":"t"}`),
			want:           "",
		},
		{
			name: "flat thread_id fills a missing thread field",
			body: codexBody(`{"thread_id":"flat-thread","x-codex-installation-id":"` + codexInstallationID + `"}`),
			// The header blob exists but carries no thread_id.
			headerSnapshot: turnSnapshot(`{"session_id":"blob-session"}`),
			want:           "flat-thread",
		},
		{
			name: "flat thread_id does not override the blob's thread field",
			body: codexBody(`{"thread_id":"flat-thread","x-codex-installation-id":"` + codexInstallationID + `"}`),
			// The header blob already carries a thread_id; the flat key
			// must not replace it.
			headerSnapshot: turnSnapshot(`{"thread_id":"blob-thread"}`),
			want:           "blob-thread",
		},
		{
			name:           "flat thread_id alone, no blob at all",
			body:           codexBody(`{"thread_id":"flat-thread","x-codex-installation-id":"` + codexInstallationID + `"}`),
			headerSnapshot: turnSnapshot(""),
			want:           "flat-thread",
		},
		{
			name:           "lowercase snapshot spelling resolves the same",
			body:           codexBody(codexInstallationOnlyMetadata),
			headerSnapshot: []byte(`{"x-codex-turn-metadata":[` + quoted(codexTurnBlob) + `]}`),
			want:           codexThreadID,
		},
		{
			name:           "repeated turn-metadata header counts as absent",
			body:           codexBody(codexInstallationOnlyMetadata),
			headerSnapshot: []byte(`{"X-Codex-Turn-Metadata":[` + quoted(codexTurnBlob) + `,` + quoted(codexTurnBlob) + `]}`),
			want:           "",
		},
		{
			name:           "header value is not JSON",
			body:           codexBody(codexInstallationOnlyMetadata),
			headerSnapshot: turnSnapshot(`garbage`),
			want:           "",
		},
		{
			name:           "header value is JSON but not an object",
			body:           codexBody(codexInstallationOnlyMetadata),
			headerSnapshot: turnSnapshot(`[1,2,3]`),
			want:           "",
		},
		{
			name:           "client_metadata blob entry is an object, not a string",
			body:           codexBody(`{"x-codex-turn-metadata":{"thread_id":"nested"}}`),
			headerSnapshot: turnSnapshot(""),
			want:           "",
		},
		{
			name:           "thread_id equal to the redaction sentinel",
			body:           codexBody(codexInstallationOnlyMetadata),
			headerSnapshot: turnSnapshot(`{"thread_id":"[REDACTED]"}`),
			want:           "",
		},
		{
			name:           "thread_id containing a control character",
			body:           codexBody(codexInstallationOnlyMetadata),
			headerSnapshot: turnSnapshot(`{"thread_id":"bad\u0001id"}`),
			want:           "",
		},
		{
			name:           "thread_id longer than 128 characters",
			body:           codexBody(codexInstallationOnlyMetadata),
			headerSnapshot: turnSnapshot(`{"thread_id":"` + strings.Repeat("a", 129) + `"}`),
			want:           "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractCodexSession(tt.body, tt.headerSnapshot); got != tt.want {
				t.Fatalf("ExtractCodexSession() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractAgentSessionDispatchesByTool(t *testing.T) {
	claude := claudeBody(quoted(claudeUserIDHistorical))
	codex := codexBody(codexInstallationOnlyMetadata)

	tests := []struct {
		name   string
		client string
		body   []byte
		snap   []byte
		want   string
	}{
		{
			name:   "claude-code routes to the body rule",
			client: "claude-code",
			body:   claude,
			want:   "64148506-29da-4511-8892-034ac0cd18e9",
		},
		{
			name:   "codex routes to the dual-source rule",
			client: "codex",
			body:   codex,
			snap:   []byte(codexSnapshot),
			want:   codexThreadID,
		},
		{
			name:   "a tool with no rule gets nothing, even from a claude-shaped body",
			client: "opencode",
			body:   claude,
			want:   "",
		},
		{
			name:   "unknown tool name gets nothing",
			client: "curl",
			body:   claude,
			snap:   []byte(codexSnapshot),
			want:   "",
		},
		{
			name:   "empty tool name gets nothing",
			client: "",
			body:   claude,
			want:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractAgentSession(tt.client, tt.body, tt.snap); got != tt.want {
				t.Fatalf("ExtractAgentSession(%q) = %q, want %q", tt.client, got, tt.want)
			}
		})
	}
}

// The merge contract's sharpest edge, pinned on its own: the session value
// comes from thread_id only. A blob carrying BOTH ids (every real capture
// does) still attributes thread_id, and a blob carrying ONLY session_id
// attributes nothing — the session_id key is a projection field, never the
// session, no matter which source or flat key supplied it.
func TestExtractCodexSessionAlwaysTakesTheThreadID(t *testing.T) {
	if got := ExtractCodexSession(codexBody(codexInstallationOnlyMetadata), []byte(codexSnapshot)); got != codexThreadID {
		t.Fatalf("dual-id blob: got %q, want %q", got, codexThreadID)
	}
	sessionOnly := turnSnapshot(`{"session_id":"only-session","turn_id":"t"}`)
	if got := ExtractCodexSession(codexBody(codexInstallationOnlyMetadata), sessionOnly); got != "" {
		t.Fatalf("session-only blob: got %q, want no result", got)
	}
	// Different values in the two keys: thread_id still wins as the value.
	disagreeing := turnSnapshot(`{"session_id":"a","thread_id":"b"}`)
	if got := ExtractCodexSession(codexBody(codexInstallationOnlyMetadata), disagreeing); got != "b" {
		t.Fatalf("disagreeing ids: got %q, want %q", got, "b")
	}
}

// Redaction sentinel defense on the codex path: a masked session_id
// anywhere in the merge never leaks into the result — only thread_id is
// ever read, and the cleaner would reject the sentinel anyway.
func TestExtractCodexSessionRejectsMaskedValues(t *testing.T) {
	maskedBoth := turnSnapshot(`{"session_id":"[REDACTED]","thread_id":"[REDACTED]"}`)
	if got := ExtractCodexSession(nil, maskedBoth); got != "" {
		t.Fatalf("masked thread_id: got %q, want no result", got)
	}
	maskedSessionOnly := turnSnapshot(`{"session_id":"[REDACTED]","thread_id":"real-thread"}`)
	if got := ExtractCodexSession(nil, maskedSessionOnly); got != "real-thread" {
		t.Fatalf("masked session_id with real thread_id: got %q, want real-thread", got)
	}
}
