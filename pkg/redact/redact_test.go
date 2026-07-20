package redact

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestRedactBytes(t *testing.T) {
	callerKey := "sk-yr-aabbccddeeff00112233445566778899"
	tests := []struct {
		name string
		body string
		want string // substring assertions checked below
	}{
		{"caller key exact", "msg with " + callerKey + " pasted", "[REDACTED]"},
		{"openai-style sk-", `{"key":"sk-proj-AAAAAAAAAAAAAAAAAAAAAAAA"}`, "[REDACTED]"},
		{"bearer", "Authorization: Bearer sk-something-secret-1234567890", "[REDACTED]"},
		{"authorization header", "Authorization: Bearer abc123", "[REDACTED]"},
		{"json api_key field", `{"api_key":"sk-live-xxxxxxxxxxxxxxxxxxxxxxxx","model":"gpt-4o"}`, `"[REDACTED]"`},
		{"json secret field", `{"secret":"hunter2hunter2hunter2hunter2","x":1}`, `"[REDACTED]"`},
		{"non-json passthrough", "just a normal chat message", "just a normal chat message"},
		{"idempotent", "Bearer sk-abc1234567890", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactBytes([]byte(tt.body), callerKey)
			if tt.want == "" {
				// idempotent: redact twice == redact once
				again := RedactBytes(got, callerKey)
				if !bytes.Equal(got, again) {
					t.Fatalf("not idempotent:\nfirst=%s\nsecond=%s", got, again)
				}
				return
			}
			if !bytes.Contains(got, []byte(tt.want)) {
				t.Fatalf("%s: expected to contain %q, got %s", tt.name, tt.want, got)
			}
			if bytes.Contains(got, []byte(callerKey)) {
				t.Fatalf("%s: caller key leaked: %s", tt.name, got)
			}
		})
	}
}

func TestRedactBytesNonJSON(t *testing.T) {
	// invalid JSON falls back to regex-only redaction, never errors
	in := []byte(`{"bad json`)
	got := RedactBytes(in, "")
	if !bytes.Contains(got, []byte("bad json")) {
		t.Fatalf("corrupted non-redacted content unexpectedly altered: %s", got)
	}
}

func TestRedactJSONFieldTypes(t *testing.T) {
	// ensure JSON round-trip preserves non-redacted fields and types
	in := `{"model":"gpt-4o","api_key":"sk-leak-AAAAAAAAAAAAAAAAAAAA","n":3}`
	out := RedactBytes([]byte(in), "")
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not valid JSON: %v (out=%s)", err, out)
	}
	if m["api_key"] != "[REDACTED]" {
		t.Fatalf("api_key not redacted: %v", m["api_key"])
	}
	if m["model"] != "gpt-4o" {
		t.Fatalf("model altered: %v", m["model"])
	}
	if m["n"].(float64) != 3 {
		t.Fatalf("n altered: %v", m["n"])
	}
}

// TestRedactSSELineJSONFields covers the code-review finding that
// JSON-key-based redaction (api_key/token/secret/etc.) silently never fired
// on stream-captured SSE lines, because every such line carries a literal
// "data: " prefix that broke json.Decode outright, falling back to the
// regex-only pass (which doesn't know about arbitrary JSON key names).
func TestRedactSSELineJSONFields(t *testing.T) {
	in := "data: {\"id\":\"1\",\"choices\":[],\"api_key\":\"abcdefghijklmnopqrstuvwxyz-not-sk-format\"}\n"
	out := string(RedactBytes([]byte(in), ""))
	if !bytes.HasPrefix([]byte(out), []byte("data: ")) {
		t.Fatalf("SSE framing prefix lost: %q", out)
	}
	if !bytes.HasSuffix([]byte(out), []byte("\n")) {
		t.Fatalf("trailing newline lost: %q", out)
	}
	if bytes.Contains([]byte(out), []byte("abcdefghijklmnopqrstuvwxyz-not-sk-format")) {
		t.Fatalf("api_key value leaked through SSE framing: %q", out)
	}
	if !bytes.Contains([]byte(out), []byte(`"api_key":"[REDACTED]"`)) {
		t.Fatalf("api_key field not redacted: %q", out)
	}
	var payload map[string]any
	trimmed := bytes.TrimPrefix([]byte(out), []byte("data: "))
	if err := json.Unmarshal(bytes.TrimSpace(trimmed), &payload); err != nil {
		t.Fatalf("redacted SSE payload is not valid JSON: %v (out=%q)", err, out)
	}
}

// TestRedactSSEDoneLinePassesThrough ensures the SSE-prefix-stripping logic
// added for TestRedactSSELineJSONFields doesn't corrupt the literal
// "data: [DONE]" terminator line (which has no JSON payload to decode).
func TestRedactSSEDoneLinePassesThrough(t *testing.T) {
	in := "data: [DONE]\n"
	out := string(RedactBytes([]byte(in), ""))
	if out != in {
		t.Fatalf("expected [DONE] line untouched, got %q", out)
	}
}

// TestRedactDoesNotTruncateOrdinaryContentMentioningAuthorization covers the
// finding that the old greedy "authorization\s*:\s*[^\r\n]+" regex matched
// from ANY occurrence of "authorization:" through the rest of the (typically
// single-line, no embedded \r\n) JSON body, silently discarding real trailing
// user content instead of just the credential value.
func TestRedactDoesNotTruncateOrdinaryContentMentioningAuthorization(t *testing.T) {
	in := `{"model":"gpt-4o","messages":[{"role":"user","content":"How do I add an Authorization: Bearer token to my curl request?"}]}`
	out := RedactBytes([]byte(in), "")
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("ordinary content mentioning 'Authorization:' corrupted output into invalid JSON: %v (out=%s)", err, out)
	}
	if m["model"] != "gpt-4o" {
		t.Fatalf("model field lost/altered: %v (out=%s)", m["model"], out)
	}
}

// TestRedactBearerHandlesBase64Padding covers the finding that the Bearer
// token charset excluded '+', '/', '=' (standard base64 alphabet/padding),
// so a base64-encoded bearer token was only partially redacted.
func TestRedactBearerHandlesBase64Padding(t *testing.T) {
	in := `{"content":"curl -H 'Bearer abcDEF123+xyz/789==' https://api"}`
	out := RedactBytes([]byte(in), "")
	if bytes.Contains(out, []byte("xyz/789==")) {
		t.Fatalf("base64 tail of bearer token leaked: %s", out)
	}
	if !bytes.Contains(out, []byte("[REDACTED]")) {
		t.Fatalf("bearer token not redacted at all: %s", out)
	}
}

// TestRedactSKKeyWithDotSeparator covers the finding that reSKKey required an
// unbroken 20+ char run right after "sk-", so a single '.' anywhere in the
// first 20 characters (e.g. a dot-segmented key) defeated the match entirely.
func TestRedactSKKeyWithDotSeparator(t *testing.T) {
	in := `{"note":"key is sk-abc123.def456.ghijklmnopqrstuvwxyz0123456789"}`
	out := RedactBytes([]byte(in), "")
	if bytes.Contains(out, []byte("ghijklmnopqrstuvwxyz0123456789")) {
		t.Fatalf("dot-segmented sk- key leaked entirely unredacted: %s", out)
	}
	if !bytes.Contains(out, []byte("[REDACTED]")) {
		t.Fatalf("dot-segmented sk- key not redacted at all: %s", out)
	}
}

// TestRedactSKKeyShortWordNotFalsePositive guards the 20-char floor kept in
// reSKKey: an ordinary short word starting with "sk-" (not a real credential)
// must not be redacted.
func TestRedactSKKeyShortWordNotFalsePositive(t *testing.T) {
	in := `{"note":"sk-ip is a real word, not a secret"}`
	out := RedactBytes([]byte(in), "")
	if bytes.Contains(out, []byte("[REDACTED]")) {
		t.Fatalf("short non-credential sk- word was redacted as a false positive: %s", out)
	}
}

// TestRedactSKKeyCaseSensitivityUnchangedByRegexMerge guards a specific
// regression risk from merging reSKKey/reBearer/reAuthzHeader into one
// alternation (reCredential): the sk- branch must stay case-SENSITIVE (only
// lowercase "sk-", never "SK-"/"Sk-") exactly as the original standalone
// reSKKey was — while bearer/authorization stay case-INSENSITIVE. A naive
// merge using one top-level (?i) flag would have silently widened the sk-
// branch too.
func TestRedactSKKeyCaseSensitivityUnchangedByRegexMerge(t *testing.T) {
	in := `{"note":"SK-AAAAAAAAAAAAAAAAAAAAAAAA is not a real sk- credential shape"}`
	out := RedactBytes([]byte(in), "")
	if bytes.Contains(out, []byte("[REDACTED]")) {
		t.Fatalf("uppercase SK- was redacted, but reSKKey was always case-sensitive lowercase-only: %s", out)
	}
}

// TestRedactJSONFieldsDeeplyNestedStillRedacted guards the containsAnyJSONKey
// pre-check (code-review efficiency finding): it must never cause a false
// negative — a credential key name nested several levels deep, or mixed
// case, still has to trigger the full decode/walk/marshal path.
func TestRedactJSONFieldsDeeplyNestedStillRedacted(t *testing.T) {
	in := `{"a":{"b":[{"c":{"API_Key":"sk-nested-deep-AAAAAAAAAAAAAAAAAAAA"}}]}}`
	out := RedactBytes([]byte(in), "")
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output not valid JSON: %v (out=%s)", err, out)
	}
	if !bytes.Contains(out, []byte(`"[REDACTED]"`)) {
		t.Fatalf("deeply nested credential key was not redacted: %s", out)
	}
	if bytes.Contains(out, []byte("sk-nested-deep")) {
		t.Fatalf("deeply nested credential value leaked: %s", out)
	}
}

// TestRedactJSONFieldsNoKeyNameSkipsCleanly ensures the pre-check's fast
// path (no full decode) still returns valid, unmodified content for a body
// with no credential-shaped key name at all — the common case for an
// ordinary chat delta chunk.
func TestRedactJSONFieldsNoKeyNameSkipsCleanly(t *testing.T) {
	in := `{"id":"chatcmpl-1","choices":[{"delta":{"content":"hello"}}]}`
	out := RedactBytes([]byte(in), "")
	if string(out) != in {
		t.Fatalf("body with no credential key name was altered: got %s, want %s", out, in)
	}
}
