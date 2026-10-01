// Session-id extraction for the historical backfill's second pass: pure
// functions that promote one stored capture — the raw request body plus the
// masked request-header snapshot request_log_bodies keeps beside every row —
// to the calling tool's own session id, "" when the capture carries no
// session these rules can recover. The live write path reads the session
// from dedicated headers while the request is in flight; rows written
// before that pipeline existed kept their bodies verbatim, and this file
// is how those rows get their session back without touching the gateway.
//
// The rules deliberately mirror the recognition design of
// gateway.AgentFromHeaderSnapshot: pure functions over persisted bytes (a
// stored row's columns and the extractors over those columns can never
// disagree), case-insensitive snapshot keys, a repeated header counted as
// absent, and a result of "" meaning "no session" so the engine can leave
// the column SQL NULL. The codex field-merge shape follows the Langfuse
// ai-gateway agent-context parser (telemetry/context.rs): the turn-metadata
// blob's thread_id is the session, the session_id key never is, and the
// flat client_metadata projections only fill fields the blobs left absent.
package agentbackfill

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxSessionIDChars bounds an accepted session id: identifiers are short
// (UUID-shaped in every observed tool), so anything longer is not an id but
// a parsing accident, and the column has no business storing it. Same idea
// as the gateway recognizer's bounded inputs, applied to the output.
const maxSessionIDChars = 128

// redactedSessionSentinel is the sanitizer's masked-value marker. It can
// never be a session id, and a historical capture is fully capable of
// carrying it (the pre-allowlist snapshots masked the session headers), so
// the cleaner rejects the literal before any caller can persist it.
const redactedSessionSentinel = "[REDACTED]"

// codexTurnMetadataKey names the Codex CLI's turn snapshot: a JSON blob the
// tool sends canonically as client_metadata["x-codex-turn-metadata"] in the
// request body, and — as the compatibility projection older captures rely
// on — as the same-named request header. The header spelling in a stored
// snapshot is whatever the capture kept ("X-Codex-Turn-Metadata" observed);
// lookups are case-insensitive like every other snapshot reader.
const codexTurnMetadataKey = "x-codex-turn-metadata"

// codexFlatFields maps the flat client_metadata keys the Codex CLI projects
// from the same turn snapshot onto the merged field names the extractor
// works with. Flat keys are a compatibility projection, so they only ever
// FILL a field neither blob provided — never override the blob — and only
// session-relevant string values are kept.
var codexFlatFields = []struct{ source, field string }{
	{"session_id", "session_id"},
	{"thread_id", "thread_id"},
	{"turn_id", "turn_id"},
	{"root_turn_id", "root_turn_id"},
	{"parent_turn_id", "parent_turn_id"},
	{"x-codex-installation-id", "installation_id"},
	{"x-codex-window-id", "window_id"},
	{"x-codex-parent-thread-id", "parent_thread_id"},
}

// ExtractAgentSession is the dispatch the backfill engine drives: it picks
// the extraction rule for one already-attributed row's tool and returns
// that rule's session id, "" when the tool has no rule or the rule finds
// nothing. Keeping the dispatch here (rather than in the engine) means the
// engine's injected function stays a dumb seam and every parsing decision
// lives beside its test.
func ExtractAgentSession(agentClient string, body, headerSnapshot []byte) string {
	switch agentClient {
	case "claude-code":
		return ExtractClaudeCodeSession(body)
	case "codex":
		return ExtractCodexSession(body, headerSnapshot)
	}
	return ""
}

// ExtractClaudeCodeSession reads the Claude Code CLI's session out of one
// stored request body. The tool stamps every request with a metadata
// object whose user_id value is itself a JSON string —
// {"device_id":…,"account_uuid":…,"session_id":"<uuid>"} in every row
// observed so far — and the session is that inner object's session_id.
// A body that is not JSON, lacks the metadata object, carries a user_id
// that is not a JSON-encoded object, or whose inner object lacks a usable
// session_id yields "": the row keeps its NULL rather than a guess.
func ExtractClaudeCodeSession(body []byte) string {
	var top struct {
		Metadata *struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		return ""
	}
	if top.Metadata == nil {
		return ""
	}
	var inner struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(top.Metadata.UserID), &inner); err != nil {
		return ""
	}
	return cleanSessionID(inner.SessionID)
}

// ExtractCodexSession reads the Codex CLI's session out of one stored
// capture, from two projections of the same turn snapshot:
//
//  1. the X-Codex-Turn-Metadata request header in the masked snapshot —
//     in the captures this pass exists for, the header blob is the one
//     projection that survived whole (the body's client_metadata object
//     holds only the installation id there);
//  2. the body's top-level client_metadata["x-codex-turn-metadata"] — the
//     canonical position in newer clients, kept as the second source.
//
// Both blobs' fields merge (header first, body filling what the header
// blob left absent), then the flat client_metadata keys fill whatever is
// still missing. The session is ALWAYS the merged thread_id — never the
// session_id key, which is only ever a projection field — so a snapshot
// whose two ids disagree still attributes one deterministic value. No
// thread_id in any source means no session: "".
func ExtractCodexSession(body, headerSnapshot []byte) string {
	fields := parseTurnMetadataBlob(codexTurnMetadataHeader(headerSnapshot))
	if fields == nil {
		fields = map[string]string{}
	}
	clientMetadata := codexClientMetadata(body)
	if blob, ok := clientMetadata[codexTurnMetadataKey]; ok {
		for field, value := range parseTurnMetadataBlob(blob) {
			if _, dup := fields[field]; !dup {
				fields[field] = value
			}
		}
	}
	for _, mapping := range codexFlatFields {
		if value, ok := clientMetadata[mapping.source]; ok {
			if _, dup := fields[mapping.field]; !dup {
				fields[mapping.field] = value
			}
		}
	}
	return cleanSessionID(fields["thread_id"])
}

// cleanSessionID is the output hygiene every rule's candidate passes
// through: trimmed, non-empty, at most maxSessionIDChars characters, no
// control characters, and never the sanitizer's redaction sentinel.
// Anything else is rejected as "" — the column's contract is that a stored
// value was always a real caller-supplied id, and the backfill does not
// get to relax it.
func cleanSessionID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == redactedSessionSentinel {
		return ""
	}
	if utf8.RuneCountInString(value) > maxSessionIDChars {
		return ""
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return ""
	}
	return value
}

// codexTurnMetadataHeader pulls the turn-metadata blob out of one masked
// header snapshot. The snapshot is the JSON object SanitizeHeaders stores
// (each key mapping to its list of values); keys arrive in whatever case
// the capture kept, so the lookup collapses them once. A header that is
// absent, repeated, or blank is no blob at all.
func codexTurnMetadataHeader(snapshot []byte) string {
	if len(snapshot) == 0 {
		return ""
	}
	var headers map[string][]string
	if err := json.Unmarshal(snapshot, &headers); err != nil {
		return ""
	}
	lower := make(map[string][]string, len(headers))
	for k, v := range headers {
		lower[strings.ToLower(k)] = v
	}
	values, ok := lower[codexTurnMetadataKey]
	if !ok || len(values) != 1 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

// codexClientMetadata returns the body's top-level client_metadata object
// reduced to its string entries, nil when the body is not JSON, has no
// such object, or the object is not the Codex CLI's (it must carry at
// least one x-codex-* key — a client_metadata of any other shape is some
// other caller's field and is never mined for a codex session).
func codexClientMetadata(body []byte) map[string]string {
	if len(body) == 0 {
		return nil
	}
	var top struct {
		ClientMetadata map[string]json.RawMessage `json:"client_metadata"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		return nil
	}
	codex := false
	for key := range top.ClientMetadata {
		if strings.HasPrefix(key, "x-codex-") {
			codex = true
			break
		}
	}
	if !codex {
		return nil
	}
	fields := make(map[string]string, len(top.ClientMetadata))
	for key, raw := range top.ClientMetadata {
		var value string
		if err := json.Unmarshal(raw, &value); err == nil {
			fields[key] = value
		}
	}
	return fields
}

// parseTurnMetadataBlob decodes one turn-metadata blob into its string
// fields. A blob that is empty, not JSON, or not a JSON object yields nil
// (a projection that cannot be read is a projection that is not there);
// non-string entries inside the object are skipped, exactly like the
// reference parser's string-only field reads.
func parseTurnMetadataBlob(blob string) map[string]string {
	if blob == "" {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(blob), &raw); err != nil {
		return nil
	}
	fields := make(map[string]string, len(raw))
	for key, value := range raw {
		var s string
		if err := json.Unmarshal(value, &s); err == nil {
			fields[key] = s
		}
	}
	return fields
}
