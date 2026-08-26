package compressors

import (
	"fmt"
	"regexp"
	"strconv"
)

// parenSecondsRe extracts a parenthesized "(0.12s)"-style duration — the
// shape both go test's "--- PASS: Name (0.12s)" lines and pytest's
// duration-reporting plugins print on the lines the compressors fold. It is
// end-anchored AND whitespace-separated, because the reporter prints the
// duration last and after a space: a test NAME containing or ending in
// "(30s)" must never be read as one.
var parenSecondsRe = regexp.MustCompile(`\s\((\d+(?:\.\d+)?)s\)\s*$`)

// collapsedMarker renders the generic fold marker: "[N <noun> (collapsed)]".
// One function rather than per-compressor fmt strings because the marker
// shape is a cross-compressor contract — idempotency depends on no
// compressor mistaking another's marker for foldable content.
func collapsedMarker(count int, noun string) string {
	return fmt.Sprintf("[%d %s (collapsed)]", count, noun)
}

// collapsedPassMarker is the test-output variant: a pass count, plus the max
// folded duration when the folded lines carried any. Sub-centisecond maxima
// render in milliseconds — a JS suite whose slowest check took 9ms must not
// flatten to the information-free "max 0.00s".
func collapsedPassMarker(passCount int, maxDur float64) string {
	switch {
	case maxDur >= 0.01:
		return fmt.Sprintf("[%d passed (collapsed), max %.2fs]", passCount, maxDur)
	case maxDur >= 0:
		return fmt.Sprintf("[%d passed (collapsed), max %.0fms]", passCount, maxDur*1000)
	default:
		return collapsedMarker(passCount, "passed")
	}
}

// parseSeconds converts a decimal seconds literal. The input is always a
// \d+(\.\d+)? regex capture, which ParseFloat cannot reject.
func parseSeconds(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}
