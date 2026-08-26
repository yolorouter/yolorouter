package compressors

import (
	"context"
	"regexp"
	"strings"
)

// ansiRe matches CSI escape sequences.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// StripANSI removes CSI escape sequences. Exported for the content detector:
// detection and compression must strip with the SAME definition, or a
// colored log recognized by a compressor is never routed to it.
func StripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// Log is a lossless log compressor: it strips ANSI escapes, folds runs of
// consecutive identical lines into a single line plus a duplicate count, and
// collapses runs of blank lines to a single blank line. It never deletes a
// line whose content differs from the previous one.
type Log struct{}

func (l *Log) Name() string { return "log" }

func (l *Log) Compress(ctx context.Context, content string) (string, error) {
	lines := strings.Split(content, "\n")
	var b strings.Builder
	var prev string
	var dupCount int
	var blankRun int
	flush := func() {
		writeDupBlock(&b, prev, dupCount)
		dupCount = 0
	}
	var fence FenceTracker
	for i, raw := range lines {
		if i%256 == 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			default:
			}
		}
		// Fenced spans are quoted examples: their duplicate lines, blank
		// spacing and escapes ARE the content. Emitted verbatim, with any
		// pending duplicate run flushed first so the fold cannot straddle
		// the fence boundary.
		if fence.Line(raw) || fence.Inside() {
			flush()
			blankRun = 0
			prev = ""
			b.WriteString(raw)
			b.WriteByte('\n')
			continue
		}
		line := ansiRe.ReplaceAllString(raw, "")
		// Collapse consecutive blank lines to a single one.
		if strings.TrimSpace(line) == "" {
			flush()
			blankRun++
			if blankRun == 1 {
				b.WriteByte('\n')
			}
			prev = ""
			continue
		}
		blankRun = 0
		if line == prev && prev != "" {
			dupCount++
			continue
		}
		flush()
		prev = line
		dupCount = 1
	}
	flush()
	if fence.Inside() {
		// A fence ran unclosed to EOF: the whole tail is a quoted span whose
		// exact bytes matter, so the block is not safely foldable at all.
		return content, nil
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
