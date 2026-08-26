package compress

import (
	"regexp"
	"strings"

	"github.com/yolorouter/yolorouter/internal/compress/compressors"
)

var (
	// diffGitRe matches only lines that start with "diff --git " — the
	// unambiguous marker of a git diff (avoids mistaking isolated ---/@@ lines).
	diffGitRe    = regexp.MustCompile(`^diff --git `)
	searchLineRe = regexp.MustCompile(`^[^\s:]+[./][^\s:]*:\d+:`)
)

// ContentType identifies the detected content category.
type ContentType int

const (
	ContentPlainText     ContentType = iota
	ContentBuildOutput               // go test / build log
	ContentGitDiff                   // git diff output
	ContentSearchResults             // grep / rg search output
)

// logPatterns are the anchors used to recognize build/test output.
// The set is compiled once at package init.
var logPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(ERROR|FAIL|FAILED|FATAL|CRITICAL)\b`),
	regexp.MustCompile(`(?i)\b(WARN|WARNING)\b`),
	regexp.MustCompile(`^\s*(PASS|FAIL|SKIP)\b`),
	regexp.MustCompile(`^=== RUN\b`),
	regexp.MustCompile(`^--- (PASS|FAIL|SKIP):`),
	regexp.MustCompile(`^ok\s`),
	regexp.MustCompile(`^\?\s`),
	// go test -json events: every line is a Time/Action/Package object (in
	// that emission order), and an all-pass run contains none of the text
	// anchors above.
	regexp.MustCompile(`^\{"Time":"[^"]+","Action":"(start|run|pass|fail|skip|output|bench)","Package":"`),
	regexp.MustCompile(`Traceback \(most recent call last\)`),
	// pytest: the verbose per-test lines (path::test STATUS), the ===-framed
	// section bars, and the collection header. Needed because an all-pass run
	// contains none of the generic ERROR/FAIL anchors above.
	regexp.MustCompile(`\S::\S.*\b(PASSED|FAILED|ERROR|SKIPPED|XFAIL|XPASS)\b`),
	// The ===-framed bar is anchored on pytest's own EXACT section titles
	// (plus its counted "... in X.XXs" summary) rather than the frame or a
	// status word alone — any tool can draw a "==== something failed ===="
	// banner, and that must never route content to the build chain.
	regexp.MustCompile(`^={3,} (test session starts|FAILURES|ERRORS|short test summary info|warnings summary|slowest \d+ durations) ={3,}$|^={3,}.*\b\d+ (passed|failed|error|errors|skipped|xfailed|xpassed|warnings?)\b.* in \d+(\.\d+)?s ={3,}$`),
	regexp.MustCompile(`^collected \d+ item`),
	// vitest / jest: the per-check status glyphs (only when duration-suffixed
	// — a bare "✓ item" is how humans write checklists, and treating those as
	// build output routed prose into the compressor chain), the versioned RUN
	// header, and the run-summary labels. Needed because an all-pass run
	// contains none of the generic anchors.
	regexp.MustCompile(`^\s*[✓×✕❯↓]\s.*\d+\s*m?s\)?\s*$`),
	// Every runner-summary anchor demands runner-specific shape, not just
	// the leading word: prose reuses these words freely ("RUN v2 migration",
	// "Tests: 2 options", "Duration: 3 days") and a heading must never
	// count as build-output evidence.
	regexp.MustCompile(`^\s*RUN\s+v\d+\.\d+`),
	regexp.MustCompile(`^\s*(Test Files|Tests|Test Suites|Snapshots)\s*:?\s+\d+\s+(passed|failed|skipped|todo|total)`),
	regexp.MustCompile(`^\s*(Duration|Time)\s*:?\s+\d+(\.\d+)?\s*m?s\b`),
	// npm / pnpm install logs: the npm-prefixed lines, npm's change summary,
	// and pnpm's progress / package-delta / completion lines. Needed because
	// a warning-free install carries none of the generic anchors.
	regexp.MustCompile(`^npm (warn|WARN|error|ERR!|notice|info|verb|http)`),
	regexp.MustCompile(`^(added|removed|changed|audited) \d+ package`),
	regexp.MustCompile(`^Progress: resolved \d+`),
	regexp.MustCompile(`^Packages: [+-]?\d+`),
	regexp.MustCompile(`^Done in \d+(\.\d+)?\s*m?s\b`),
	regexp.MustCompile(`\d+ packages are looking for funding`),
	regexp.MustCompile(`\d+ (low|moderate|high|critical) severity vulnerabilit`),
}

// tryDetectDiff returns ContentGitDiff when at least one line begins with
// the "diff --git " marker.
func tryDetectDiff(lines []string) (ContentType, bool) {
	for _, ln := range lines {
		if diffGitRe.MatchString(ln) {
			return ContentGitDiff, true
		}
	}
	return ContentPlainText, false
}

// tryDetectSearch recognizes grep/rg output: the fraction of non-empty lines
// matching the search-line pattern must reach the 1/3 threshold.
func tryDetectSearch(lines []string) (ContentType, bool) {
	var matched, nonEmpty int
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		nonEmpty++
		if searchLineRe.MatchString(ln) {
			matched++
		}
	}
	if nonEmpty == 0 || matched == 0 {
		return ContentPlainText, false
	}
	// Threshold: >= 33% of non-empty lines must look like search hits.
	if float64(matched)/float64(nonEmpty) >= 1.0/3.0 {
		return ContentSearchResults, true
	}
	return ContentPlainText, false
}

// detectContentType classifies content by priority:
// GitDiff -> BuildOutput -> SearchResults -> PlainText.
// A single splitLinesCapped(500) slice is reused across stages to avoid
// scanning the input more than once. Lines are ANSI-stripped before any
// pattern runs: real test-runner and installer output is colored by
// default, and the compressors strip with the same definition — detection
// must see what they will see or a recognized log is never routed to them.
func detectContentType(content string) ContentType {
	if content == "" {
		return ContentPlainText
	}
	lines := splitLinesCapped(content, 500)
	// ANSI-stripped for matching, and fenced lines dropped from the evidence
	// set entirely: a quoted example inside a code fence is somebody's
	// document, and letting it vote for BuildOutput routes prose into the
	// compressor chain (the specialized gates then decline, but the generic
	// log pass would still reshape it).
	kept := lines[:0]
	var fence compressors.FenceTracker
	for _, ln := range lines {
		if fence.Line(ln) || fence.Inside() {
			continue
		}
		kept = append(kept, compressors.StripANSI(ln))
	}
	lines = kept

	if ct, ok := tryDetectDiff(lines); ok {
		return ct
	}

	// BuildOutput is checked before SearchResults so that Go compiler
	// diagnostics (file.go:N:) are not misclassified as search hits.
	buildLines := lines
	if len(buildLines) > 200 {
		buildLines = buildLines[:200]
	}
	var matched, nonEmpty int
	for _, ln := range buildLines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		nonEmpty++
		for _, re := range logPatterns {
			if re.MatchString(ln) {
				matched++
				break
			}
		}
	}
	if nonEmpty > 0 && matched > 0 {
		confidence := 0.3 + float64(matched)/float64(nonEmpty)*0.5
		if confidence >= 0.5 {
			return ContentBuildOutput
		}
	}

	searchLines := lines
	if len(searchLines) > 100 {
		searchLines = searchLines[:100]
	}
	if ct, ok := tryDetectSearch(searchLines); ok {
		return ct
	}

	return ContentPlainText
}

// splitLinesCapped splits s on '\n' and returns at most the first n lines,
// so very large inputs are not scanned in full.
func splitLinesCapped(s string, n int) []string {
	out := make([]string, 0, n)
	start := 0
	for i := 0; i < len(s) && len(out) < n; i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if len(out) < n && start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
