package audit

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactDetailBlacklistSamples is the field-name half of the double
// insurance: a value sitting under a credential-named field is replaced
// wholesale, whatever the value looks like — including the {"old","new"}
// diff wrapper, which is dropped around the secret rather than trusted to
// carry it safely.
func TestRedactDetailBlacklistSamples(t *testing.T) {
	secrets := map[string]string{
		"api_key":          "not-a-known-prefix-but-secret-by-name",
		"token":            "opaque-random-opaque",
		"client_secret":    "oidc-client-secret-value",
		"password":         "plain-text-passw0rd",
		"access_token":     "opaque-access-token",
		"refresh_token":    "opaque-refresh-token",
		"password_hash":    "$2a$10$notarealhashjustbytes",
		"current_password": "old-pass-plain",
		"proxy_url":        "https://proxy.internal:3128",
		"authorization":    "Basic dXNlcjpwYXNz",
		"cookie":           "session-blob-here",
		"credentials":      "user:pass",
	}
	sample := map[string]any{}
	for field, secret := range secrets {
		sample[field] = secret
	}
	// The diff shape a settings-style change produces for a secret field.
	sample["upstream_key"] = map[string]any{"old": "sk-oldvalue000000", "new": "sk-newvalue000000"}

	out, err := RedactDetail(sample)
	if err != nil {
		t.Fatalf("RedactDetail: %v", err)
	}
	assertNoPlaintext(t, "blacklist sample", out, secrets)
	assertNoPlaintext(t, "blacklist sample diff", out, map[string]string{
		"diff old": "sk-oldvalue000000",
		"diff new": "sk-newvalue000000",
	})
	// Positive shape: every credential field survives as a name with the
	// inert placeholder — the audit page must still show WHICH field was
	// set, just never its value.
	for field := range secrets {
		if !strings.Contains(out, `"`+field+`":"`+redactionPlaceholder+`"`) {
			t.Errorf("field %q must keep its name with the placeholder value, got %s", field, out)
		}
	}
	if !strings.Contains(out, `"upstream_key":"`+redactionPlaceholder+`"`) {
		t.Errorf("diff-wrapped secret must be replaced whole (no {old,new} wrapper kept), got %s", out)
	}
}

// TestRedactDetailMixedCaseFieldNames pins the case-insensitivity the
// fieldNameSensitive contract promises ("evaluated case-insensitively
// against the exact name, the head segment, then every segment"): each
// layer must catch a credential name written in ANY letter case. Every
// planted value is deliberately sniffer-inert — a bare proxy address
// carries no user:pass, none matches a key prefix — so only the name
// rules can produce the redaction under test.
func TestRedactDetailMixedCaseFieldNames(t *testing.T) {
	sample := map[string]any{
		// Exact-name layer: the registered name is "proxy_url"; these must
		// never slip past just for their casing.
		"Proxy_Url": "https://proxy.internal:3128",
		"PROXY_URL": "https://proxy.internal:3128",
		// Head-segment layer: head noun key/token in mixed case.
		"API_KEY":        "not-a-known-prefix-but-secret-by-name",
		"Upstream_Token": "opaque-mixed-case-token",
		// Any-segment layer: "password" mid-name in mixed case under an
		// inert head noun ("backup").
		"My_PASSWORD_Backup": "mixed-case-password-value",
	}
	out, err := RedactDetail(sample)
	if err != nil {
		t.Fatalf("RedactDetail: %v", err)
	}
	for field := range sample {
		if !strings.Contains(out, `"`+field+`":"`+redactionPlaceholder+`"`) {
			t.Errorf("mixed-case credential field %q must be redacted by the name rules, got %s", field, out)
		}
	}
}

// TestRedactDetailSniffingSamples is the value-shape half: a recognizable
// credential is redacted even in a field nobody flagged — the case where a
// future caller parks business values under an innocent name.
func TestRedactDetailSniffingSamples(t *testing.T) {
	secrets := map[string]string{
		"openai style":  "sk-sniff1234567890abcdef",
		"openrouter":    "sk-or-v1-sniff000000000000000000000000",
		"anthropic":     "sk-ant-api03-sniff-0000000000000000",
		"github token":  "ghp_sniff16charsOK",
		"slack token":   "xoxb-sniff-slack-bot-token",
		"aws key id":    "AKIAIOSFODNN7EXAMPLE",
		"google key":    "AIzaSyA-sniffGoogleKey0000000",
		"bearer header": "Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig",
		"jwt bare":      "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.sig",
		"bcrypt hash":   "$2a$10$vpIoHknMZAeHODNlCkCaIO",
		"cred url":      "https://user:hunter2@proxy.example:3128",
	}
	sample := map[string]any{
		"note":        secrets["openai style"],
		"label":       secrets["openrouter"],
		"description": secrets["anthropic"],
		"remark":      secrets["github token"],
		"comment":     secrets["slack token"],
		"metadata":    secrets["aws key id"],
		"extra":       secrets["google key"],
		"header":      secrets["bearer header"],
		"assertion":   secrets["jwt bare"],
		"stored_hash": secrets["bcrypt hash"],
		"upstream":    secrets["cred url"],
	}
	out, err := RedactDetail(sample)
	if err != nil {
		t.Fatalf("RedactDetail: %v", err)
	}
	assertNoPlaintext(t, "sniffing sample", out, secrets)
	for _, field := range []string{"note", "label", "description", "remark", "comment",
		"metadata", "extra", "header", "assertion", "stored_hash", "upstream"} {
		if !strings.Contains(out, `"`+field+`":"`+redactionPlaceholder+`"`) {
			t.Errorf("field %q must carry the placeholder after sniffing, got %s", field, out)
		}
	}
}

// TestRedactDetailNestedDocuments walks the nesting contract: the pipeline
// recurses through arbitrarily deep maps and arrays, redacting by name and
// by shape at every level while leaving non-secret structure readable.
func TestRedactDetailNestedDocuments(t *testing.T) {
	nested := map[string]any{
		"provider": map[string]any{
			"name": "acme",
			"entries": []any{
				map[string]any{"api_key": "sk-nested-level-three", "weight": 10},
				map[string]any{"api_key": "sk-nested-level-three-b", "weight": 5},
			},
			"settings": map[string]any{
				"retry": map[string]any{"max": 3, "backoff": "exponential"},
			},
		},
		"count": 42,
		"ok":    true,
		"nil":   nil,
	}
	out, err := RedactDetail(nested)
	if err != nil {
		t.Fatalf("RedactDetail: %v", err)
	}
	assertNoPlaintext(t, "nested sample", out, map[string]string{
		"nested key a": "sk-nested-level-three",
		"nested key b": "sk-nested-level-three-b",
	})
	// Structure and non-secret leaves survive the walk.
	for _, keep := range []string{`"name":"acme"`, `"weight":10`, `"weight":5`, `"backoff":"exponential"`, `"max":3`, `"count":42`, `"ok":true`, `"nil":null`} {
		if !strings.Contains(out, keep) {
			t.Errorf("non-secret leaf %s must survive, got %s", keep, out)
		}
	}
	// A field literally named "keys"/"tokens" is a credential list and is
	// replaced wholesale — recursion there would let opaque token strings
	// through on shape alone. Detail authors list deletions under a neutral
	// name ("deleted") whose entries carry key_suffix identifiers instead.
	wholesale, err := RedactDetail(map[string]any{
		"keys":   []any{map[string]any{"key_suffix": "ab12"}},
		"tokens": []any{"opaque-token-string"},
	})
	if err != nil {
		t.Fatalf("RedactDetail: %v", err)
	}
	if !strings.Contains(wholesale, `"keys":"***"`) || !strings.Contains(wholesale, `"tokens":"***"`) {
		t.Errorf("bare keys/tokens fields must be replaced wholesale, got %s", wholesale)
	}
}

// TestRedactDetailDeletionIdentifiersSurvive pins the deletion-class shape:
// the identifying fragments an audit row NEEDS after a delete — a key's
// last four characters, an import candidate's upstream model name — are
// display identifiers by construction and must not be eaten by the same
// rules that eat the full key. The full key itself still never survives.
func TestRedactDetailDeletionIdentifiersSurvive(t *testing.T) {
	fullKey := "sk-fullkey00000000ab12"
	sample := map[string]any{
		"deleted": []any{
			map[string]any{
				"key_suffix":          "ab12",
				"provider_model_name": "deepseek-ai/DeepSeek-V4",
			},
			map[string]any{
				"key_suffix": "cd34",
			},
		},
	}
	out, err := RedactDetail(sample)
	if err != nil {
		t.Fatalf("RedactDetail: %v", err)
	}
	// Zero-plaintext for the FULL key the fragments came from.
	if strings.Contains(out, fullKey) {
		t.Fatalf("full key plaintext leaked into deletion detail: %s", out)
	}
	// The identifying fragments themselves survive.
	for _, keep := range []string{`"key_suffix":"ab12"`, `"key_suffix":"cd34"`, `"provider_model_name":"deepseek-ai/DeepSeek-V4"`} {
		if !strings.Contains(out, keep) {
			t.Errorf("deletion identifier %s must survive redaction, got %s", keep, out)
		}
	}
}

// TestRedactDetailSafeCompoundFieldNames pins the head-noun nuance against
// regression: fields that merely CONTAIN "token"/"key"/"authorization" in
// a compound name whose head noun is a public concept stay readable —
// redacting them would blank out the public halves of an OAuth config.
func TestRedactDetailSafeCompoundFieldNames(t *testing.T) {
	sample := map[string]any{
		"token_endpoint":         "https://idp.example/token",
		"authorization_endpoint": "https://idp.example/authorize",
		"key_prefix":             "pk-6Oxu",
		"key_suffix":             "mC4N",
		"client_id":              "public-client-id",
		"keyword":                "search-term",
	}
	out, err := RedactDetail(sample)
	if err != nil {
		t.Fatalf("RedactDetail: %v", err)
	}
	for field, value := range sample {
		want := `"` + field + `":"` + value.(string) + `"`
		if !strings.Contains(out, want) {
			t.Errorf("safe compound field redacted: expected %s in %s", want, out)
		}
	}
	// The name exemption is not a value exemption: a key_prefix whose
	// value itself looks like a live key head (an OpenAI key's first
	// characters are "sk-…") is caught by the sniffer — the display halves
	// are only safe while they do not look like the secret itself.
	sniffed, err := RedactDetail(map[string]any{"key_prefix": "sk-6Oxu"})
	if err != nil {
		t.Fatalf("RedactDetail: %v", err)
	}
	if !strings.Contains(sniffed, `"key_prefix":"***"`) {
		t.Errorf("secret-shaped key_prefix value must be sniffed, got %s", sniffed)
	}
	// The head nouns on their own remain sensitive — the nuance must not
	// decay into "never match key/token".
	strictOut, err := RedactDetail(map[string]any{
		"key": "opaque-1", "token": "opaque-2",
	})
	if err != nil {
		t.Fatalf("RedactDetail: %v", err)
	}
	if !strings.Contains(strictOut, `"key":"***"`) || !strings.Contains(strictOut, `"token":"***"`) {
		t.Errorf("bare key/token fields must stay redacted, got %s", strictOut)
	}
}

// TestRedactJSONInvalidDocument errors rather than passing unverifiable
// bytes through — the capture layer drops such documents (sanitizeDetail).
func TestRedactJSONInvalidDocument(t *testing.T) {
	if _, err := RedactJSON(`{"broken":`); err == nil {
		t.Fatal("RedactJSON must reject a malformed document")
	}
	if _, err := RedactJSON(`plain text, not json`); err == nil {
		t.Fatal("RedactJSON must reject non-JSON input")
	}
}

// TestRedactJSONIdempotent pins the property the capture layer's second
// pass depends on: redacting an already-redacted document changes nothing,
// so the backstop can run over every row without double effects.
func TestRedactJSONIdempotent(t *testing.T) {
	first, err := RedactDetail(map[string]any{
		"api_key": "sk-idempotence-check", "name": "keep-me", "nested": map[string]any{"token": "opaque"},
	})
	if err != nil {
		t.Fatalf("RedactDetail: %v", err)
	}
	second, err := RedactJSON(first)
	if err != nil {
		t.Fatalf("RedactJSON: %v", err)
	}
	if first != second {
		t.Fatalf("redaction must be idempotent:\nfirst:  %s\nsecond: %s", first, second)
	}
}

// TestRedactAllSampleGroupsZeroPlaintext is the mandatory zero-plaintext
// reverse assertion over the full four-group corpus in one serialized
// document: field-name hits, sniffing hits, nesting, and deletion
// identifiers together, the way a real rich detail document would mix
// them. Every planted secret must be absent from the serialized result;
// every safe identifier must remain.
func TestRedactAllSampleGroupsZeroPlaintext(t *testing.T) {
	planted := map[string]string{
		"g1 api_key":    "sk-corpus-one-111111111",
		"g1 password":   "corpus-password-plain",
		"g1 client":     "corpus-client-secret",
		"g2 sniff":      "ghp_corpus-sniff-000000",
		"g2 url cred":   "https://admin:corpus-pass@proxy.example",
		"g3 nested":     "sk-corpus-nested-2222222",
		"g4 full key":   "sk-corpus-fullkey-3333",
		"g4 full key b": "sk-corpus-fullkey-4444",
	}
	corpus := map[string]any{
		"changed": []any{"api_key", "password"},
		"diff": map[string]any{
			"client_secret": map[string]any{"old": planted["g1 client"], "new": "rotated-corpus-secret"},
			"mode":          map[string]any{"old": "off", "new": "custom"},
		},
		"note":  planted["g2 sniff"],
		"proxy": planted["g2 url cred"],
		"provider": map[string]any{
			"entries": []any{
				map[string]any{"api_key": planted["g3 nested"], "weight": 2},
			},
		},
		"deleted": []any{
			map[string]any{"key_suffix": "3333"},
			map[string]any{"key_suffix": "4444", "provider_model_name": "vendor/model-x"},
		},
	}
	out, err := RedactDetail(corpus)
	if err != nil {
		t.Fatalf("RedactDetail: %v", err)
	}
	assertNoPlaintext(t, "corpus", out, planted)
	assertNoPlaintext(t, "corpus rotated", out, map[string]string{"rotated": "rotated-corpus-secret"})
	for _, keep := range []string{`"key_suffix":"3333"`, `"key_suffix":"4444"`, `"vendor/model-x"`, `"old":"off"`, `"new":"custom"`, `"weight":2`} {
		if !strings.Contains(out, keep) {
			t.Errorf("safe content %s must survive the corpus pass, got %s", keep, out)
		}
	}
	// The document must still be valid JSON after redaction — a broken
	// re-serialization would silently strip detail at the capture layer.
	var reparsed any
	if err := json.Unmarshal([]byte(out), &reparsed); err != nil {
		t.Fatalf("redacted corpus is no longer valid JSON: %v\n%s", err, out)
	}
}

// assertNoPlaintext is the zero-plaintext reverse assertion every redaction
// case owes: after serialization, no planted secret may appear anywhere in
// the document — as a value, a key, or a fragment of either.
func assertNoPlaintext(t *testing.T, label string, serialized string, secrets map[string]string) {
	t.Helper()
	for name, secret := range secrets {
		if secret == "" {
			continue
		}
		if strings.Contains(serialized, secret) {
			t.Errorf("%s: plaintext %q (%s) appears in serialized detail:\n%s", label, secret, name, serialized)
		}
	}
}
