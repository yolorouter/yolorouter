package gateway

// rate_limit_evidence.go parses what an upstream says about a key's own
// rate limits when it rejects with a 429, and turns that evidence into
// two things: the learned rows to persist (the observed limits), and the
// bench duration for the key that just got limited.
//
// Three dialects are understood, and only these three — OpenAI-style
// x-ratelimit-* headers, Anthropic-style anthropic-ratelimit-* headers,
// and Gemini's error-body RetryInfo/quota details. Free-text variants
// ("Limit 60 per minute" prose) are deliberately NOT parsed: a wrong
// guess writes into a table whose limit values only move down, so a
// misread ceiling sticks around. Better to learn nothing than to learn
// the wrong number.
//
// Every parser here is total: garbage input yields zero values, never an
// error, because the evidence arrives on a failure path where the only
// sane response to unparseable input is to ignore it.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// observedBenchCeiling caps how long rate-limit reset EVIDENCE can bench a
// key. The standing Retry-After ceiling (retryAfterCeiling, 10m) exists
// because a lone header can lie; the higher cap here is justified by
// corroboration — the status line already proved a 429, and a
// rate-limit-family reset names a concrete window. Even so, a hostile
// upstream must not be able to park a key for a week, so the cap stands.
const observedBenchCeiling = 24 * time.Hour

// rateLimitMeterEvidence is what one meter (requests or tokens) of one
// 429 response said. Pointer fields distinguish "upstream did not say"
// (nil) from "upstream said zero" — remaining really does reach 0 at the
// wall, and learning that matters.
type rateLimitMeterEvidence struct {
	Meter      string
	Limit      *int64
	Remaining  *int64
	WindowSecs *int64
	// ResetAt is when the limiting window reopens, normalized to an
	// absolute time. Zero means no reset was parseable.
	ResetAt time.Time
}

// parseRateLimitHeaderEvidence reads the OpenAI-style and Anthropic-style
// header families. A meter yields evidence when ANY of its fields parses;
// the two families are merged field-by-field rather than either-wins, so
// an OpenAI-compatible gateway that also sets the Anthropic spellings
// contributes whatever each family actually carries.
//
// The generic x-ratelimit-limit/-remaining/-reset trio (no meter suffix)
// is consulted only when neither specific family produced anything: it is
// the shape small OpenAI-compatible gateways use, and mixing it with the
// metered families would double-count one opinion. Its evidence is
// attributed to the requests meter — generic gateways that set it at all
// are counting calls.
func parseRateLimitHeaderEvidence(h http.Header, now time.Time) []rateLimitMeterEvidence {
	var out []rateLimitMeterEvidence
	for _, meter := range []string{MeterRequests, MeterTokens} {
		ev := rateLimitMeterEvidence{Meter: meter}
		ev.Limit = firstInt64(
			h.Get("x-ratelimit-limit-"+meter),
			h.Get("anthropic-ratelimit-"+meter+"-limit"),
		)
		ev.Remaining = firstInt64(
			h.Get("x-ratelimit-remaining-"+meter),
			h.Get("anthropic-ratelimit-"+meter+"-remaining"),
		)
		// Resets: OpenAI style carries a duration ("6ms", "1d"); Anthropic
		// style carries an RFC3339 timestamp. Both normalize to ResetAt.
		if d := parseGoDurationHeader(h.Get("x-ratelimit-reset-" + meter)); d > 0 {
			ev.ResetAt = now.Add(d)
		} else if at, ok := parseRFC3339Header(h.Get("anthropic-ratelimit-" + meter + "-reset")); ok {
			ev.ResetAt = at
		}
		if ev.Limit != nil || ev.Remaining != nil || !ev.ResetAt.IsZero() {
			out = append(out, ev)
		}
	}
	if len(out) > 0 {
		return out
	}
	limit := parseInt64Header(h.Get("x-ratelimit-limit"))
	remaining := parseInt64Header(h.Get("x-ratelimit-remaining"))
	var resetAt time.Time
	if d := parseGenericResetHeader(h.Get("x-ratelimit-reset"), now); d > 0 {
		resetAt = now.Add(d)
	}
	if limit == nil && remaining == nil && resetAt.IsZero() {
		return nil
	}
	return []rateLimitMeterEvidence{{
		Meter:     MeterRequests,
		Limit:     limit,
		Remaining: remaining,
		ResetAt:   resetAt,
	}}
}

// geminiRateLimitEvidence is the body-side dialect: Gemini reports its
// rate limits in the error payload (google.rpc.RetryInfo carries a
// retryDelay; google.rpc.QuotaFailure violations name the quota metric,
// whose suffix says which window tripped). WindowSecs is 0 when the
// metric named no window word — many metrics are windowless counters.
type geminiRateLimitEvidence struct {
	RetryDelay time.Duration
	WindowSecs int64
}

// parseGeminiRateLimitBody reports ok=false when the body carries no
// RetryInfo delay AND no windowed quota metric — a 429 body that says
// neither teaches nothing and must not produce a row.
func parseGeminiRateLimitBody(body []byte) (ev geminiRateLimitEvidence, ok bool) {
	var parsed struct {
		Error struct {
			Details []struct {
				Type       string `json:"@type"`
				RetryDelay string `json:"retryDelay"`
				Violations []struct {
					QuotaMetric string `json:"quotaMetric"`
				} `json:"violations"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ev, false
	}
	for _, d := range parsed.Error.Details {
		if strings.Contains(d.Type, "RetryInfo") && d.RetryDelay != "" {
			if dur := parseGoDurationHeader(d.RetryDelay); dur > 0 {
				ev.RetryDelay = dur
			}
		}
		if strings.Contains(d.Type, "QuotaFailure") {
			for _, v := range d.Violations {
				// Order matters: PerMinute is a substring-safe distinct
				// token, but check the specific windows before defaults —
				// a metric naming PerDay must not match a PerMinute probe.
				switch {
				case strings.Contains(v.QuotaMetric, "PerMinute"):
					ev.WindowSecs = 60
				case strings.Contains(v.QuotaMetric, "PerHour"):
					ev.WindowSecs = 3600
				case strings.Contains(v.QuotaMetric, "PerDay"):
					ev.WindowSecs = 86400
				}
			}
		}
	}
	return ev, ev.RetryDelay > 0 || ev.WindowSecs > 0
}

// geminiLongWindowBench converts Gemini body evidence into a bench
// candidate, 0 when the evidence carries no delay. Any positive
// structured retryDelay qualifies — whether it actually lengthens the
// standing bench is lengthenKeyBench's call (it only ever extends), so a
// delay shorter than the header-stage bench simply changes nothing. The
// cap stays here: a hostile RetryInfo must not park a key beyond
// observedBenchCeiling however honest it claims to be. A window word
// alone (PerDay) without a delay contributes no duration at all —
// corroboration without a number is not a bench.
func geminiLongWindowBench(ev geminiRateLimitEvidence) time.Duration {
	if ev.RetryDelay <= 0 {
		return 0
	}
	return min(ev.RetryDelay, observedBenchCeiling)
}

// rateLimitBenchDuration decides how long a 429 benches its key, header
// stage (the body has not been read yet). The standing rule is unchanged
// for everything that does not name a concrete long window: Retry-After,
// clamped 1s..10m, with the configured fallback when absent. A parsed
// rate-limit reset that exceeds the 10m ceiling overrides it, floored at
// the ceiling and capped at observedBenchCeiling — that is the whole
// point of parsing resets: a daily wall is not a 10-minute wall.
func rateLimitBenchDuration(h http.Header, now time.Time, fallback time.Duration) time.Duration {
	if w := longestResetWindow(h, now); w > retryAfterCeiling {
		return min(w, observedBenchCeiling)
	}
	return cooldownFromRetryAfter(h.Get("Retry-After"), now, fallback)
}

// longestResetWindow is the largest window any understood reset header
// names, 0 when none parses. Max rather than the meter that tripped —
// with both meters present the status line does not say which wall was
// hit, and under-benching on a long window is the mistake that matters.
func longestResetWindow(h http.Header, now time.Time) time.Duration {
	var longest time.Duration
	consider := func(d time.Duration) {
		if d > longest {
			longest = d
		}
	}
	for _, meter := range []string{MeterRequests, MeterTokens} {
		if d := parseGoDurationHeader(h.Get("x-ratelimit-reset-" + meter)); d > 0 {
			consider(d)
		}
		if at, ok := parseRFC3339Header(h.Get("anthropic-ratelimit-" + meter + "-reset")); ok {
			consider(at.Sub(now))
		}
	}
	// The generic reset joins only when no metered family appeared at all
	// — the same suppression rule the storage parser applies to the whole
	// trio, judged the same way (family PRESENCE, not whether its own
	// reset happened to parse), so the two paths cannot disagree about
	// which dialect owns a mixed response.
	meteredPresent := false
	for _, meter := range []string{MeterRequests, MeterTokens} {
		for _, key := range []string{
			"x-ratelimit-limit-" + meter,
			"x-ratelimit-remaining-" + meter,
			"x-ratelimit-reset-" + meter,
			"anthropic-ratelimit-" + meter + "-limit",
			"anthropic-ratelimit-" + meter + "-remaining",
			"anthropic-ratelimit-" + meter + "-reset",
		} {
			if strings.TrimSpace(h.Get(key)) != "" {
				meteredPresent = true
			}
		}
	}
	if !meteredPresent {
		if d := parseGenericResetHeader(h.Get("x-ratelimit-reset"), now); d > 0 {
			consider(d)
		}
	}
	return longest
}

// parseGoDurationHeader reads Go duration strings ("6ms", "30s", "12h"),
// plus the bare-day suffix ("1d") that OpenAI uses and time.ParseDuration
// rejects. 0 for anything unparsable — callers treat 0 as absent.
func parseGoDurationHeader(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if n := len(s); s[n-1] == 'd' {
		// Days beyond a year are upstream nonsense, not a window — reject
		// rather than trust the multiplication (and the Duration it can
		// overflow into).
		if days, err := strconv.ParseFloat(s[:n-1], 64); err == nil && days > 0 && days <= 365 {
			return time.Duration(days * 24 * float64(time.Hour))
		}
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// parseRFC3339Header reads an absolute reset timestamp, reporting ok=false
// for anything else. A timestamp in the past yields a negative duration
// upstream and is treated as absent by callers comparing against floors.
func parseRFC3339Header(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// parseGenericResetHeader reads the suffix-less x-ratelimit-reset, whose
// integer is ambiguous across gateways: small values are delta seconds,
// absurdly large ones can only be unix epoch seconds (a 30-day delta and
// a 2001-or-later epoch differ by three orders of magnitude, so the
// two-day line separates them cleanly). 0 for anything unparsable.
func parseGenericResetHeader(s string, now time.Time) time.Duration {
	n := parseInt64Header(s)
	if n == nil || *n <= 0 {
		return 0
	}
	secs := *n
	const twoDays = 2 * 24 * 3600
	if secs > twoDays {
		d := time.Unix(secs, 0).Sub(now)
		if d <= 0 {
			return 0
		}
		return d
	}
	return time.Duration(secs) * time.Second
}

// parseInt64Header reads a bare non-negative integer, nil for anything
// else — negatives included, since no meter has a negative ceiling and a
// minus sign is a formatting bug, not a fact.
func parseInt64Header(s string) *int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return nil
	}
	v := n
	return &v
}

// firstInt64 returns the first non-nil parse, so family spellings of the
// same field can be merged without preferring empty over present.
func firstInt64(headers ...string) *int64 {
	for _, h := range headers {
		if v := parseInt64Header(h); v != nil {
			return v
		}
	}
	return nil
}
