package compressors

import (
	"context"
	"strings"
	"testing"
)

func TestLogStripsANSIAndFoldsDupes(t *testing.T) {
	in := "building...\x1b[32mok\x1b[0m\n" +
		strings.Repeat("retrying connection timeout\n", 5) +
		"done\n"
	out, err := (&Log{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatal("ANSI escapes should be stripped")
	}
	if strings.Count(out, "retrying connection timeout") != 1 {
		t.Fatalf("consecutive duplicate lines should fold to 1 + count, got:\n%s", out)
	}
	if !strings.Contains(out, "done") {
		t.Fatal("non-duplicate lines must be preserved")
	}
}

func TestLogPreservesDistinctLines(t *testing.T) {
	in := "line a\nline b\nline c\n"
	out, _ := (&Log{}).Compress(context.Background(), in)
	for _, w := range []string{"line a", "line b", "line c"} {
		if !strings.Contains(out, w) {
			t.Fatalf("distinct lines must not be dropped: %s", w)
		}
	}
}

// TestLogPreservesFencedContentVerbatim: the generic log pass is the chain's
// last resort and sees everything the specialized compressors declined — a
// quoted example's duplicate lines, blank spacing and escapes are the
// example, not noise.
func TestLogPreservesFencedContentVerbatim(t *testing.T) {
	in := "WARN something happened\n" +
		"```\n" +
		"Progress: resolved 1, reused 0, downloaded 0, added 0\n" +
		"Progress: resolved 1, reused 0, downloaded 0, added 0\n" +
		"\n\n" +
		"\x1b[32mkept escape\x1b[0m\n" +
		"```\n"
	out, err := (&Log{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	wantFenced := "```\n" +
		"Progress: resolved 1, reused 0, downloaded 0, added 0\n" +
		"Progress: resolved 1, reused 0, downloaded 0, added 0\n" +
		"\n\n" +
		"\x1b[32mkept escape\x1b[0m\n" +
		"```"
	if !strings.Contains(out, wantFenced) {
		t.Fatalf("fenced span must survive byte-identical; got:\n%q", out)
	}
}
