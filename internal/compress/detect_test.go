package compress

import "testing"

func TestDetectBuildOutput(t *testing.T) {
	gotest := "=== RUN   TestFoo\n--- PASS: TestFoo (0.00s)\nPASS\nok  \tpkg/foo\t0.1s\n"
	if ct := detectContentType(gotest); ct != ContentBuildOutput {
		t.Fatalf("go test output should be detected as BuildOutput, got %v", ct)
	}
}

func TestDetectPlainTextFallback(t *testing.T) {
	if ct := detectContentType("just some prose without log markers"); ct != ContentPlainText {
		t.Fatalf("plain prose should fall back to PlainText, got %v", ct)
	}
	if ct := detectContentType(""); ct != ContentPlainText {
		t.Fatalf("empty string should be PlainText, got %v", ct)
	}
}

func TestDetectGitDiff(t *testing.T) {
	d := "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1,3 +1,4 @@\n func f() {\n-\tx := 1\n+\tx := 2\n+\ty := 3\n }\n"
	if detectContentType(d) != ContentGitDiff {
		t.Fatal("expected GitDiff detection")
	}
}

func TestDetectSearchResults(t *testing.T) {
	s := "src/main.go:42:func process() {\nsrc/util.go:13:\treturn nil\nlib/x.go:7:type X struct{}\n"
	if detectContentType(s) != ContentSearchResults {
		t.Fatal("expected SearchResults detection")
	}
}

func TestDetectSearchResultsNoFalsePositive(t *testing.T) {
	// The label:N: form (without a path separator like / or .) must not be
	// misclassified as SearchResults.
	plain := "case:1: first\ncase:2: second\nitem:3: third\nitem:4: fourth\n"
	if ct := detectContentType(plain); ct == ContentSearchResults {
		t.Fatal("label:N: text without a path separator should not be flagged as SearchResults")
	}
}

func TestDetectPytestOutput(t *testing.T) {
	// An all-pass verbose run has no ERROR/FAIL anchors, so pytest needs its
	// own: the ::-status test lines, the ===-bars, and the collected line.
	allPass := "============================= test session starts ==============================\n" +
		"collected 2 items\n\n" +
		"tests/test_a.py::test_x PASSED                               [ 50%]\n" +
		"tests/test_a.py::test_y PASSED                               [100%]\n\n" +
		"============================== 2 passed in 0.10s ==============================\n"
	if ct := detectContentType(allPass); ct != ContentBuildOutput {
		t.Fatalf("pytest verbose output should be detected as BuildOutput, got %v", ct)
	}
}

func TestPytestCompressorRegisteredInBuildChain(t *testing.T) {
	// Detection routing is only half the wiring — the build chain must
	// actually contain the pytest compressor or the block falls through to
	// the generic log pass untouched.
	for _, c := range buildCompressors {
		if c.Name() == "pytest" {
			return
		}
	}
	t.Fatal("pytest compressor missing from the build-output chain")
}

func TestDetectVitestOutput(t *testing.T) {
	// An all-pass vitest run has no generic ERROR/FAIL anchors — the ✓
	// check lines and the summary labels are its signature.
	allPass := " RUN  v1.6.0 /home/user/proj\n\n" +
		" ✓ src/a.test.ts > adds 2ms\n" +
		" ✓ src/a.test.ts > subtracts 1ms\n\n" +
		" Test Files  1 passed (1)\n" +
		"      Tests  2 passed (2)\n" +
		"   Duration  0.30s\n"
	if ct := detectContentType(allPass); ct != ContentBuildOutput {
		t.Fatalf("vitest output should be detected as BuildOutput, got %v", ct)
	}
}

func TestVitestCompressorRegisteredInBuildChain(t *testing.T) {
	for _, c := range buildCompressors {
		if c.Name() == "vitest" {
			return
		}
	}
	t.Fatal("vitest compressor missing from the build-output chain")
}

func TestDetectNpmInstallOutput(t *testing.T) {
	// A warning-free npm install is just the summary lines — they need their
	// own anchor.
	log := "added 1247 packages, and audited 1248 packages in 43s\n\n" +
		"213 packages are looking for funding\n" +
		"  run `npm fund` for details\n\n" +
		"6 moderate severity vulnerabilities\n"
	if ct := detectContentType(log); ct != ContentBuildOutput {
		t.Fatalf("npm install output should be detected as BuildOutput, got %v", ct)
	}
}

func TestDetectPnpmInstallOutput(t *testing.T) {
	log := "Progress: resolved 245, reused 240, downloaded 5, added 0\n" +
		"Progress: resolved 1247, reused 1200, downloaded 47, added 1247, done\n" +
		"Packages: +1247\n" +
		"Done in 12.3s\n"
	if ct := detectContentType(log); ct != ContentBuildOutput {
		t.Fatalf("pnpm install output should be detected as BuildOutput, got %v", ct)
	}
}

func TestPkgManagerCompressorsRegisteredInBuildChain(t *testing.T) {
	found := map[string]bool{}
	for _, c := range buildCompressors {
		found[c.Name()] = true
	}
	for _, want := range []string{"npm", "pnpm"} {
		if !found[want] {
			t.Errorf("%s compressor missing from the build-output chain", want)
		}
	}
}

func TestDetectColoredVitestOutput(t *testing.T) {
	// Real vitest output is ANSI-colored by default; detection must strip the
	// escapes before matching or every colored run lands in PlainText and the
	// compressor never sees it.
	in := " \x1b[36mRUN\x1b[39m \x1b[90mv1.6.0\x1b[39m /home/user/proj\n\n" +
		" \x1b[32m✓\x1b[39m src/a.test.ts > adds \x1b[2m2ms\x1b[22m\n" +
		" \x1b[32m✓\x1b[39m src/a.test.ts > subtracts \x1b[2m1ms\x1b[22m\n\n" +
		" \x1b[2mTests\x1b[22m  \x1b[1m\x1b[32m2 passed\x1b[39m\x1b[22m (2)\n"
	if ct := detectContentType(in); ct != ContentBuildOutput {
		t.Fatalf("colored vitest output should be BuildOutput, got %v", ct)
	}
}

func TestDetectProseChecklistStaysPlainText(t *testing.T) {
	// A human checklist uses the same ✓ glyph as a test runner but carries no
	// durations and no runner framing. It must stay PlainText — once routed
	// to the build chain, even the generic log pass can reshape it (folding
	// intentional blank spacing / repeated lines), which is content
	// modification, not compression.
	in := "Release checklist:\n\n" +
		" ✓ bump the version\n" +
		" ✓ update the changelog\n" +
		" ✓ update the changelog\n\n\n" +
		" ✓ tag the release\n" +
		" ✓ push the tag\n"
	if ct := detectContentType(in); ct != ContentPlainText {
		t.Fatalf("prose checklist should be PlainText, got %v", ct)
	}
}

func TestDetectRunnerWordsInProseAreNotAnchors(t *testing.T) {
	// Prose reuses the runner vocabulary freely; none of these lines may
	// count as build-output evidence.
	in := "RUN v2 migration before the release.\n" +
		"Tests: 2 options were considered.\n" +
		"Duration: 3 days for the rollout.\n" +
		"Time: 10:00 in the main room.\n" +
		"Done in 3 days if nothing slips.\n" +
		"Tests to run: none today.\n"
	if ct := detectContentType(in); ct != ContentPlainText {
		t.Fatalf("prose with runner words should be PlainText, got %v", ct)
	}
}

func TestDetectFencedOnlyEvidenceStaysPlainText(t *testing.T) {
	// A quoted installer example is somebody's document, not an installer
	// run — fenced lines are excluded from build-output evidence entirely,
	// so nothing downstream (including the generic log pass) reshapes it.
	in := "Here is what the installer prints:\n" +
		"```\n" +
		"Progress: resolved 1, reused 0, downloaded 0, added 0\n" +
		"Progress: resolved 9, reused 9, downloaded 0, added 9, done\n" +
		"Done in 2.0s\n" +
		"```\n" +
		"Nothing else to see.\n"
	if ct := detectContentType(in); ct != ContentPlainText {
		t.Fatalf("fenced-only evidence should stay PlainText, got %v", ct)
	}
}

func TestDetectAllPassGoTestJSON(t *testing.T) {
	in := `{"Time":"2026-08-26T10:00:00Z","Action":"run","Package":"pkg/x","Test":"TestA"}
{"Time":"2026-08-26T10:00:01Z","Action":"pass","Package":"pkg/x","Test":"TestA","Elapsed":0.25}
{"Time":"2026-08-26T10:00:01Z","Action":"pass","Package":"pkg/x","Elapsed":0.3}
`
	if ct := detectContentType(in); ct != ContentBuildOutput {
		t.Fatalf("all-pass go test -json should be BuildOutput, got %v", ct)
	}
}
