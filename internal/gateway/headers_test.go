package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

func TestSanitizeHeadersMasksSensitiveKeepsRest(t *testing.T) {
	h := http.Header{
		"Authorization": []string{"Bearer sk-secret-value"},
		"Cookie":        []string{"session=abc"},
		"X-Api-Key":     []string{"key-123"},
		"User-Agent":    []string{"curl/8.0"},
		"Content-Type":  []string{"application/json"},
		"X-Custom":      []string{"keepme"},
	}
	out := SanitizeHeaders(h)
	if out == nil {
		t.Fatal("expected non-nil JSON for a non-empty header set")
	}

	var got map[string][]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}

	// Sensitive headers → [REDACTED], value never leaks.
	for _, k := range []string{"Authorization", "Cookie", "X-Api-Key"} {
		if len(got[k]) != 1 || got[k][0] != "[REDACTED]" {
			t.Errorf("header %q = %v, want [REDACTED]", k, got[k])
		}
	}
	if bytes.Contains(out, []byte("sk-secret-value")) || bytes.Contains(out, []byte("session=abc")) || bytes.Contains(out, []byte("key-123")) {
		t.Errorf("a sensitive header value leaked into output: %s", out)
	}

	// Non-sensitive headers → kept verbatim.
	if len(got["User-Agent"]) != 1 || got["User-Agent"][0] != "curl/8.0" {
		t.Errorf("User-Agent = %v, want kept verbatim", got["User-Agent"])
	}
	if len(got["X-Custom"]) != 1 || got["X-Custom"][0] != "keepme" {
		t.Errorf("X-Custom = %v, want kept verbatim", got["X-Custom"])
	}
}

func TestSanitizeHeadersMatchesCaseInsensitively(t *testing.T) {
	// A non-canonical (lowercase) key spelling must still be recognized as
	// sensitive — isSensitiveHeader matches the lowercased name, so casing
	// and header canonicalization don't matter.
	h := http.Header{"authorization": []string{"Bearer leak"}}
	out := SanitizeHeaders(h)
	if bytes.Contains(out, []byte("leak")) {
		t.Errorf("lowercase 'authorization' was not masked: %s", out)
	}
}

// TestSanitizeHeadersPreservesAllowlistedToolSessionHeaders is the tool-session
// allowlist matrix: the three exact names (in any casing) are kept verbatim,
// while every OTHER name containing a credential word — including near-miss
// spellings of the allowed three and the other session-chain headers — stays
// masked. The allowlist is exact on the name on purpose: "x-session-id" is a
// task identifier, "x-session-id-token" is not, and only the exact spellings
// were decided to be non-credentials.
func TestSanitizeHeadersPreservesAllowlistedToolSessionHeaders(t *testing.T) {
	cases := []struct {
		name   string
		header string // the key exactly as the caller sent it
		value  string
		want   string // the value the snapshot must carry
	}{
		// The three allowed names, in canonical and non-canonical casings:
		// the allowlist matches the lowercased exact name.
		{name: "claude code session id, canonical", header: "X-Claude-Code-Session-Id", value: "sess-claude-9f1", want: "sess-claude-9f1"},
		{name: "claude code session id, lowercase", header: "x-claude-code-session-id", value: "sess-claude-a2", want: "sess-claude-a2"},
		{name: "opencode session, canonical", header: "X-Opencode-Session", value: "sess-open-3c", want: "sess-open-3c"},
		{name: "opencode session, mixed case", header: "X-OPENCODE-Session", value: "sess-open-4d", want: "sess-open-4d"},
		{name: "session id, canonical", header: "X-Session-Id", value: "sess-plain-5e", want: "sess-plain-5e"},
		{name: "session id, lowercase", header: "x-session-id", value: "sess-plain-6f", want: "sess-plain-6f"},

		// Everything else containing a credential word stays masked. The
		// first three are the classic auth headers; the rest pin that the
		// carve-out is exact-name only: added/changed characters, a
		// different session header, and an underscore spelling all miss.
		{name: "authorization stays masked", header: "Authorization", value: "Bearer sk-live-9", want: "[REDACTED]"},
		{name: "cookie stays masked", header: "Cookie", value: "session=abc", want: "[REDACTED]"},
		{name: "x-session-token stays masked", header: "X-Session-Token", value: "tok-77", want: "[REDACTED]"},
		{name: "x-session-id with suffix is not the exact name", header: "X-Session-Id-History", value: "hist-88", want: "[REDACTED]"},
		{name: "pluralized claude session id is not the exact name", header: "X-Claude-Code-Session-Ids", value: "vals-99", want: "[REDACTED]"},
		{name: "session affinity is session-chain but not allowlisted", header: "X-Session-Affinity", value: "aff-10", want: "[REDACTED]"},
		{name: "underscore spelling is not the dash spelling", header: "Session_Id", value: "under-11", want: "[REDACTED]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := SanitizeHeaders(http.Header{tc.header: []string{tc.value}})
			var got map[string][]string
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("output is not valid JSON: %v (%s)", err, out)
			}
			if len(got[tc.header]) != 1 || got[tc.header][0] != tc.want {
				t.Errorf("header %q = %v, want single value %q", tc.header, got[tc.header], tc.want)
			}
		})
	}
}

// TestSanitizeHeadersAllowlistKeepsTheValueReadableInTheSnapshot proves the
// point of the carve-out end to end: a tool-bearing request keeps its session
// id plaintext in the snapshot while the credentials around it are masked, so
// the recognizer (and the admin detail view) can read the id back.
func TestSanitizeHeadersAllowlistKeepsTheValueReadableInTheSnapshot(t *testing.T) {
	h := http.Header{
		"Authorization":            []string{"Bearer sk-mask-me"},
		"User-Agent":               []string{"claude-cli/2.0.0 (external)"},
		"X-Claude-Code-Session-Id": []string{"6a2f1e4d-dead-beef-9c8b-7d6f5e4d3c2b"},
	}
	out := SanitizeHeaders(h)
	if !bytes.Contains(out, []byte("6a2f1e4d-dead-beef-9c8b-7d6f5e4d3c2b")) {
		t.Errorf("the allowlisted session id must survive masking verbatim: %s", out)
	}
	if !bytes.Contains(out, []byte("[REDACTED]")) {
		t.Errorf("the Authorization value must still be masked: %s", out)
	}
	if bytes.Contains(out, []byte("sk-mask-me")) {
		t.Errorf("the Authorization value leaked: %s", out)
	}
}

func TestSanitizeHeadersNilReturnsNil(t *testing.T) {
	if out := SanitizeHeaders(nil); out != nil {
		t.Errorf("SanitizeHeaders(nil) = %q, want nil", out)
	}
}
