package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
)

// redactedHeaderValue is the sentinel that replaces a masked header value. It
// is a cross-layer wire-format contract (the request-log detail frontend
// renders it, tests assert on it), so it lives in one named place rather than
// being re-spelled per call site. v0.1 does NOT redact request/response body
// CONTENT at all (only header key masking is retained), so this is the sole
// producer of the marker on the Go side.
const redactedHeaderValue = "[REDACTED]"

// sensitiveHeaderSubstrings are credential-suggesting fragments; ANY header
// whose (lowercased) name contains one is masked by default. This fail-closed
// substring match is the whole mechanism: providers use
// many spellings — "X-Api-Key", "Api-Key" (Azure), "X-Goog-Api-Key",
// "Anthropic-Api-Key", "X-Auth-Token", "X-Amz-Security-Token", "X-Secret",
// etc. An exact allowlist silently persisted every unrecognized one in
// plaintext and exposed it in the admin request-log detail. Matching by
// credential-word closes that whole class instead of chasing individual
// names — and the obvious exact names ("authorization"/"cookie" cover
// Authorization/Proxy-Authorization/Cookie/Set-Cookie) are already substrings
// here, so no separate exact denylist is needed. The one deliberate exception
// is the exact-name toolSessionHeaderAllowlist below, which un-masks three
// non-credential session headers; everything else containing a fragment
// stays masked.
var sensitiveHeaderSubstrings = []string{
	"authorization",
	"api-key",
	"apikey",
	"secret",
	"token",
	"password",
	"passwd",
	"credential",
	"private-key",
	"session",
	"cookie",
}

// isSensitiveHeader reports whether a header's value must be masked before
// persistence: a case-insensitive credential-word substring check so
// unknown/misspelled auth headers are masked by default (fail closed) rather
// than leaked (fail open). Matching on the lowercased name directly makes it
// spelling- and canonicalization-agnostic.
func isSensitiveHeader(name string) bool {
	lower := strings.ToLower(name)
	for _, frag := range sensitiveHeaderSubstrings {
		if strings.Contains(lower, frag) {
			return true
		}
	}
	return false
}

// toolSessionHeaderAllowlist is the exact-name carve-out from the substring
// masking above: coding-agent tools stamp their own session identity into
// these headers, and that value is a task identifier (which conversation this
// request belongs to), not a credential — no gateway accepts it as proof of
// anything. Masking it destroyed the one field that can group a tool's
// requests, so these three names are kept verbatim while every OTHER name
// containing a credential word (Cookie, Authorization, X-Session-Token, ...)
// stays masked. The match is exact on the lowercased name: a name that merely
// contains "session" is NOT allowed, only these spellings are. Adding a name
// here is a security decision — an allowed header's value is persisted and
// shown in the admin request-log detail — and must not be done casually.
var toolSessionHeaderAllowlist = map[string]struct{}{
	"x-claude-code-session-id": {},
	"x-opencode-session":       {},
	"x-session-id":             {},
}

// isToolSessionHeader reports whether name is one of the allowlisted
// tool-session headers, compared case-insensitively on the exact name.
func isToolSessionHeader(name string) bool {
	_, ok := toolSessionHeaderAllowlist[strings.ToLower(name)]
	return ok
}

// SanitizeHeaders returns the caller's request headers as a JSON object with
// every credential-bearing header value replaced by "[REDACTED]" and all
// other headers kept verbatim. A header is treated as
// credential-bearing if its name contains a credential word
// (isSensitiveHeader) — redact-by-default so an unrecognized auth header name
// can't leak a live provider key into the admin log — unless it is one of the
// exact allowlisted tool-session headers (isToolSessionHeader), whose values
// are task identifiers the agent-attribution columns read back out of this
// snapshot. This is header-NAME-based masking only; v0.1 deliberately does
// not scrub body CONTENT for credential-shaped substrings (that layer was
// removed). Returns nil for a nil header set.
func SanitizeHeaders(h http.Header) []byte {
	if h == nil {
		return nil
	}
	clean := make(http.Header, len(h))
	for k, v := range h {
		if isSensitiveHeader(k) && !isToolSessionHeader(k) {
			clean[k] = []string{redactedHeaderValue}
		} else {
			clean[k] = v
		}
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return nil
	}
	return b
}
