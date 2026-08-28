package protocols_test

import (
	"github.com/yolorouter/yolorouter/internal/protocols"
	"strings"
	"testing"
)

func TestRedactURLStripsUserInfoCredentials(t *testing.T) {
	got := protocols.RedactURL("https://user:secret@example.com/v1/chat/completions?token=abc")
	if strings.Contains(got, "secret") || strings.Contains(got, "token") {
		t.Fatalf("RedactURL leaked credential/query: %q", got)
	}
	if strings.Contains(got, "user@") || strings.Contains(got, "user:") {
		t.Fatalf("RedactURL kept userinfo: %q", got)
	}
	if !strings.HasPrefix(got, "https://example.com/") {
		t.Fatalf("RedactURL lost scheme/host: %q", got)
	}
}

func TestRedactURLPreservesNonSecretQuery(t *testing.T) {
	// Gemini streaming dispatches with alt=sse (a protocol selector, not a
	// credential) alongside a credential-bearing query param. The logged URL
	// must keep alt=sse so it matches what was actually dispatched, while
	// dropping the credential and userinfo.
	got := protocols.RedactURL("https://user:secret@host.example/v1beta/models/m:streamGenerateContent?alt=sse&key=topsecret")
	if !strings.Contains(got, "alt=sse") {
		t.Fatalf("RedactURL dropped non-secret alt=sse: %q", got)
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "topsecret") || strings.Contains(got, "user@") {
		t.Fatalf("RedactURL leaked credentials: %q", got)
	}
	if !strings.HasPrefix(got, "https://host.example/") {
		t.Fatalf("RedactURL lost scheme/host: %q", got)
	}
}

func TestJoinUpstreamURL(t *testing.T) {
	cases := []struct {
		name       string
		base       string
		egressPath string
		proto      protocols.ProtocolID
		want       string
	}{
		// --- OpenAI Chat egress ---
		{
			"openai chat: base /v1 → strip /v1 from path",
			"https://api.openai.com/v1", "/v1/chat/completions", protocols.ProtocolOpenAI,
			"https://api.openai.com/v1/chat/completions",
		},
		{
			"openai chat: bare host → append /v1",
			"https://api.openai.com", "/v1/chat/completions", protocols.ProtocolOpenAI,
			"https://api.openai.com/v1/chat/completions",
		},
		{
			"openai chat: path-prefixed proxy (no version) → still append /v1",
			"https://gateway.example/openai", "/v1/chat/completions", protocols.ProtocolOpenAI,
			"https://gateway.example/openai/v1/chat/completions",
		},
		{
			"openai chat: mandao with /v1",
			"https://ai-api.mandao.com/v1", "/v1/chat/completions", protocols.ProtocolOpenAI,
			"https://ai-api.mandao.com/v1/chat/completions",
		},
		// Regression: a bare base URL with no /v1 must still get /v1
		// auto-appended, otherwise the join produces a path missing the
		// version segment and the upstream returns a 404.
		{
			"openai chat: ctyun bare host → auto-append /v1 (real incident regression)",
			"https://wishub-x6.ctyun.cn", "/v1/chat/completions", protocols.ProtocolOpenAI,
			"https://wishub-x6.ctyun.cn/v1/chat/completions",
		},
		// Non-v1 version token (v4, e.g. Zhipu).
		{
			"openai chat: zhipu /v4 path",
			"https://open.bigmodel.cn/api/paas/v4", "/v1/chat/completions", protocols.ProtocolOpenAI,
			"https://open.bigmodel.cn/api/paas/v4/chat/completions",
		},

		// --- Responses egress ---
		{
			"responses: base /v1 + bare /responses path",
			"https://api.openai.com/v1", "/responses", protocols.ProtocolResponses,
			"https://api.openai.com/v1/responses",
		},
		{
			"responses: bare host → append /v1",
			"https://api.openai.com", "/responses", protocols.ProtocolResponses,
			"https://api.openai.com/v1/responses",
		},
		{
			"responses: path-prefixed proxy → append /v1",
			"https://gateway.example/openai", "/responses", protocols.ProtocolResponses,
			"https://gateway.example/openai/v1/responses",
		},

		// --- Anthropic egress ---
		{
			"claude: base /v1 → /messages",
			"https://api.anthropic.com/v1", "/v1/messages", protocols.ProtocolClaude,
			"https://api.anthropic.com/v1/messages",
		},
		{
			"claude: bare host → /v1/messages",
			"https://api.anthropic.com", "/v1/messages", protocols.ProtocolClaude,
			"https://api.anthropic.com/v1/messages",
		},
		{
			"claude: zhipu anthropic compat (no version) → /v1/messages",
			"https://open.bigmodel.cn/api/anthropic", "/v1/messages", protocols.ProtocolClaude,
			"https://open.bigmodel.cn/api/anthropic/v1/messages",
		},

		// --- Gemini egress ---
		{
			"gemini: base /v1beta + bare model path",
			"https://generativelanguage.googleapis.com/v1beta", "/models/gemini-2.0:generateContent", protocols.ProtocolGemini,
			"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0:generateContent",
		},
		{
			"gemini: bare host → /v1beta",
			"https://generativelanguage.googleapis.com", "/models/gemini-2.0:generateContent", protocols.ProtocolGemini,
			"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0:generateContent",
		},
		{
			"gemini: path-prefixed proxy → still append /v1beta",
			"https://gateway.example/gemini", "/models/gemini-2.0:generateContent", protocols.ProtocolGemini,
			"https://gateway.example/gemini/v1beta/models/gemini-2.0:generateContent",
		},
		// Model path with a query string (streamGenerateContent?alt=sse).
		{
			"gemini: bare host + stream path with query",
			"https://generativelanguage.googleapis.com", "/models/gemini-2.0:streamGenerateContent?alt=sse", protocols.ProtocolGemini,
			"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0:streamGenerateContent?alt=sse",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := protocols.JoinUpstreamURL(tc.base, tc.egressPath, tc.proto)
			if got != tc.want {
				t.Errorf("\nbase=%q\negressPath=%q\nproto=%s\ngot:  %q\nwant: %q",
					tc.base, tc.egressPath, tc.proto, got, tc.want)
			}
		})
	}
}

// TestJoinUpstreamURLTrailingSlash is a regression test: a provider
// base_url ending in "/" is an allowed config shape (the key-test path at
// provider_client.go:153 trims it the same way), but before the fix
// JoinUpstreamURL only trimmed the trailing slash on the Gemini branch —
// every other protocol produced a doubled "//" when base ended in "/". This
// asserts all four protocol branches produce the identical URL whether or
// not base carries a trailing slash.
func TestJoinUpstreamURLTrailingSlash(t *testing.T) {
	cases := []struct {
		name       string
		base       string
		egressPath string
		proto      protocols.ProtocolID
	}{
		{"openai chat: base /v1", "https://api.openai.com/v1", "/v1/chat/completions", protocols.ProtocolOpenAI},
		{"openai chat: bare host", "https://api.openai.com", "/v1/chat/completions", protocols.ProtocolOpenAI},
		{"responses: base /v1", "https://api.openai.com/v1", "/responses", protocols.ProtocolResponses},
		{"responses: bare host", "https://api.openai.com", "/responses", protocols.ProtocolResponses},
		{"claude: base /v1", "https://api.anthropic.com/v1", "/v1/messages", protocols.ProtocolClaude},
		{"claude: bare host", "https://api.anthropic.com", "/v1/messages", protocols.ProtocolClaude},
		{"gemini: base /v1beta", "https://generativelanguage.googleapis.com/v1beta", "/models/gemini-2.0:generateContent", protocols.ProtocolGemini},
		{"gemini: bare host", "https://generativelanguage.googleapis.com", "/models/gemini-2.0:generateContent", protocols.ProtocolGemini},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withoutSlash := protocols.JoinUpstreamURL(tc.base, tc.egressPath, tc.proto)
			withSlash := protocols.JoinUpstreamURL(tc.base+"/", tc.egressPath, tc.proto)
			if withSlash != withoutSlash {
				t.Errorf("trailing-slash base produced a different URL:\nbase=%q  -> %q\nbase=%q -> %q",
					tc.base, withoutSlash, tc.base+"/", withSlash)
			}
			if strings.Contains(strings.TrimPrefix(withSlash, "https://"), "//") {
				t.Errorf("trailing-slash base produced a doubled slash: %q", withSlash)
			}
		})
	}
}

// TestEndsWithVersionSegment covers the root cause of a prior bug: a weak
// "path length > 1 implies versioned" check would misclassify a path prefix
// like https://gateway.example/openai (not a version) as versioned, which
// then caused the join step to skip appending /v1. This test locks the
// stricter rule that a segment only counts as a version token when it is
// strictly "v" followed by a digit.
func TestEndsWithVersionSegment(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"https://api.openai.com/v1", true},
		{"https://api.openai.com/v1beta", true},
		{"https://api.openai.com/v2", true},
		{"https://api.anthropic.com/v1/", true}, // trailing slash
		{"https://example.com/v10", true},       // two-digit version
		{"https://example.com/api/v1", true},
		{"https://example.com/api/paas/v4", true},
		{"https://open.bigmodel.cn/api/paas/v4", true},
		{"https://open.bigmodel.cn/api/coding/paas/v4", true},
		// Regression for the same root cause: the old weak check ("path
		// length > 1 implies versioned") would misclassify these as true,
		// causing the join step to skip appending /v1. These must return
		// false:
		{"https://gateway.example.com/openai", false},     // gateway prefix, not a version
		{"https://gateway.example.com/anthropic", false},  // protocol name, not a version
		{"https://open.bigmodel.cn/api/anthropic", false}, // last segment is not a version
		{"https://api.deepseek.com/anthropic", false},     // last segment is not a version
		{"https://api.openai.com", false},                 // bare host
		{"https://api.openai.com/", false},                // bare host with trailing slash
		{"https://example.com/v", false},                  // last segment is just "v"
		{"https://example.com/version", false},            // starts with "v" but not a version token
		{"", false},                                       // empty string
	}
	for _, tc := range cases {
		got := protocols.EndsWithVersionSegment(tc.raw)
		if got != tc.want {
			t.Errorf("EndsWithVersionSegment(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// A URL that will not parse must be replaced wholesale, never handed back:
// the unparsed string is exactly what this function exists to sanitize — a
// configured base URL with a stray control character fails to parse, fails to
// build a request, and its error text carrying the raw URL (credentials and
// all) is what would get persisted. Weaken the branch back to returning the
// input and this reads the secret straight through.
func TestRedactURLReplacesAnUnparseableURLWholesale(t *testing.T) {
	raw := "https://user:secret@example.com/v1?\x7f=x&token=abc"
	got := protocols.RedactURL(raw)
	if strings.Contains(got, "secret") || strings.Contains(got, "token=abc") {
		t.Fatalf("RedactURL(%q) = %q: the unparseable input leaked through", raw, got)
	}
	if got == raw {
		t.Fatalf("RedactURL returned its input unchanged; an unparseable URL must be replaced wholesale")
	}
}
