package compressors

import (
	"context"
	"regexp"
	"strings"
)

var (
	// vitestPassRe matches one passing check line — vitest and jest share the
	// leading "✓" (jest indents it under its describe blocks). Matched
	// against the ANSI-stripped form so colored output folds the same.
	vitestPassRe = regexp.MustCompile(`^\s*✓\s`)
	// The four duration shapes a reporter actually prints at a check line's
	// END — jest's parenthesized "(3 ms)" (a SPACE between number and unit)
	// and vitest's bare space-separated "3ms" / "1.24s". All end-anchored
	// and separator-strict: a test NAME ending in "(30s)" or containing
	// "utils2s" matches none of them, so no duration is ever fabricated
	// from a name. The seconds patterns cannot match the "ms" suffix — the
	// digit run is immediately followed by "m" there, not "s".
	jestParenMsRe  = regexp.MustCompile(`\((\d+(?:\.\d+)?) ms\)\s*$`)
	jestParenSecRe = regexp.MustCompile(`\((\d+(?:\.\d+)?) s\)\s*$`)
	vitestMsRe     = regexp.MustCompile(`\s(\d+(?:\.\d+)?)ms\s*$`)
	vitestSecRe    = regexp.MustCompile(`\s(\d+(?:\.\d+)?)s\s*$`)
	// vitestSuiteRe recognizes runner framing no plain ✓-bulleted checklist
	// carries: vitest's RUN header, or a NUMERIC Test Files / Tests / Test
	// Suites summary line. Folding is gated on it — "✓" alone is common in
	// ordinary prose, and so are bare words like "Tests"/"Duration"/"PASS"
	// at a heading's start ("Tests to run:"), so every alternative demands
	// runner-specific shape: a version number or a leading count.
	vitestSuiteRe = regexp.MustCompile(`^\s*(RUN\s+v\d+\.\d+|(Test Files|Test Suites:|Tests:?)\s+\d+\s+(passed|failed|skipped|todo|total))`)
)

// Vitest is a lossless-for-signal compressor for vitest and jest terminal
// output — the two print the same shape and one pass covers both. It folds
// passing check lines ("✓ …") into one count marker carrying the max folded
// duration, and preserves everything that describes a problem:
//   - failing checks (× / ✕ / ❯ lines) and their assertion diffs
//   - error stacks and code frames
//   - the run summaries (Test Files / Tests / Snapshots / Time / Duration)
//   - the full contents of fenced code blocks (mixed-content safety)
//
// Output with no passing check to fold is returned unchanged so the chain
// can hand the block to the next candidate.
type Vitest struct{}

func (v *Vitest) Name() string { return "vitest" }

func (v *Vitest) Compress(ctx context.Context, content string) (string, error) {
	lines := strings.Split(content, "\n")
	// Suite-signature gate BEFORE any folding: without runner framing the ✓
	// lines are somebody's checklist, not checks. Fenced lines don't count as
	// evidence — a quoted example must not authorize folding the rest.
	sawSuite := false
	var gateFence FenceTracker
	for i, raw := range lines {
		if i%256 == 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			default:
			}
		}
		if gateFence.Line(raw) || gateFence.Inside() {
			continue
		}
		if vitestSuiteRe.MatchString(ansiRe.ReplaceAllString(raw, "")) {
			sawSuite = true
			break
		}
	}
	if !sawSuite {
		return content, nil
	}
	var b strings.Builder
	passCount := 0
	maxDur := -1.0
	var fence FenceTracker
	for i, raw := range lines {
		if i%256 == 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			default:
			}
		}
		if fence.Line(raw) || fence.Inside() {
			b.WriteString(raw)
			b.WriteByte('\n')
			continue
		}
		ln := ansiRe.ReplaceAllString(raw, "")
		// Only a duration-suffixed ✓ line is provably a reporter result: the
		// suite signature says the BLOCK is a test report, but a failing
		// test's captured stdout can print ✓-bulleted lines of its own, and
		// those must survive. A duration-less ✓ line therefore stays — a
		// short line kept beats a captured signal deleted.
		if vitestPassRe.MatchString(ln) {
			if d := jestParenMsRe.FindStringSubmatch(ln); d != nil {
				passCount++
				maxDur = max(maxDur, parseSeconds(d[1])/1000)
				continue
			}
			if d := jestParenSecRe.FindStringSubmatch(ln); d != nil {
				passCount++
				maxDur = max(maxDur, parseSeconds(d[1]))
				continue
			}
			if d := vitestMsRe.FindStringSubmatch(ln); d != nil {
				passCount++
				maxDur = max(maxDur, parseSeconds(d[1])/1000)
				continue
			}
			if d := vitestSecRe.FindStringSubmatch(ln); d != nil {
				passCount++
				maxDur = max(maxDur, parseSeconds(d[1]))
				continue
			}
		}
		b.WriteString(raw)
		b.WriteByte('\n')
	}
	if passCount == 0 || fence.Inside() {
		// Nothing folded, or a fence ran unclosed to EOF (the marker would
		// land inside the quoted span) — leave the block untouched.
		return content, nil
	}
	return strings.TrimRight(b.String(), "\n") + "\n" + collapsedPassMarker(passCount, maxDur), nil
}
