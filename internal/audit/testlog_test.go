package audit

import (
	"strings"

	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// swapForwardToGlobal replaces the nil-Logger fallback's dispatch to the
// process-global pkg/logger with a test capture, returning a restore
// function. The fallback's own test needs this seam because pkg/logger
// cannot be pointed at an observer core from outside; package tests run
// sequentially, so swapping a package-level var is race-free here.
func swapForwardToGlobal(f func(zapcore.Entry, []zapcore.Field)) func() {
	old := forwardToGlobal
	forwardToGlobal = f
	return func() { forwardToGlobal = old }
}

// newObserverCore builds a zap core recording everything at or above
// level. The worker's loss-reporting contract lives in its log lines, so
// the tests assert on real log entries rather than mirroring state in a
// parallel counter that could disagree with what operators actually see.
func newObserverCore(level zapcore.Level) (zapcore.Core, *observer.ObservedLogs) {
	return observer.New(level)
}

// logEntry is one recorded line flattened for assertions: the message and
// the structured fields beside it.
type logEntry struct {
	message string
	fields  []zapcore.Field
}

// observerSink wraps the observed logs with the small query surface the
// tests use.
type observerSink struct {
	logs *observer.ObservedLogs
}

// warnEntries returns the Warn-level entries whose message contains frag.
func (s *observerSink) warnEntries(frag string) []logEntry {
	var out []logEntry
	for _, e := range s.logs.FilterLevelExact(zapcore.WarnLevel).All() {
		if strings.Contains(e.Message, frag) {
			out = append(out, logEntry{message: e.Message, fields: e.Context})
		}
	}
	return out
}

// allMessages returns every recorded message, for failure output.
func (s *observerSink) allMessages() []string {
	var out []string
	for _, e := range s.logs.All() {
		out = append(out, e.Message)
	}
	return out
}

// int64Field digs one integer field out of an entry, whichever integer
// flavor the logger used (zap.Int and zap.Int64 both store in .Integer).
func int64Field(e logEntry, key string) (int64, bool) {
	for _, f := range e.fields {
		if f.Key != key {
			continue
		}
		switch f.Type {
		case zapcore.Int64Type, zapcore.Int32Type:
			return f.Integer, true
		}
	}
	return 0, false
}
