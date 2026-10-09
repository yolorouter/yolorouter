package audit

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// redactionPlaceholder is what the pipeline substitutes for every value it
// considers secret. A short constant rather than anything derived from the
// value: the placeholder must itself be unmistakably inert (it gets stored,
// exported to CSV, and rendered in the console) and must never encode a
// hint of what it replaced.
const redactionPlaceholder = "***"

// sensitiveHeadSegments names the secret "head noun" of a field: when the
// LAST underscore/hyphen segment of a field name is one of these, the field
// holds a credential and its value is replaced wholesale. Matching the head
// (rather than any segment) is what keeps compound identifier fields
// readable: "token_endpoint"/"authorization_endpoint" end in "endpoint"
// (public IdP URLs), "key_prefix"/"key_suffix" end in prefix/suffix (the
// deliberately non-sensitive display halves of caller keys), while
// "api_key", "refresh_token", "client_secret", and "password" all end in
// the noun itself.
var sensitiveHeadSegments = map[string]bool{
	"key": true, "keys": true,
	"token": true, "tokens": true,
	"secret": true, "secrets": true,
	"password": true, "passwords": true,
	"passwd": true, "pwd": true,
	"credential": true, "credentials": true,
	"authorization": true,
	"bearer":        true,
	"cookie":        true, "cookies": true,
}

// sensitiveAnySegment is the stronger tier: a field carrying one of these
// segments ANYWHERE in its name is sensitive regardless of its head noun —
// "password_hash" or "secret_fingerprint" must never survive just because
// they end in "hash"/"fingerprint". Only nouns that are unambiguous on
// their own sit here; "key" and "token" stay head-only, or fields like
// "token_endpoint" (whose VALUE is a public URL) would be redacted too.
var sensitiveAnySegment = map[string]bool{
	"password": true, "passwords": true,
	"passwd": true, "pwd": true,
	"secret": true, "secrets": true,
	"credential": true, "credentials": true,
}

// sensitiveExactNames catches fields whose name hides the secret from both
// segment rules. A proxy URL is the classic case: authenticated proxies
// embed "user:pass@" inside an otherwise ordinary URL, so the whole value
// is treated as a credential — the diff shows that the URL changed, never
// what it changed to.
var sensitiveExactNames = map[string]bool{
	"proxy_url": true,
}

// secretValuePrefixes are credential shapes recognizable on sight,
// independent of any field name — the second lock of the double insurance.
// A value that looks like a vendor key is redacted even when it lands in a
// field nobody flagged (a note, a description, a nested config blob).
var secretValuePrefixes = []string{
	"sk-",         // OpenAI-style API keys (sk-, sk-or-, sk-ant-, sk-proj-, …)
	"ghp_",        // GitHub personal access token
	"gho_",        // GitHub OAuth access token
	"ghu_",        // GitHub user-to-server token
	"ghs_",        // GitHub server-to-server token
	"ghr_",        // GitHub refresh token
	"github_pat_", // GitHub fine-grained token
	"xoxb-",       // Slack bot token
	"xoxa-",       // Slack app token
	"xoxp-",       // Slack legacy token
	"AKIA",        // AWS access key id
	"AIza",        // Google API key
	"$2a$",        // bcrypt password hash
	"$2b$",        // bcrypt password hash (variant)
	"$2x$",        // bcrypt password hash (variant)
	"$2y$",        // bcrypt password hash (variant)
	"$argon2",     // argon2 password hash
}

// fieldNameSensitive reports whether a detail field's name marks its value
// as a credential. Evaluated case-insensitively against the exact name, the
// head segment, then every segment. The intended bias is over-redaction: a
// false positive costs one diff field, a false negative costs a secret.
func fieldNameSensitive(name string) bool {
	lower := strings.ToLower(name)
	if sensitiveExactNames[lower] {
		return true
	}
	segments := strings.FieldsFunc(lower, func(r rune) bool { return r == '_' || r == '-' })
	if len(segments) == 0 {
		return false
	}
	head := segments[len(segments)-1]
	if sensitiveHeadSegments[head] {
		return true
	}
	for _, seg := range segments {
		if sensitiveAnySegment[seg] {
			return true
		}
	}
	return false
}

// valueLooksSecret reports whether a bare string value carries a
// recognizable credential shape, whatever field it sits in.
func valueLooksSecret(s string) bool {
	for _, prefix := range secretValuePrefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	// "Bearer <token>" credentials, case-insensitive per RFC 7235 grammar.
	if len(s) >= 7 && strings.EqualFold(s[:7], "bearer ") {
		return true
	}
	// JWT compact serialization: base64url segments whose header ("eyJ…"
	// is the base64 of '{"'…') makes the shape unambiguous with two dots.
	if strings.HasPrefix(s, "eyJ") && strings.Count(s, ".") >= 2 {
		return true
	}
	// URLs with an embedded password ("scheme://user:pass@host") —
	// authenticated proxy addresses being the detail-relevant case.
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			return true
		}
	}
	return false
}

// redactValue walks one decoded JSON value, returning its redacted form.
// Maps redact by field name first (whole subtree replaced — keeping an
// {"old":…,"new":…} wrapper around a redacted secret would only invite
// leaks through future shape changes) and recurse otherwise; strings are
// sniffed; every other scalar passes untouched. The walk is total: an
// unknown shape is passed through as-is, and json.Unmarshal only ever
// produces the shapes handled here.
func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if fieldNameSensitive(k) {
				out[k] = redactionPlaceholder
				continue
			}
			out[k] = redactValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = redactValue(val)
		}
		return out
	case string:
		if valueLooksSecret(t) {
			return redactionPlaceholder
		}
		return t
	default:
		return v
	}
}

// RedactJSON runs an already-serialized JSON detail document through the
// redaction pipeline and returns the redacted, canonicalized document.
// This is the choke-point form: the capture middleware re-runs every
// parked detail through it at request exit, so a caller that forgets the
// contract cannot leak through the seat. The operation is idempotent —
// the placeholder trips neither rule — so already-redacted documents come
// back byte-identical.
func RedactJSON(doc string) (string, error) {
	var parsed any
	if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
		return "", fmt.Errorf("audit redaction: detail is not valid JSON: %w", err)
	}
	out, err := json.Marshal(redactValue(parsed))
	if err != nil {
		return "", fmt.Errorf("audit redaction: re-marshal redacted detail: %w", err)
	}
	return string(out), nil
}

// RedactDetail serializes v and returns its redacted JSON form — the
// explicit-event layer's one-call path for building a detail document from
// a Go value (field maps, diff shapes, identifiers). Values here are the
// caller's raw business values; the pipeline is what makes them safe to
// store, so callers never hand-redact.
func RedactDetail(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("audit redaction: marshal detail: %w", err)
	}
	return RedactJSON(string(raw))
}
