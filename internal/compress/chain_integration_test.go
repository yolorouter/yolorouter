package compress

import (
	"strings"
	"testing"
)

// The build chain tries compressors in order and the first one that shrinks
// the block wins. That contract only holds if a compressor that recognized
// NOTHING returns its input untouched — trailing-newline trimming alone must
// never count as a win, or the chain head swallows every later compressor's
// content. These tests run the real chain the way runCompress does and
// assert WHICH compressor fired.

// chainWinner replicates runCompress's per-block compressor walk: first
// accepted (strictly shorter) result wins.
func chainWinner(t *testing.T, content string) (name string, out string) {
	t.Helper()
	if ct := detectContentType(content); ct != ContentBuildOutput {
		t.Fatalf("content not detected as build output (got %v):\n%s", ct, content)
	}
	for _, c := range buildCompressors {
		r, err := c.Compress(t.Context(), content)
		if err != nil {
			continue
		}
		if acceptCompressed(content, r) {
			return c.Name(), r
		}
	}
	return "", content
}

func TestChainRoutesPytestToPytestCompressor(t *testing.T) {
	in := "============================= test session starts ==============================\n" +
		"collected 2 items\n\n" +
		"tests/test_a.py::test_x PASSED                               [ 50%]\n" +
		"tests/test_a.py::test_y PASSED                               [100%]\n\n" +
		"============================== 2 passed in 0.10s ==============================\n"
	name, out := chainWinner(t, in)
	if name != "pytest" {
		t.Fatalf("winner = %q, want pytest; out:\n%s", name, out)
	}
	if !strings.Contains(out, "[2 passed (collapsed)]") {
		t.Errorf("pytest marker missing; got:\n%s", out)
	}
}

func TestChainRoutesVitestToVitestCompressor(t *testing.T) {
	in := " RUN  v1.6.0 /home/user/proj\n\n" +
		" ✓ src/a.test.ts > adds 2ms\n" +
		" ✓ src/a.test.ts > subtracts 1ms\n\n" +
		" Test Files  1 passed (1)\n" +
		"      Tests  2 passed (2)\n" +
		"   Duration  0.30s\n"
	name, out := chainWinner(t, in)
	if name != "vitest" {
		t.Fatalf("winner = %q, want vitest; out:\n%s", name, out)
	}
}

func TestChainRoutesNpmToNpmCompressor(t *testing.T) {
	in := "npm warn deprecated inflight@1.0.6: This module is not supported\n" +
		"npm warn deprecated glob@7.2.3: unsupported\n\n" +
		"added 12 packages, and audited 13 packages in 2s\n\n" +
		"found 0 vulnerabilities\n"
	name, out := chainWinner(t, in)
	if name != "npm" {
		t.Fatalf("winner = %q, want npm; out:\n%s", name, out)
	}
}

func TestChainRoutesPnpmToPnpmCompressor(t *testing.T) {
	in := "Progress: resolved 1, reused 0, downloaded 0, added 0\n" +
		"Progress: resolved 90, reused 80, downloaded 10, added 0\n" +
		"Progress: resolved 100, reused 90, downloaded 10, added 100, done\n" +
		"Packages: +100\n" +
		"Done in 2.0s\n"
	name, out := chainWinner(t, in)
	if name != "pnpm" {
		t.Fatalf("winner = %q, want pnpm; out:\n%s", name, out)
	}
}

// A verbose npm install whose first hundreds of lines are http fetch traces
// must still be recognized — the summary sits at the very end.
func TestDetectNpmVerboseFetchLog(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 300; i++ {
		b.WriteString("npm http fetch GET 200 https://registry.npmjs.org/pkg-x 12ms (cache miss)\n")
	}
	b.WriteString("added 300 packages in 9s\n")
	if ct := detectContentType(b.String()); ct != ContentBuildOutput {
		t.Fatalf("verbose npm fetch log should be BuildOutput, got %v", ct)
	}
}
