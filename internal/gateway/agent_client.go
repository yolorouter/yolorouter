package gateway

import (
	"encoding/json"
	"strings"
)

// Coding-agent tools (Claude Code, Codex CLI, OpenCode, ...) call this
// gateway with a signature no generic SDK produces: dedicated x- headers on
// every request and a User-Agent that names the tool. This file reads that
// signature out of the masked request-header snapshot and reduces it to two
// values — the normalized tool name and the tool's own session id — the same
// way traceparent.go promotes a trace-id: a pure function over the snapshot,
// never over the live request, so whatever is persisted beside the capture
// can always be re-derived from the capture. Recognition rules follow the
// Langfuse ai-gateway agent-context design (telemetry/context.rs): dedicated
// headers in a fixed priority order, User-Agent prefixes as the fallback, a
// bounded input budget. Both results are "" when nothing matched, which the
// audit row stores as SQL NULL — a non-NULL value always means the caller's
// own tool identified itself; nothing is ever guessed.

// maxAgentHeaderBytes caps the total bytes of recognition input (the values
// of every header this recognizer reads, user-agent included). A caller that
// stuffs its user-agent or tool headers past this bound is not a tool worth
// attributing: the whole recognition is abandoned — no client name, no
// session id — rather than parsed partially. Same contract as Langfuse's
// MAX_AGENT_HEADER_BYTES.
const maxAgentHeaderBytes = 8 * 1024

// userAgentKey is the snapshot key user-agent is looked up under. The lookup
// itself is case-insensitive — unlike the traceparent reader's exact key
// match, this parser also consumes historical snapshot rows, whose keys were
// stored exactly as captured, not necessarily canonical.
const userAgentKey = "user-agent"

// agentUserAgentPrefixes maps a lowercased User-Agent prefix to the tool name
// it identifies, used only when no dedicated header matched. Every prefix
// carries the version slash ("claude-cli/", not "claude-cli") so a name that
// merely STARTS with a tool's word cannot collide: "codexa/1.0" is not
// "codex/", and "MyCherryStudio/x" is not "cherrystudio/". Generic SDK and
// tool UAs (curl, urllib, OpenAI-JS) are deliberately absent — an unmapped
// caller is a caller this gateway cannot attribute.
var agentUserAgentPrefixes = []struct {
	prefix string
	client string
}{
	{"claude-cli/", "claude-code"},
	{"opencode/", "opencode"},
	{"geminicli/", "gemini-cli"},
	{"qwencode/", "qwen-code"},
	{"charm-crush/", "crush"},
	{"codewhale/", "codewhale"},
	{"cherrystudio/", "cherry-studio"},
	{"pi/", "pi"},
	{"codex/", "codex"},
}

// agentDetectionGroups fixes the dedicated-header detection priority: each
// group's owner name with the headers that identify it, checked in slice
// order — claude-code, then codex, then opencode (the order Langfuse parses
// them in; deliberately fixed so two tools' headers on one request resolve
// deterministically). Adding a tool means adding a row here and (if it has a
// recognizable UA) one in the prefix table above.
var agentDetectionGroups = []struct {
	client  string
	headers []string
}{
	{client: "claude-code", headers: []string{
		"x-claude-code-session-id",
		"x-claude-code-agent-id",
		"x-claude-code-parent-agent-id",
	}},
	{client: "codex", headers: []string{
		// The Codex CLI stamps its installation/window/thread/turn
		// identifiers on every request.
		"x-codex-installation-id",
		"x-codex-window-id",
		"x-codex-parent-thread-id",
		"x-codex-turn-metadata",
	}},
	{client: "opencode", headers: []string{
		"x-opencode-project",
		"x-opencode-session",
		"x-opencode-request",
	}},
}

// agentSessionChainHeaders is where a session id is taken from, in this fixed
// order: the tool-specific headers first, then the two generic session
// headers OpenCode-style tools send. The first header with a usable value
// wins; a chain entry whose value was masked (or is absent) is skipped, not
// preferred over a later real one.
var agentSessionChainHeaders = []string{
	"x-claude-code-session-id",
	"x-opencode-session",
	"x-session-id",
	"x-session-affinity",
}

// AgentFromHeaderSnapshot reads the caller's agent signature out of a masked
// request-header snapshot (the JSON SanitizeHeaders produces) and returns the
// normalized client name and the client's session id, either "" when absent.
//
// The input is the snapshot rather than the live header for the same reason
// as the trace-id reader: the audit row's columns and the stored header JSON
// then cannot disagree. Reading the MASKED snapshot also defines the
// sentinel's meaning here, and the two readers below split it deliberately:
//
//   - Detection treats a "[REDACTED]"-valued dedicated header as PRESENT. A
//     masked value still proves the caller sent the header, and snapshots
//     written before the toolSessionHeaderAllowlist existed mask exactly
//     these headers — historical rows must still attribute their tool from
//     that presence (the value is gone, the fact it was sent is not).
//   - Session extraction treats "[REDACTED]" as NO value. A masked value is
//     not a session id; persisting the literal would poison the column, so
//     the chain skips it (and pre-whitelist history can never get one back).
//
// Everything about the parse is case-insensitive (header names and User-Agent
// both), a repeated header counts as absent, and an input whose recognition
// headers total more than maxAgentHeaderBytes abandons recognition outright.
func AgentFromHeaderSnapshot(snapshot []byte) (client string, sessionID string) {
	if len(snapshot) == 0 {
		return "", ""
	}
	var headers map[string][]string
	if err := json.Unmarshal(snapshot, &headers); err != nil {
		return "", ""
	}
	// Collapse keys to their lowercased form once: snapshots arrive with
	// canonical Go keys ("X-Claude-Code-Session-Id"), but historical rows
	// and hand-written tests may spell them any way.
	lower := make(map[string][]string, len(headers))
	for k, v := range headers {
		lower[strings.ToLower(k)] = v
	}
	if agentRecognitionBytes(lower) > maxAgentHeaderBytes {
		return "", ""
	}
	client = detectAgentClient(lower)
	if client == "" {
		// No tool identified itself: no session either, even if a generic
		// x-session-id happens to be present. A value in these columns
		// means the TOOL was recognized, never just any header.
		return "", ""
	}
	return client, agentSessionValue(lower)
}

// detectAgentClient resolves the tool name: dedicated headers first, in the
// fixed agentDetectionGroups order, then the User-Agent prefix table as the
// fallback.
func detectAgentClient(lower map[string][]string) string {
	for _, group := range agentDetectionGroups {
		for _, name := range group.headers {
			if headerPresent(lower, name) {
				return group.client
			}
		}
	}
	return clientFromUserAgent(lower)
}

// clientFromUserAgent maps the (lowercased) User-Agent through the prefix
// table. No prefix matches — curl, urllib, an empty UA, an unmapped SDK —
// returns "".
func clientFromUserAgent(lower map[string][]string) string {
	ua := headerValue(lower, userAgentKey)
	if ua == "" {
		return ""
	}
	ua = strings.ToLower(ua)
	for _, m := range agentUserAgentPrefixes {
		if strings.HasPrefix(ua, m.prefix) {
			return m.client
		}
	}
	return ""
}

// agentSessionValue walks the session chain and returns the first usable
// value, "" when no link carries one. The masked sentinel is skipped: it is
// the sanitizer's marker, not the caller's id.
func agentSessionValue(lower map[string][]string) string {
	for _, name := range agentSessionChainHeaders {
		if v := headerValue(lower, name); v != "" {
			return v
		}
	}
	return ""
}

// headerPresent reports whether name carries a single non-blank value in the
// snapshot. A "[REDACTED]" value counts: the value was masked, not absent,
// and the header's presence is itself the tool signature (see the sentinel
// note on AgentFromHeaderSnapshot). A repeated or blank header does not
// count — neither can be attributed to the tool with any confidence.
func headerPresent(lower map[string][]string, name string) bool {
	values, ok := lower[name]
	if !ok || len(values) != 1 {
		return false
	}
	return strings.TrimSpace(values[0]) != ""
}

// headerValue returns name's single usable value, trimmed; "" when the header
// is absent, repeated, blank, or masked to the redaction sentinel.
func headerValue(lower map[string][]string, name string) string {
	values, ok := lower[name]
	if !ok || len(values) != 1 {
		return ""
	}
	v := strings.TrimSpace(values[0])
	if v == "" || v == redactedHeaderValue {
		return ""
	}
	return v
}

// agentRecognitionBytes sums the byte length of every value of every header
// the recognizer reads — the user-agent, all dedicated detection headers, and
// the session chain — over the lowercased snapshot. This is the budget the
// maxAgentHeaderBytes cap is measured against; headers outside the
// recognition set cost nothing because the recognizer never looks at them.
func agentRecognitionBytes(lower map[string][]string) int {
	total := 0
	seen := make(map[string]struct{}, 13)
	count := func(name string) {
		if _, dup := seen[name]; dup {
			return // x-claude-code-session-id is in two tables; count once
		}
		seen[name] = struct{}{}
		for _, v := range lower[name] {
			total += len(v)
		}
	}
	count(userAgentKey)
	for _, group := range agentDetectionGroups {
		for _, name := range group.headers {
			count(name)
		}
	}
	for _, name := range agentSessionChainHeaders {
		count(name)
	}
	return total
}
