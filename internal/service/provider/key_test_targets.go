package provider

import "encoding/json"

// KeyTestTargetResult is what ONE destination answered during a key
// verification run. A provider that declares extra protocol endpoints is
// probed at every one of them, but the columns that record the run
// (verification_status, last_test_result) only ever describe the worst
// destination — so a healthy credential whose extra endpoint speaks a
// protocol the upstream does not support is indistinguishable, from those
// columns, from a rejected credential. Keeping one of these per destination
// is what lets the admin UI name the protocol that failed and quote what the
// upstream said about it.
//
// Detail is admin-facing only, exactly like the client-level diagnostic it
// carries: it may quote the upstream's own error text and is never shown to
// end users.
type KeyTestTargetResult struct {
	Proto      string `json:"proto"`
	Outcome    int    `json:"outcome"`
	DurationMs int64  `json:"duration_ms"`
	Detail     string `json:"detail"`
}

// encodeKeyTestTargets serializes a run's per-destination results into the
// text stored in provider_keys.last_test_targets. Marshalling cannot fail —
// every field is a string or a number — so the error is discarded rather
// than given a branch no input can reach.
func encodeKeyTestTargets(targets []KeyTestTargetResult) *string {
	encoded, _ := json.Marshal(targets)
	text := string(encoded)
	return &text
}

// decodeKeyTestTargets reads the stored breakdown back for an API response.
// Deliberately lenient, like every other read of a JSON column here: nil for
// a row that has none (never tested, or last tested by a build that predates
// the column), and nil again for a value that no longer parses — a listing of
// every provider must not fail because one key holds text this build cannot
// read.
func decodeKeyTestTargets(raw *string) []KeyTestTargetResult {
	if raw == nil || *raw == "" {
		return nil
	}
	var targets []KeyTestTargetResult
	if err := json.Unmarshal([]byte(*raw), &targets); err != nil {
		return nil
	}
	return targets
}
