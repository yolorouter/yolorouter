package gateway

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Every recognition case drives the exact chain production runs — http.Header
// into SanitizeHeaders into AgentFromHeaderSnapshot — rather than calling the
// internals directly, so the matrices cannot drift from what the masked
// snapshot actually preserves. Cases whose shape the masking step can no
// longer produce (pre-allowlist history, plaintext session-affinity) carry
// the snapshot verbatim in raw instead.

// agentRecognitionCase is one recognition verdict: which client, which
// session id, from which snapshot.
type agentRecognitionCase struct {
	name string
	// headers, when non-nil, is masked into a snapshot the way Handle entry
	// does it.
	headers http.Header
	// raw, when non-nil, is the snapshot verbatim, for shapes the masking
	// step itself can never produce.
	raw []byte
	// wantClient/wantSession are the expected returns; "" is SQL NULL at the
	// persistence layer.
	wantClient  string
	wantSession string
}

func (tc agentRecognitionCase) snapshot() []byte {
	if tc.raw != nil {
		return tc.raw
	}
	return SanitizeHeaders(tc.headers) // nil headers -> nil snapshot
}

// TestAgentRecognitionIdentifiesTheTool is the positive matrix: every
// dedicated detection header, then every User-Agent prefix, each on its own
// identifies its tool with no other signature present.
func TestAgentRecognitionIdentifiesTheTool(t *testing.T) {
	cases := []agentRecognitionCase{
		// The dedicated-header matrix. Each case carries an UNMAPPED
		// user-agent, proving the header alone — not the UA — did the
		// identifying. Session ids only come from the session chain, so the
		// non-session detection headers yield "".
		{name: "claude code session id", headers: http.Header{"User-Agent": {"curl/8.0"}, "X-Claude-Code-Session-Id": {"sess-cc-1"}}, wantClient: "claude-code", wantSession: "sess-cc-1"},
		{name: "claude code agent id", headers: http.Header{"User-Agent": {"curl/8.0"}, "X-Claude-Code-Agent-Id": {"agent-cc-2"}}, wantClient: "claude-code"},
		{name: "claude code parent agent id", headers: http.Header{"User-Agent": {"curl/8.0"}, "X-Claude-Code-Parent-Agent-Id": {"parent-cc-3"}}, wantClient: "claude-code"},
		{name: "codex installation id", headers: http.Header{"User-Agent": {"curl/8.0"}, "X-Codex-Installation-Id": {"inst-cx-1"}}, wantClient: "codex"},
		{name: "codex window id", headers: http.Header{"User-Agent": {"curl/8.0"}, "X-Codex-Window-Id": {"win-cx-2"}}, wantClient: "codex"},
		{name: "codex parent thread id", headers: http.Header{"User-Agent": {"curl/8.0"}, "X-Codex-Parent-Thread-Id": {"thr-cx-3"}}, wantClient: "codex"},
		{name: "codex turn metadata", headers: http.Header{"User-Agent": {"curl/8.0"}, "X-Codex-Turn-Metadata": {"{\"turn_id\":\"t-4\"}"}}, wantClient: "codex"},
		{name: "opencode project", headers: http.Header{"User-Agent": {"curl/8.0"}, "X-Opencode-Project": {"proj-oc-1"}}, wantClient: "opencode"},
		{name: "opencode session", headers: http.Header{"User-Agent": {"curl/8.0"}, "X-Opencode-Session": {"sess-oc-2"}}, wantClient: "opencode", wantSession: "sess-oc-2"},
		{name: "opencode request", headers: http.Header{"User-Agent": {"curl/8.0"}, "X-Opencode-Request": {"req-oc-3"}}, wantClient: "opencode"},

		// The User-Agent prefix matrix, each prefix with no dedicated
		// header anywhere. Version text after the slash is the caller's
		// business; the prefix is the whole match.
		{name: "ua claude-cli", headers: http.Header{"User-Agent": {"claude-cli/2.0.0 (external, cli)"}}, wantClient: "claude-code"},
		{name: "ua opencode", headers: http.Header{"User-Agent": {"opencode/1.2.3"}}, wantClient: "opencode"},
		{name: "ua geminicli", headers: http.Header{"User-Agent": {"geminicli/0.8.0 (linux; x64)"}}, wantClient: "gemini-cli"},
		{name: "ua qwencode", headers: http.Header{"User-Agent": {"qwencode/1.0.0"}}, wantClient: "qwen-code"},
		{name: "ua charm-crush", headers: http.Header{"User-Agent": {"charm-crush/0.4.2"}}, wantClient: "crush"},
		{name: "ua codewhale", headers: http.Header{"User-Agent": {"codewhale/0.1.7"}}, wantClient: "codewhale"},
		{name: "ua cherrystudio", headers: http.Header{"User-Agent": {"CherryStudio/1.4.0"}}, wantClient: "cherry-studio"},
		{name: "ua pi", headers: http.Header{"User-Agent": {"pi/0.1.0"}}, wantClient: "pi"},
		{name: "ua codex", headers: http.Header{"User-Agent": {"codex/0.44.0 (Mac OS 15.0; arm64)"}}, wantClient: "codex"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, session := AgentFromHeaderSnapshot(tc.snapshot())
			if client != tc.wantClient || session != tc.wantSession {
				t.Errorf("AgentFromHeaderSnapshot() = (%q, %q), want (%q, %q)", client, session, tc.wantClient, tc.wantSession)
			}
		})
	}
}

// TestAgentRecognitionDedicatedHeadersBeatTheUserAgent pins the priority: a
// dedicated header identifies the tool even when the User-Agent names a
// different one, and among dedicated headers the group order
// claude-code → codex → opencode is fixed.
func TestAgentRecognitionDedicatedHeadersBeatTheUserAgent(t *testing.T) {
	cases := []agentRecognitionCase{
		{
			name:       "opencode header over gemini-cli ua",
			headers:    http.Header{"User-Agent": {"geminicli/0.8.0"}, "X-Opencode-Project": {"proj-1"}},
			wantClient: "opencode",
		},
		{
			name:       "claude-code header over codex ua",
			headers:    http.Header{"User-Agent": {"codex/1.0.0"}, "X-Claude-Code-Agent-Id": {"a-1"}},
			wantClient: "claude-code",
		},
		{
			name:       "claude-code group wins over codex group",
			headers:    http.Header{"X-Claude-Code-Agent-Id": {"a-2"}, "X-Codex-Turn-Metadata": {"{}"}},
			wantClient: "claude-code",
		},
		{
			name:       "codex group wins over opencode group",
			headers:    http.Header{"X-Codex-Window-Id": {"w-1"}, "X-Opencode-Request": {"r-1"}},
			wantClient: "codex",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, session := AgentFromHeaderSnapshot(tc.snapshot())
			if client != tc.wantClient || session != tc.wantSession {
				t.Errorf("AgentFromHeaderSnapshot() = (%q, %q), want (%q, %q)", client, session, tc.wantClient, tc.wantSession)
			}
		})
	}
}

// TestAgentRecognitionSessionChainFallsThroughLevelByLevel walks the session
// chain: the first usable value wins, in the fixed order
// x-claude-code-session-id → x-opencode-session → x-session-id →
// x-session-affinity. The tool itself is identified by UA in these cases, so
// only the chain decides the session column.
func TestAgentRecognitionSessionChainFallsThroughLevelByLevel(t *testing.T) {
	cases := []agentRecognitionCase{
		{
			name:        "tool-specific session header",
			headers:     http.Header{"User-Agent": {"claude-cli/2.0"}, "X-Claude-Code-Session-Id": {"cc-1"}},
			wantClient:  "claude-code",
			wantSession: "cc-1",
		},
		{
			// The chain's second link is also opencode's dedicated detection
			// header, so it identifies the tool before any UA is consulted
			// (qwencode names a different tool and still loses), and the
			// session is then read from that same header.
			name:        "opencode session header identifies opencode over a qwen-code ua",
			headers:     http.Header{"User-Agent": {"qwencode/1.0"}, "X-Opencode-Session": {"oc-1"}},
			wantClient:  "opencode",
			wantSession: "oc-1",
		},
		{
			name:        "generic x-session-id",
			headers:     http.Header{"User-Agent": {"claude-cli/2.0"}, "X-Session-Id": {"plain-1"}},
			wantClient:  "claude-code",
			wantSession: "plain-1",
		},
		{
			// x-session-affinity is NOT on the sanitizer allowlist, so a
			// snapshot the masking step produced carries the sentinel, not
			// the value — the chain skips it and the column stays NULL.
			name:        "affinity is masked in live snapshots",
			headers:     http.Header{"User-Agent": {"claude-cli/2.0"}, "X-Session-Affinity": {"aff-1"}},
			wantClient:  "claude-code",
			wantSession: "",
		},
		{
			// A pre-allowlist snapshot (or one hand-fed to the recognizer)
			// can carry a plaintext affinity; the chain's last link reads it.
			name:        "plaintext affinity is the last resort",
			raw:         []byte(`{"User-Agent":["claude-cli/2.0"],"X-Session-Affinity":["aff-2"]}`),
			wantClient:  "claude-code",
			wantSession: "aff-2",
		},
		{
			// Earlier link wins when two chain headers are present.
			name:        "session id preferred over affinity",
			raw:         []byte(`{"User-Agent":["claude-cli/2.0"],"X-Session-Id":["plain-2"],"X-Session-Affinity":["aff-3"]}`),
			wantClient:  "claude-code",
			wantSession: "plain-2",
		},
		{
			// A masked link is skipped, not stored: the sentinel must never
			// reach the column, and a later real value still can.
			name:        "masked link skipped for a later real value",
			raw:         []byte(`{"User-Agent":["claude-cli/2.0"],"X-Claude-Code-Session-Id":["[REDACTED]"],"X-Session-Id":["plain-3"]}`),
			wantClient:  "claude-code",
			wantSession: "plain-3",
		},
		{
			// A repeated session header cannot be reconciled into one
			// conversation; neither value is preferred.
			name:        "repeated session header refused",
			raw:         []byte(`{"User-Agent":["opencode/1.2"],"X-Opencode-Session":["s-a","s-b"]}`),
			wantClient:  "opencode",
			wantSession: "",
		},
		{
			name:        "no session header at all",
			headers:     http.Header{"User-Agent": {"claude-cli/2.0"}},
			wantClient:  "claude-code",
			wantSession: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, session := AgentFromHeaderSnapshot(tc.snapshot())
			if client != tc.wantClient || session != tc.wantSession {
				t.Errorf("AgentFromHeaderSnapshot() = (%q, %q), want (%q, %q)", client, session, tc.wantClient, tc.wantSession)
			}
		})
	}
}

// TestAgentRecognitionRefusesLookalikes is the collision matrix: a User-Agent
// that merely starts with a tool's word — or wraps it in another product's
// name — matches nothing. Every prefix in the table ends in a slash for
// exactly this reason.
func TestAgentRecognitionRefusesLookalikes(t *testing.T) {
	cases := []agentRecognitionCase{
		{name: "codexa is not codex", headers: http.Header{"User-Agent": {"codexa/1.0"}}, wantClient: "", wantSession: ""},
		{name: "MyCherryStudio is not cherrystudio", headers: http.Header{"User-Agent": {"MyCherryStudio/x"}}, wantClient: "", wantSession: ""},
		{name: "claude-cli without the version slash", headers: http.Header{"User-Agent": {"claude-cli 2.0"}}, wantClient: "", wantSession: ""},
		{name: "opencode inside a longer product name", headers: http.Header{"User-Agent": {"myopencode-tool/1.0"}}, wantClient: "", wantSession: ""},
		{name: "pi without the slash is not the pi tool", headers: http.Header{"User-Agent": {"pi (codeassist)"}}, wantClient: "", wantSession: ""},
		{name: "pip is not pi", headers: http.Header{"User-Agent": {"pip/25.0"}}, wantClient: "", wantSession: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, session := AgentFromHeaderSnapshot(tc.snapshot())
			if client != tc.wantClient || session != tc.wantSession {
				t.Errorf("AgentFromHeaderSnapshot() = (%q, %q), want (%q, %q)", client, session, tc.wantClient, tc.wantSession)
			}
		})
	}
}

// TestAgentRecognitionWithoutASignatureYieldsNothing is the all-miss matrix:
// generic callers (curl, urllib, an official SDK, an empty or absent UA) get
// both columns empty — a value there always means the tool itself was
// identified. A lone session header changes nothing on its own.
func TestAgentRecognitionWithoutASignatureYieldsNothing(t *testing.T) {
	cases := []agentRecognitionCase{
		{name: "curl", headers: http.Header{"User-Agent": {"curl/7.81.0"}}, wantClient: "", wantSession: ""},
		{name: "python urllib", headers: http.Header{"User-Agent": {"Python-urllib/3.10"}}, wantClient: "", wantSession: ""},
		{name: "openai sdk is deliberately unmapped", headers: http.Header{"User-Agent": {"OpenAI-JS/4.0.0"}}, wantClient: "", wantSession: ""},
		{name: "empty user-agent value", headers: http.Header{"User-Agent": {""}}, wantClient: "", wantSession: ""},
		{name: "no user-agent at all", headers: http.Header{"Content-Type": {"application/json"}}, wantClient: "", wantSession: ""},
		{
			// A session header without a tool signature is not attribution.
			name:        "session header alone identifies nothing",
			headers:     http.Header{"User-Agent": {"curl/7.81.0"}, "X-Session-Id": {"orphan-1"}},
			wantClient:  "",
			wantSession: "",
		},
		{name: "no snapshot at all", headers: nil, raw: nil, wantClient: "", wantSession: ""},
		{name: "snapshot is not json", raw: []byte(`{"User-Agent": ["claude-cli/`), wantClient: "", wantSession: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, session := AgentFromHeaderSnapshot(tc.snapshot())
			if client != tc.wantClient || session != tc.wantSession {
				t.Errorf("AgentFromHeaderSnapshot() = (%q, %q), want (%q, %q)", client, session, tc.wantClient, tc.wantSession)
			}
		})
	}
}

// TestAgentRecognitionAbandonsOnOversizedInput pins the byte budget: when the
// recognition headers (user-agent included) total more than 8KB, the whole
// recognition is abandoned — no client, no session — even though a header
// would otherwise match. Exactly 8KB is still recognized: the cap is a
// greater-than bound, matching the Langfuse semantics it follows.
func TestAgentRecognitionAbandonsOnOversizedInput(t *testing.T) {
	const kb = 1024
	cases := []agentRecognitionCase{
		{
			name: "9KB user-agent swamps the budget",
			headers: http.Header{
				"User-Agent": {strings.Repeat("u", 9*kb)},
				// A dedicated header that would match is still not enough:
				// the budget gates the whole recognition.
				"X-Opencode-Project": {"proj-big"},
			},
			wantClient:  "",
			wantSession: "",
		},
		{
			name: "9KB session header swamps the budget",
			headers: http.Header{
				"User-Agent":               {"claude-cli/2.0"},
				"X-Claude-Code-Session-Id": {strings.Repeat("s", 9*kb)},
			},
			wantClient:  "",
			wantSession: "",
		},
		{
			// Exactly 8KB of recognition input is still within the budget:
			// the UA's own bytes count, so the project header is sized to
			// make the sum land on the bound exactly.
			name: "exactly 8KB is within the budget",
			headers: http.Header{
				"User-Agent":         {"opencode/1.2"},
				"X-Opencode-Project": {strings.Repeat("p", 8*kb-len("opencode/1.2"))},
			},
			wantClient:  "opencode",
			wantSession: "",
		},
		{
			// Headers outside the recognition set are free: a huge cookie
			// (masked anyway) must not starve the recognition of its budget.
			name: "huge irrelevant header costs nothing",
			headers: http.Header{
				"User-Agent": {"claude-cli/2.0"},
				"Cookie":     {strings.Repeat("c", 32*kb)},
			},
			wantClient:  "claude-code",
			wantSession: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, session := AgentFromHeaderSnapshot(tc.snapshot())
			if client != tc.wantClient || session != tc.wantSession {
				t.Errorf("AgentFromHeaderSnapshot() = (%q, %q), want (%q, %q)", client, session, tc.wantClient, tc.wantSession)
			}
		})
	}
}

// TestAgentRecognitionIsCaseInsensitiveOnBothAxes: header names are matched
// case-insensitively (snapshots — historical rows included — do not
// necessarily carry canonical keys), and the User-Agent is lowercased before
// the prefix match.
func TestAgentRecognitionIsCaseInsensitiveOnBothAxes(t *testing.T) {
	cases := []agentRecognitionCase{
		{
			name:        "uppercase user-agent",
			headers:     http.Header{"User-Agent": {"CLAUDE-CLI/2.0.0"}},
			wantClient:  "claude-code",
			wantSession: "",
		},
		{
			name:        "mixed-case user-agent",
			headers:     http.Header{"User-Agent": {"OpenCode/1.2.3"}},
			wantClient:  "opencode",
			wantSession: "",
		},
		{
			name:        "lowercase dedicated header key",
			headers:     http.Header{"user-agent": {"curl/8.0"}, "x-codex-turn-metadata": {"{}"}},
			wantClient:  "codex",
			wantSession: "",
		},
		{
			name:        "mixed-case dedicated header key",
			headers:     http.Header{"User-Agent": {"curl/8.0"}, "X-OPENCODE-PROJECT": {"proj-1"}},
			wantClient:  "opencode",
			wantSession: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, session := AgentFromHeaderSnapshot(tc.snapshot())
			if client != tc.wantClient || session != tc.wantSession {
				t.Errorf("AgentFromHeaderSnapshot() = (%q, %q), want (%q, %q)", client, session, tc.wantClient, tc.wantSession)
			}
		})
	}
}

// TestAgentRecognitionAttributesPreAllowlistHistory is the shape the backfill
// consumes: snapshots written before the toolSessionHeaderAllowlist existed
// mask the session header to the sentinel. Presence still identifies the
// tool — the masked value proves the header was sent — but no session id can
// exist, and the literal sentinel must never reach the column.
func TestAgentRecognitionAttributesPreAllowlistHistory(t *testing.T) {
	cases := []agentRecognitionCase{
		{
			name:        "masked claude code session header still names the tool",
			raw:         []byte(`{"User-Agent":["curl/7.81.0"],"X-Claude-Code-Session-Id":["[REDACTED]"]}`),
			wantClient:  "claude-code",
			wantSession: "",
		},
		{
			name:        "masked opencode session header still names the tool",
			raw:         []byte(`{"User-Agent":["QwenCode/1.0.0"],"X-Opencode-Session":["[REDACTED]"]}`),
			wantClient:  "opencode",
			wantSession: "",
		},
		{
			// The sentinel is not stored even when every chain link is
			// masked: the column reads NULL, not "[REDACTED]".
			name:        "all session links masked",
			raw:         []byte(`{"User-Agent":["claude-cli/2.0"],"X-Claude-Code-Session-Id":["[REDACTED]"],"X-Session-Id":["[REDACTED]"],"X-Session-Affinity":["[REDACTED]"]}`),
			wantClient:  "claude-code",
			wantSession: "",
		},
		{
			// A blank detection header carries no signature: whitespace is
			// not a value, and curl does not become a tool.
			name:        "blank detection header is absent",
			raw:         []byte(fmt.Sprintf(`{"User-Agent":["curl/7.81.0"],"X-Codex-Turn-Metadata":["%s"]}`, "   ")),
			wantClient:  "",
			wantSession: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, session := AgentFromHeaderSnapshot(tc.snapshot())
			if client != tc.wantClient || session != tc.wantSession {
				t.Errorf("AgentFromHeaderSnapshot() = (%q, %q), want (%q, %q)", client, session, tc.wantClient, tc.wantSession)
			}
		})
	}
}
