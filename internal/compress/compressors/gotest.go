package compressors

import (
	"context"
	"regexp"
	"strings"
)

var (
	runLineRe  = regexp.MustCompile(`^=== RUN\s`)
	passLineRe = regexp.MustCompile(`^\s*--- PASS:`)
	contLineRe = regexp.MustCompile(`^=== (CONT|PAUSE|NAME)\s`)
	// goSuiteRe is the package-framing gate: real go test output always ends
	// in PASS/FAIL/ok/? package lines. Isolated === RUN / --- PASS lines also
	// appear INSIDE other runners' output (pytest captured stdout, quoted
	// logs); without package framing they are not gotest's to fold. The
	// package-line alternatives demand go test's full shape — path plus a
	// duration, (cached), or a bracketed status — because a bare
	// "FAIL <path>" is also how jest labels a failing test FILE.
	goSuiteRe = regexp.MustCompile(`^(PASS|FAIL)$|^(ok|FAIL)\s+\S+\s+(\d+(\.\d+)?s\b|\(cached\)|\[)|^\?\s+\S+\s+\[no test files\]`)
	// -json event shapes. A passing test emits BOTH a run and a pass event,
	// so run events fold WITHOUT counting (counting them doubled every pass
	// and let a failing test's run event pose as a pass). Only pass events
	// carrying a "Test" field count — a Test-less pass is the package-level
	// summary (the -json equivalent of the "ok" line): it is kept verbatim
	// and its Elapsed (whole-package wall time) must not pose as a test
	// duration. jsonElapsedRe recovers the per-test duration; text PASS
	// lines use the shared parenSecondsRe.
	jsonRunRe      = regexp.MustCompile(`"Action":"run"`)
	jsonTestPassRe = regexp.MustCompile(`"Action":"pass"`)
	jsonFailRe     = regexp.MustCompile(`"Action":"fail"`)
	jsonTestField  = regexp.MustCompile(`"Test":`)
	jsonElapsedRe  = regexp.MustCompile(`"Elapsed":(\d+(?:\.\d+)?)`)
)

// GoTest is a lossless-for-signal compressor for `go test` text output. It
// drops the boilerplate of passing tests (=== RUN / --- PASS / === CONT lines,
// counted but not emitted) while preserving every failure/skip indicator:
//   - --- FAIL / --- SKIP lines
//   - panic output and error stacks
//   - ok / FAIL summary lines
//   - the full contents of fenced code blocks (mixed-content safety)
type GoTest struct{}

func (g *GoTest) Name() string { return "gotest" }

func (g *GoTest) Compress(ctx context.Context, content string) (string, error) {
	lines := strings.Split(content, "\n")
	// Package-framing gate BEFORE any folding, fence-outside lines only. The
	// -json form frames with Test-less package pass/fail events instead of
	// PASS/FAIL/ok text lines.
	sawSuite := false
	var gateFence FenceTracker
	for i, ln := range lines {
		if i%256 == 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			default:
			}
		}
		if gateFence.Line(ln) || gateFence.Inside() {
			continue
		}
		if goSuiteRe.MatchString(ln) {
			sawSuite = true
			break
		}
		if (jsonTestPassRe.MatchString(ln) || jsonFailRe.MatchString(ln)) && !jsonTestField.MatchString(ln) {
			sawSuite = true
			break
		}
	}
	if !sawSuite {
		return content, nil
	}
	var b strings.Builder
	passCount := 0
	// folded counts EVERY dropped line, including the uncounted run/CONT
	// boilerplate: when it stays zero nothing was recognized and the content
	// must come back byte-identical — trailing-newline trimming alone must
	// never register as a "win", or this chain-head compressor swallows
	// every later compressor's input.
	folded := 0
	maxDur := -1.0
	var fence FenceTracker
	for i, ln := range lines {
		if i%256 == 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			default:
			}
		}
		// Fenced code blocks are emitted verbatim, so adversarial mixed
		// content (e.g. a code block that looks like test output) is kept.
		if fence.Line(ln) || fence.Inside() {
			b.WriteString(ln)
			b.WriteByte('\n')
			continue
		}
		// Drop passing-test boilerplate (run/CONT folded uncounted, PASS
		// counted for the summary tail).
		if runLineRe.MatchString(ln) || contLineRe.MatchString(ln) {
			folded++
			continue
		}
		if passLineRe.MatchString(ln) {
			passCount++
			folded++
			if d := parenSecondsRe.FindStringSubmatch(ln); d != nil {
				maxDur = max(maxDur, parseSeconds(d[1]))
			}
			continue
		}
		// -json events: run folds uncounted; a test-level pass folds and
		// counts; a package-level pass (no Test field) is the run summary and
		// is kept below. Output events carry failure detail, never folded.
		if jsonRunRe.MatchString(ln) {
			folded++
			continue
		}
		if jsonTestPassRe.MatchString(ln) && jsonTestField.MatchString(ln) {
			passCount++
			folded++
			if d := jsonElapsedRe.FindStringSubmatch(ln); d != nil {
				maxDur = max(maxDur, parseSeconds(d[1]))
			}
			continue
		}
		// Everything else (FAIL / SKIP / stacks / ok / package pass) is kept.
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	if folded == 0 || fence.Inside() {
		// Nothing recognized to fold, or a fence ran unclosed to EOF (the
		// marker would land inside the quoted span) — either way the block
		// is not safely this compressor's; leave it untouched.
		return content, nil
	}
	out := strings.TrimRight(b.String(), "\n")
	if passCount > 0 {
		out += "\n" + collapsedPassMarker(passCount, maxDur)
	}
	return out, nil
}
