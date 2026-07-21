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

func TestSanitizeHeadersNilReturnsNil(t *testing.T) {
	if out := SanitizeHeaders(nil); out != nil {
		t.Errorf("SanitizeHeaders(nil) = %q, want nil", out)
	}
}
