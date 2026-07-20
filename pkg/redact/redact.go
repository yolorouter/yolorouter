// Package redact scrubs credentials from gateway request/response bodies
// before persistence (PRD §6.8.6: auth headers and internal credentials are
// out of scope for body logging; provider raw errors must hide possible
// credentials before display; full API keys/upstream keys must never be
// stored).
//
// Applied server-side to every persisted body (gateway §4.1) and the stream
// file (§4.3). The frontend never sees raw credentials — redaction is
// mandatory and never frontend-only.
package redact

import (
	"bytes"
	"encoding/json"
	"regexp"
)

const redacted = "[REDACTED]"

// reCredential matches all three regex-shaped credential patterns in one
// alternation, so RedactBytes makes a single pass over body instead of three
// (code-review finding: three separate ReplaceAll calls each re-scan the
// whole buffer — on the gateway's hot path, this runs on every persisted
// body AND every single SSE stream line). ReplaceAllFunc below distinguishes
// which branch matched to apply the right replacement.
//
//   - "sk-" + 20+ chars, ending on an alphanumeric. The 20-char floor guards
//     against false positives on ordinary words like "sk-ip"; '.'/'_'/'-'
//     separators are allowed WITHIN that run (e.g. a dot-segmented key) — an
//     unbroken run was too strict and let a single '.' anywhere in the first
//     20 characters defeat the match entirely (Codex review finding).
//   - "bearer <token>", token charset includes standard base64 alphabet
//     chars ('+', '/') and padding ('=') so a base64-encoded bearer token
//     isn't left partially redacted after the first special character
//     (Codex review finding).
//   - "authorization: <token>", value charset deliberately narrow
//     (token-shaped characters only, no whitespace) instead of "rest of the
//     line" — an ordinary JSON body has no embedded \r\n, so a greedy
//     "match to end of line" pattern used to consume everything after any
//     literal "Authorization:" substring appearing in ordinary user
//     content, corrupting the persisted JSON (Codex review finding).
//
// Case sensitivity is preserved exactly per-branch via Go regexp's scoped
// (?i:...) flag groups: the sk- branch stays case-SENSITIVE (as it always
// was — "sk-" is a literal prefix, never "SK-"), while bearer/authorization
// stay case-INSENSITIVE (as they always were). A single top-level (?i) would
// have silently widened the sk- branch to also match "SK-"/"Sk-", a
// behavior change from the original three-regex version.
var reCredential = regexp.MustCompile(`sk-[A-Za-z0-9_\-.]{19,}[A-Za-z0-9]|(?i:bearer\s+[A-Za-z0-9_\-.+/=]+)|(?i:authorization\s*:\s*[A-Za-z0-9_\-.+/=]+)`)

// jsonKeySet holds the credential-bearing JSON string keys (case-insensitive)
// whose values are replaced with "[REDACTED]". Consulted by redactJSONFields.
var jsonKeySet = map[string]bool{
	"api_key":       true,
	"apikey":        true,
	"key":           true,
	"authorization": true,
	"secret":        true,
	"token":         true,
}

// RedactBytes returns a copy of body with credentials replaced by "[REDACTED]".
// callerKey is the full caller API key for THIS request (from the auth
// context); it is redacted everywhere in case the caller pasted their own key
// into messages/tools. An empty callerKey skips the exact-string pass. Never
// panics: invalid JSON falls back to regex-only redaction.
func RedactBytes(body []byte, callerKey string) []byte {
	if len(body) == 0 {
		return body
	}
	out := body
	if callerKey != "" {
		out = bytes.ReplaceAll(out, []byte(callerKey), []byte(redacted))
	}
	// Single pass over out via ReplaceAllFunc (vs three separate ReplaceAll
	// calls, each its own full scan) — the match's own bytes tell us which
	// alternation branch fired, so we can still reconstruct the
	// branch-specific replacement ("Bearer [REDACTED]" / "Authorization:
	// [REDACTED]" / bare "[REDACTED]" for sk-).
	out = reCredential.ReplaceAllFunc(out, func(m []byte) []byte {
		lower := bytes.ToLower(m)
		switch {
		case bytes.HasPrefix(lower, []byte("bearer")):
			return []byte("Bearer " + redacted)
		case bytes.HasPrefix(lower, []byte("authorization")):
			return []byte("Authorization: " + redacted)
		default:
			return []byte(redacted)
		}
	})
	out = redactJSONFields(out)
	return out
}

// stripSSEDataPrefix splits off the leading "data:" (with an optional
// following space) SSE framing that every persisted stream-capture line
// carries (internal/gateway/stream.go's appendStreamBodyLine feeds
// redactJSONFields raw "data: {...}\n" lines). Without stripping it,
// json.Decode fails on every single stream line and JSON-key credential
// redaction silently never fires on stream captures (Codex review finding)
// — only the regex passes above would ever catch a credential in a streamed
// chunk. Two fixed byte.HasPrefix checks (simplification finding) do the
// same job as a regex engine invocation for a 5-6 byte literal prefix.
func stripSSEDataPrefix(b []byte) (prefix, rest []byte) {
	if bytes.HasPrefix(b, []byte("data: ")) {
		return b[:6], b[6:]
	}
	if bytes.HasPrefix(b, []byte("data:")) {
		return b[:5], b[5:]
	}
	return nil, b
}

// redactJSONFields walks a JSON value and replaces string values of
// credential-bearing keys. Returns the input unchanged (regex-only path) if
// the body — or, for an SSE line, the payload after stripping the leading
// "data:" framing — isn't valid JSON.
func redactJSONFields(body []byte) []byte {
	prefix, payload := stripSSEDataPrefix(body)
	trimmed := bytes.TrimRight(payload, "\r\n")
	if len(trimmed) == 0 || string(bytes.TrimSpace(trimmed)) == "[DONE]" {
		return body // no JSON payload to redact (blank line / [DONE] terminator)
	}
	// Cheap pre-check (code-review finding): walkRedact only ever touches
	// values of the fixed jsonKeySet key names, and a JSON key must appear as
	// that literal substring in the source bytes for any real-world producer
	// (json.Marshal never emits unicode-escaped ASCII key names). On the
	// gateway's hot path — every persisted body AND every SSE line — most
	// delta/content chunks carry none of these key names at all, so skip the
	// full decode/walk/marshal round-trip entirely when none can match.
	if !containsAnyJSONKey(trimmed) {
		return body
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return body // not JSON — the regex pass above is the safety net
	}
	walkRedact(&v)
	out, err := json.Marshal(v)
	if err != nil {
		return body
	}
	if prefix == nil {
		return out
	}
	// Re-attach the SSE framing plus whatever trailing bytes (newline) the
	// original line carried after the JSON payload.
	trailer := payload[len(trimmed):]
	result := make([]byte, 0, len(prefix)+len(out)+len(trailer))
	result = append(result, prefix...)
	result = append(result, out...)
	result = append(result, trailer...)
	return result
}

// walkRedact mutates credential string values in place. Only string values of
// known keys are replaced; structure and other types are untouched.
func walkRedact(v *any) {
	switch t := (*v).(type) {
	case map[string]any:
		for k, val := range t {
			if jsonKeySet[lowerASCII(k)] {
				if _, ok := val.(string); ok {
					t[k] = redacted
				}
			} else {
				vv := val
				walkRedact(&vv)
				t[k] = vv
			}
		}
	case []any:
		for i := range t {
			walkRedact(&t[i])
		}
	}
}

// containsAnyJSONKey reports whether body contains, as a raw case-insensitive
// substring, any of jsonKeySet's key names. This is a necessary (not
// sufficient) condition for walkRedact ever redacting anything — real JSON
// producers always emit key names as literal ASCII text, never
// unicode-escaped, so a substring miss here guarantees walkRedact would find
// nothing to redact and the full decode/marshal round-trip can be skipped.
func containsAnyJSONKey(body []byte) bool {
	lower := bytes.ToLower(body)
	for k := range jsonKeySet {
		if bytes.Contains(lower, []byte(k)) {
			return true
		}
	}
	return false
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}
