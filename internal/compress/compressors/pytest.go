package compressors

import (
	"context"
	"regexp"
	"strings"
)

var (
	// pytestPassRe matches one verbose passing-test line: "path::test PASSED
	// [ 10%]" (parametrized ids included). PASSED must be the status field
	// IMMEDIATELY after the node id — matched anywhere on the line it also
	// swallowed skip/fail lines whose reason text merely contained the word
	// ("SKIPPED (requires PASSED status)"). Matched against the
	// ANSI-stripped form so colored output folds the same as plain.
	pytestPassRe = regexp.MustCompile(`^(\S+::\S+)\s+PASSED\b`)
	// pytestSessionRe is the session-framing gate: real pytest output always
	// carries the collection header or its ===-framed section bars. A lone
	// "path::test PASSED" line also appears INSIDE other runners' reports
	// (captured subprocess output); without framing it is not pytest's to
	// fold. The framed alternatives are pytest's EXACT section titles plus
	// its counted "... in X.XXs" summary — a status word alone inside a
	// frame ("=== subprocess failed ===") is any tool's error banner.
	pytestSessionRe = regexp.MustCompile(`^collected \d+ item|^={3,} (test session starts|FAILURES|ERRORS|short test summary info|warnings summary|slowest \d+ durations) ={3,}$|^={3,}.*\b\d+ (passed|failed|error|errors|skipped|xfailed|xpassed|warnings?)\b.* in \d+(\.\d+)?s ={3,}$`)
	// pytestSlowestRe matches one entry of the "slowest N durations" block:
	// "0.30s call     path::test". The block is kept verbatim (per-test
	// timing is signal); call entries additionally feed the marker's max.
	pytestSlowestRe = regexp.MustCompile(`^(\d+(?:\.\d+)?)s\s+(call|setup|teardown)\s+(\S+)`)
)

// Pytest is a lossless-for-signal compressor for pytest terminal output. It
// folds verbose passing-test lines into one count marker (with the max
// passing duration when the original output carries per-test durations) and
// preserves everything that describes a problem or a decision:
//   - FAILED / ERROR / SKIPPED / XFAIL lines, with their reasons
//   - the FAILURES section: tracebacks and assertion detail
//   - the short test summary and the final session summary line
//   - the "slowest durations" block
//   - the full contents of fenced code blocks (mixed-content safety)
//
// Output it does not recognize as pytest's (no passing line folded) is
// returned unchanged, so the compressor chain can hand the block to the next
// candidate.
type Pytest struct{}

func (p *Pytest) Name() string { return "pytest" }

func (p *Pytest) Compress(ctx context.Context, content string) (string, error) {
	lines := strings.Split(content, "\n")
	// Session-framing gate BEFORE any folding, fence-outside lines only.
	sawSession := false
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
		if pytestSessionRe.MatchString(ansiRe.ReplaceAllString(raw, "")) {
			sawSession = true
			break
		}
	}
	if !sawSession {
		return content, nil
	}
	var b strings.Builder
	passCount := 0
	maxDur := -1.0
	passedIDs := map[string]bool{}
	// Slowest-block call durations are collected per test id and resolved
	// against the folded set AFTER the walk: the block precedes nothing —
	// it always follows the test lines — but resolving late keeps this
	// independent of ordering.
	callDurations := map[string]float64{}
	var fence FenceTracker
	for i, raw := range lines {
		if i%256 == 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			default:
			}
		}
		// Fenced code blocks are emitted verbatim, so adversarial mixed
		// content (a code block that looks like pytest output) is kept.
		if fence.Line(raw) || fence.Inside() {
			b.WriteString(raw)
			b.WriteByte('\n')
			continue
		}
		ln := ansiRe.ReplaceAllString(raw, "")
		if m := pytestPassRe.FindStringSubmatch(ln); m != nil {
			passCount++
			passedIDs[m[1]] = true
			if d := parenSecondsRe.FindStringSubmatch(ln); d != nil {
				maxDur = max(maxDur, parseSeconds(d[1]))
			}
			continue
		}
		if m := pytestSlowestRe.FindStringSubmatch(ln); m != nil && m[2] == "call" {
			callDurations[m[3]] = parseSeconds(m[1])
		}
		// Everything else (failures, skips, summaries, tracebacks) is kept.
		b.WriteString(raw)
		b.WriteByte('\n')
	}
	if passCount == 0 || fence.Inside() {
		// Nothing recognized to fold, or a fence ran unclosed to EOF (the
		// marker would land inside the quoted span) — either way the block
		// is untouched.
		return content, nil
	}
	for id, d := range callDurations {
		if passedIDs[id] {
			maxDur = max(maxDur, d)
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n" + collapsedPassMarker(passCount, maxDur), nil
}
