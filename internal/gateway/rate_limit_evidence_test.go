package gateway

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func headerOf(pairs ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(pairs); i += 2 {
		h.Set(pairs[i], pairs[i+1])
	}
	return h
}

// The OpenAI family: metered limit/remaining plus a duration-form reset.
func TestParseRateLimitHeaderEvidenceOpenAIFamily(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	ev := parseRateLimitHeaderEvidence(headerOf(
		"X-Ratelimit-Limit-Requests", "60",
		"X-Ratelimit-Remaining-Requests", "0",
		"X-Ratelimit-Reset-Requests", "6ms",
		"X-Ratelimit-Limit-Tokens", "100000",
	), now)
	if len(ev) != 2 {
		t.Fatalf("evidence meters = %d, want 2 (requests + tokens)", len(ev))
	}
	req := ev[0]
	if req.Meter != "requests" || req.Limit == nil || *req.Limit != 60 || req.Remaining == nil || *req.Remaining != 0 {
		t.Fatalf("requests evidence = %+v, want limit 60 remaining 0", req)
	}
	if req.ResetAt.IsZero() {
		t.Fatal("requests reset not parsed from 6ms")
	}
	tok := ev[1]
	if tok.Meter != "tokens" || tok.Limit == nil || *tok.Limit != 100000 {
		t.Fatalf("tokens evidence = %+v, want limit 100000", tok)
	}
	if !tok.ResetAt.IsZero() {
		t.Fatalf("tokens reset = %v, want zero (no reset header)", tok.ResetAt)
	}
}

// The Anthropic family: metered limit/remaining with RFC3339 resets.
func TestParseRateLimitHeaderEvidenceAnthropicFamily(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour).UTC().Format(time.RFC3339)
	ev := parseRateLimitHeaderEvidence(headerOf(
		"Anthropic-Ratelimit-Requests-Limit", "1000",
		"Anthropic-Ratelimit-Requests-Remaining", "3",
		"Anthropic-Ratelimit-Requests-Reset", reset,
	), now)
	if len(ev) != 1 {
		t.Fatalf("evidence meters = %d, want 1", len(ev))
	}
	req := ev[0]
	if req.Limit == nil || *req.Limit != 1000 || req.Remaining == nil || *req.Remaining != 3 {
		t.Fatalf("evidence = %+v, want limit 1000 remaining 3", req)
	}
	if req.ResetAt.IsZero() || now.Sub(req.ResetAt) > -50*time.Minute {
		t.Fatalf("reset = %v, want ~1h from now", req.ResetAt)
	}
}

// The generic trio is used only when no metered family spoke, and lands
// on the requests meter.
func TestParseRateLimitHeaderEvidenceGenericFallback(t *testing.T) {
	now := time.Now().UTC()
	ev := parseRateLimitHeaderEvidence(headerOf(
		"X-Ratelimit-Limit", "50",
		"X-Ratelimit-Remaining", "7",
		"X-Ratelimit-Reset", "120",
	), now)
	if len(ev) != 1 || ev[0].Meter != "requests" {
		t.Fatalf("evidence = %+v, want one requests row", ev)
	}
	if ev[0].Limit == nil || *ev[0].Limit != 50 || ev[0].Remaining == nil || *ev[0].Remaining != 7 {
		t.Fatalf("evidence = %+v, want limit 50 remaining 7", ev[0])
	}
	if ev[0].ResetAt.Sub(now) < 100*time.Second {
		t.Fatalf("reset = %v, want ~120s from now", ev[0].ResetAt)
	}

	// A metered family present suppresses the generic trio entirely.
	ev = parseRateLimitHeaderEvidence(headerOf(
		"X-Ratelimit-Limit", "50",
		"X-Ratelimit-Remaining", "7",
		"X-Ratelimit-Limit-Tokens", "40000",
	), now)
	if len(ev) != 1 || ev[0].Meter != "tokens" {
		t.Fatalf("evidence = %+v, want only the tokens row (generic suppressed)", ev)
	}
}

// Garbage is silence: no evidence, no error.
func TestParseRateLimitHeaderEvidenceGarbageTolerated(t *testing.T) {
	for name, h := range map[string]http.Header{
		"empty":     headerOf(),
		"junk":      headerOf("X-Ratelimit-Limit-Requests", "unlimited", "X-Ratelimit-Reset-Requests", "soon"),
		"negatives": headerOf("X-Ratelimit-Limit-Requests", "-5", "X-Ratelimit-Reset-Requests", "-1s"),
	} {
		if ev := parseRateLimitHeaderEvidence(h, time.Now().UTC()); len(ev) != 0 {
			t.Fatalf("%s: evidence = %+v, want none", name, ev)
		}
	}
}

func TestParseGeminiRateLimitBody(t *testing.T) {
	daily := `{"error":{"code":429,"message":"Quota exceeded","status":"RESOURCE_EXHAUSTED","details":[` +
		`{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaMetric":"generativelanguage.googleapis.com/GenerateRequestsPerDayPerProjectPerModel"}]},` +
		`{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"36000s"}]}}`
	ev, ok := parseGeminiRateLimitBody([]byte(daily))
	if !ok {
		t.Fatal("daily quota body reported no evidence")
	}
	if ev.WindowSecs != 86400 {
		t.Fatalf("window = %d, want 86400 (PerDay)", ev.WindowSecs)
	}
	if ev.RetryDelay != 36000*time.Second {
		t.Fatalf("retryDelay = %v, want 36000s", ev.RetryDelay)
	}

	perMinute := `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"32s"},` +
		`{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaMetric":"x/GenerateRequestsPerMinute"}]}]}}`
	ev, ok = parseGeminiRateLimitBody([]byte(perMinute))
	if !ok || ev.WindowSecs != 60 || ev.RetryDelay != 32*time.Second {
		t.Fatalf("perMinute evidence = %+v ok=%v, want window 60 delay 32s", ev, ok)
	}

	// No RetryInfo and no windowed metric teaches nothing.
	plain := `{"error":{"code":429,"message":"rate limited","type":"rate_limit_error"}}`
	if _, ok := parseGeminiRateLimitBody([]byte(plain)); ok {
		t.Fatal("plain body reported evidence, want none")
	}
	if _, ok := parseGeminiRateLimitBody([]byte("not json")); ok {
		t.Fatal("garbage body reported evidence, want none")
	}
}

// A lone long Retry-After keeps the standing clamp; a rate-limit reset
// that names a longer window overrides it, capped at observedBenchCeiling.
func TestRateLimitBenchDurationCorroborationRule(t *testing.T) {
	now := time.Now().UTC()
	const fallback = 30 * time.Second

	if d := rateLimitBenchDuration(headerOf("Retry-After", "3600"), now, fallback); d != retryAfterCeiling {
		t.Fatalf("lone Retry-After 3600s benched %v, want the 10m clamp", d)
	}
	if d := rateLimitBenchDuration(headerOf(), now, fallback); d != fallback {
		t.Fatalf("no evidence benched %v, want fallback", d)
	}
	if d := rateLimitBenchDuration(headerOf("X-Ratelimit-Reset-Requests", "90s", "Retry-After", "2"), now, fallback); d != 2*time.Second {
		t.Fatalf("short reset benched %v, want the Retry-After's 2s", d)
	}
	if d := rateLimitBenchDuration(headerOf("X-Ratelimit-Reset-Requests", "1h"), now, fallback); d != time.Hour {
		t.Fatalf("hourly reset benched %v, want 1h", d)
	}
	if d := rateLimitBenchDuration(headerOf("X-Ratelimit-Reset-Requests", "1d"), now, fallback); d != 24*time.Hour {
		t.Fatalf("daily reset benched %v, want the 24h cap (1d)", d)
	}
	if d := rateLimitBenchDuration(headerOf("X-Ratelimit-Reset-Requests", "7d"), now, fallback); d != observedBenchCeiling {
		t.Fatalf("weekly reset benched %v, want capped at %v", d, observedBenchCeiling)
	}
	// The generic epoch form corroborates too.
	epoch := now.Add(2 * time.Hour).Unix()
	if d := rateLimitBenchDuration(headerOf("X-Ratelimit-Reset", strconv.FormatInt(epoch, 10)), now, fallback); d < 90*time.Minute {
		t.Fatalf("epoch reset benched %v, want ~2h", d)
	}
}

func TestGeminiLongWindowBench(t *testing.T) {
	if d := geminiLongWindowBench(geminiRateLimitEvidence{RetryDelay: 10 * time.Hour}); d != 10*time.Hour {
		t.Fatalf("long delay benched %v, want 10h", d)
	}
	if d := geminiLongWindowBench(geminiRateLimitEvidence{RetryDelay: 90000 * time.Second}); d != observedBenchCeiling {
		t.Fatalf("over-cap delay benched %v, want capped at %v", d, observedBenchCeiling)
	}
	// A short delay is a candidate too — lengthenKeyBench decides whether
	// it actually extends the standing bench, and a shorter candidate
	// changes nothing there.
	if d := geminiLongWindowBench(geminiRateLimitEvidence{RetryDelay: 30 * time.Second}); d != 30*time.Second {
		t.Fatalf("short delay candidate = %v, want 30s", d)
	}
	// A window word without a delay contributes no duration at all.
	if d := geminiLongWindowBench(geminiRateLimitEvidence{WindowSecs: 86400}); d != 0 {
		t.Fatalf("window word without delay benched %v, want 0", d)
	}
}

func TestParseGoDurationHeaderDaySuffix(t *testing.T) {
	if d := parseGoDurationHeader("1d"); d != 24*time.Hour {
		t.Fatalf("1d = %v, want 24h", d)
	}
	if d := parseGoDurationHeader("6ms"); d != 6*time.Millisecond {
		t.Fatalf("6ms = %v", d)
	}
	if d := parseGoDurationHeader("soon"); d != 0 {
		t.Fatalf("garbage = %v, want 0", d)
	}
	// An absurd day count is nonsense, not a window: rejected rather than
	// multiplied into an overflowing Duration. 365d stays the last honest
	// value.
	if d := parseGoDurationHeader("365d"); d != 365*24*time.Hour {
		t.Fatalf("365d = %v, want a year", d)
	}
	if d := parseGoDurationHeader("366d"); d != 0 {
		t.Fatalf("366d = %v, want 0 (beyond a year is nonsense)", d)
	}
	if d := parseGoDurationHeader("10000d"); d != 0 {
		t.Fatalf("10000d = %v, want 0", d)
	}
}

func TestParseGenericResetHeaderDeltaVsEpoch(t *testing.T) {
	now := time.Now().UTC()
	if d := parseGenericResetHeader("120", now); d != 2*time.Minute {
		t.Fatalf("delta = %v, want 2m", d)
	}
	at := now.Add(3 * time.Hour)
	if d := parseGenericResetHeader(strconv.FormatInt(at.Unix(), 10), now); d < 2*time.Hour+50*time.Minute {
		t.Fatalf("epoch = %v, want ~3h", d)
	}
	if d := parseGenericResetHeader("0", now); d != 0 {
		t.Fatalf("zero = %v, want 0", d)
	}
	if d := parseGenericResetHeader("abc", now); d != 0 {
		t.Fatalf("garbage = %v, want 0", d)
	}
}
