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
// here, so no separate exact denylist is needed.
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

// SanitizeHeaders returns the caller's request headers as a JSON object with
// every credential-bearing header value replaced by "[REDACTED]" and all
// other headers kept verbatim. A header is treated as
// credential-bearing if its name contains a credential word
// (isSensitiveHeader) — redact-by-default so an unrecognized auth header name
// can't leak a live provider key into the admin log. This is
// header-NAME-based masking only; v0.1 deliberately does not scrub body
// CONTENT for credential-shaped substrings (that layer was removed). Returns
// nil for a nil header set.
func SanitizeHeaders(h http.Header) []byte {
	if h == nil {
		return nil
	}
	clean := make(http.Header, len(h))
	for k, v := range h {
		if isSensitiveHeader(k) {
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
