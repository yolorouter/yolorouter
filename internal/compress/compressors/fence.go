package compressors

import (
	"regexp"
	"strings"
)

// fenceDelimRe matches a CommonMark fence delimiter run: up to three leading
// spaces, then a run of at least three backticks or tildes.
var fenceDelimRe = regexp.MustCompile("^( {0,3})(`{3,}|~{3,})")

// FenceTracker walks lines and answers "is this line, or the span it sits
// in, fenced content?". It tracks the opening run's character and length the
// way CommonMark closes fences — a closer must use the same character, be at
// least as long, and carry nothing but whitespace after the run — so a
// four-backtick fence quoting a three-backtick fence stays open, and a
// tilde fence is not closed by backticks. One definition, shared by every
// compressor scan and the content detector: fenced content is somebody's
// quoted example and must pass through byte-identical everywhere.
type FenceTracker struct {
	open    bool
	ch      byte
	openLen int
}

// Line consumes one line. It returns true when the line is a fence delimiter
// (opener or closer) — the caller emits delimiter lines verbatim. Between a
// consumed opener and its closer, Inside reports true.
func (f *FenceTracker) Line(ln string) bool {
	m := fenceDelimRe.FindStringSubmatch(ln)
	if m == nil {
		return false
	}
	run := m[2]
	if !f.open {
		f.open, f.ch, f.openLen = true, run[0], len(run)
		return true
	}
	rest := ln[len(m[1])+len(run):]
	if run[0] == f.ch && len(run) >= f.openLen && strings.TrimSpace(rest) == "" {
		f.open = false
	}
	// A fence-looking line that does NOT close (wrong char, shorter run, or
	// trailing text) is quoted content; reporting true still makes the
	// caller emit it verbatim, which is exactly what quoted content needs.
	return true
}

// Inside reports whether the walk is currently between an opener and its
// closer.
func (f *FenceTracker) Inside() bool { return f.open }
