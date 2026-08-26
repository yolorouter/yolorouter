package compressors

import (
	"context"
	_ "embed"
	"strings"
	"testing"
)

//go:embed testdata/vitest_fail.txt
var vitestFailRaw string

//go:embed testdata/vitest_pass.txt
var vitestPassRaw string

//go:embed testdata/jest_fail.txt
var jestFailRaw string

func TestVitestKeepsFailuresFoldsPassed(t *testing.T) {
	out, err := (&Vitest{}).Compress(context.Background(), vitestFailRaw)
	if err != nil {
		t.Fatal(err)
	}
	// The failing check, its diff, and the stack pointer must survive.
	for _, must := range []string{
		"× src/store/auth.test.ts > auth store > refreshes the token on 401 34ms",
		"expected 'expired' to be 'fresh'",
		"- 'fresh'",
		"+ 'expired'",
		"src/store/auth.test.ts:42:5",
	} {
		if !strings.Contains(out, must) {
			t.Errorf("failure detail %q must be preserved; got:\n%s", must, out)
		}
	}
	// Passing checks fold into the marker with the max duration in seconds.
	if strings.Contains(out, "rounds to 2 decimals") {
		t.Error("passing check lines must be folded")
	}
	if !strings.Contains(out, "[6 passed (collapsed), max 9ms]") {
		t.Errorf("fold marker with count and max missing; got:\n%s", out)
	}
	// The run summary survives (exact totals + wall time).
	if !strings.Contains(out, "Tests  1 failed | 6 passed (7)") {
		t.Error("run summary must be preserved")
	}
	if len(out) >= len(vitestFailRaw) {
		t.Fatalf("output should be shorter: out=%d raw=%d", len(out), len(vitestFailRaw))
	}
}

func TestVitestAllPassStillFolds(t *testing.T) {
	out, err := (&Vitest{}).Compress(context.Background(), vitestPassRaw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[6 passed (collapsed), max 9ms]") {
		t.Errorf("all-pass marker missing; got:\n%s", out)
	}
	if !strings.Contains(out, "Duration  0.84s") {
		t.Error("duration summary line must be preserved")
	}
}

func TestVitestHandlesJestOutput(t *testing.T) {
	out, err := (&Vitest{}).Compress(context.Background(), jestFailRaw)
	if err != nil {
		t.Fatal(err)
	}
	// jest's failing check (✕), its assertion diff, and the code frame stay.
	for _, must := range []string{
		"✕ refreshes the token on 401 (34 ms)",
		`Expected: "fresh"`,
		`Received: "expired"`,
		"at Object.<anonymous> (src/store/auth.test.ts:42:19)",
		"Tests:       1 failed, 3 passed, 4 total",
	} {
		if !strings.Contains(out, must) {
			t.Errorf("jest detail %q must be preserved; got:\n%s", must, out)
		}
	}
	// jest's ✓ checks fold; per-test "(3 ms)" durations feed the max.
	if strings.Contains(out, "✓ rounds to 2 decimals") {
		t.Error("jest passing checks must be folded")
	}
	if !strings.Contains(out, "[3 passed (collapsed), max 5ms]") {
		t.Errorf("jest fold marker missing; got:\n%s", out)
	}
}

func TestVitestANSIColoredChecksStillFold(t *testing.T) {
	in := " \x1b[32m✓\x1b[0m src/a.test.ts > adds \x1b[2m2ms\x1b[0m\n" +
		" \x1b[32m✓\x1b[0m src/a.test.ts > subtracts \x1b[2m1ms\x1b[0m\n\n" +
		" Tests  2 passed (2)\n"
	out, err := (&Vitest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[2 passed (collapsed)") {
		t.Errorf("ANSI-colored checks must still fold; got:\n%s", out)
	}
	if strings.Contains(out, "adds") {
		t.Errorf("colored passing line must be folded away; got:\n%s", out)
	}
}

func TestVitestFencedBlockPreservedVerbatim(t *testing.T) {
	in := " ✓ src/a.test.ts > adds 2ms\n" +
		"```\n ✓ src/inside.test.ts > fenced 9ms\n```\n" +
		" Tests  1 passed (1)\n"
	out, err := (&Vitest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "✓ src/inside.test.ts > fenced 9ms") {
		t.Error("fenced content must be preserved verbatim, never folded")
	}
}

func TestVitestIdempotent(t *testing.T) {
	once, err := (&Vitest{}).Compress(context.Background(), vitestFailRaw)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := (&Vitest{}).Compress(context.Background(), once)
	if err != nil {
		t.Fatal(err)
	}
	if twice != once {
		t.Fatalf("second compression must be a no-op:\nonce:\n%s\ntwice:\n%s", once, twice)
	}
}

func TestVitestForeignContentUntouched(t *testing.T) {
	in := "tests/test_a.py::test_x PASSED [100%]\n==== 1 passed in 0.1s ====\n"
	out, err := (&Vitest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("pytest output is not vitest's to touch:\nin:\n%s\nout:\n%s", in, out)
	}
}

//go:embed testdata/jest_pass.txt
var jestPassRaw string

func TestVitestJestAllPassColoredCorpus(t *testing.T) {
	out, err := (&Vitest{}).Compress(context.Background(), jestPassRaw)
	if err != nil {
		t.Fatal(err)
	}
	// Every colored ✓ check folds; the suite summary survives.
	if strings.Contains(out, "rounds to 2 decimals") {
		t.Errorf("colored jest checks must be folded; got:\n%s", out)
	}
	if !strings.Contains(out, "[4 passed (collapsed), max 3ms]") {
		t.Errorf("all-pass marker missing; got:\n%s", out)
	}
	if !strings.Contains(out, "Tests:       4 passed, 4 total") {
		t.Error("jest summary must be preserved")
	}
}

func TestVitestDurationNeverReadFromNames(t *testing.T) {
	// The duration lives at the END of a check line. Digits inside test names
	// or file paths ("utils2s") must never be read as one — and since only
	// duration-suffixed lines are provably reporter results, these lines are
	// KEPT verbatim rather than folded on a fabricated reading.
	in := " ✓ src/utils2s.test.ts > works with 3s timeouts configured\n" +
		" ✓ src/utils2s.test.ts > detects 45s stalls quickly\n\n" +
		" Tests  2 passed (2)\n"
	out, err := (&Vitest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("duration-less check lines must survive verbatim:\nin:\n%s\nout:\n%s", in, out)
	}
}

//go:embed testdata/vitest_pass_ansi.txt
var vitestPassAnsiRaw string

func TestVitestANSIAllPassCorpus(t *testing.T) {
	out, err := (&Vitest{}).Compress(context.Background(), vitestPassAnsiRaw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "rounds to 2 decimals") {
		t.Errorf("colored vitest checks must be folded; got:\n%s", out)
	}
	if !strings.Contains(out, "[3 passed (collapsed), max 9ms]") {
		t.Errorf("fold marker missing; got:\n%s", out)
	}
}

// TestVitestChecklistWithoutSuiteSignatureUntouched: a plain ✓-bulleted
// checklist is USER CONTENT, not test output. Without a vitest/jest suite
// signature (RUN header, Test Files/Tests summary, PASS/FAIL prefix) the
// compressor must not touch it — folding it would destroy the list.
func TestVitestChecklistWithoutSuiteSignatureUntouched(t *testing.T) {
	in := "Release checklist:\n" +
		" ✓ bump the version\n" +
		" ✓ update the changelog\n" +
		" ✓ tag the release\n" +
		" ✓ push the tag\n"
	out, err := (&Vitest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("a checklist is not vitest's to touch:\nin:\n%s\nout:\n%s", in, out)
	}
}

// TestVitestProseHeadingsAreNotSuiteSignatures: common document headings
// ("Tests to run:", "Duration ...", "PASS this along") must not authorize
// folding — only real runner framing (the versioned RUN header, numeric
// Test Files/Tests/Test Suites summaries) counts.
func TestVitestProseHeadingsAreNotSuiteSignatures(t *testing.T) {
	for name, in := range map[string]string{
		"tests-to-run": "Tests to run:\n ✓ login flow\n ✓ checkout flow\n ✓ refund flow\n",
		"duration":     "Duration of the meeting:\n ✓ agenda agreed\n ✓ owners assigned\n",
		"pass-prose":   "PASS this checklist to the reviewer:\n ✓ docs updated\n ✓ tests added\n",
	} {
		out, err := (&Vitest{}).Compress(context.Background(), in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if out != in {
			t.Errorf("%s: prose checklist must be untouched:\nin:\n%s\nout:\n%s", name, in, out)
		}
	}
}

// TestVitestRunHeadingWithBareMajorVersionRejected: "RUN v2." is a prose
// heading shape, not a runner header — the version anchor demands at least
// major.minor.
func TestVitestRunHeadingWithBareMajorVersionRejected(t *testing.T) {
	in := "RUN v2. migration checklist\n" +
		" ✓ backup the database\n" +
		" ✓ apply the schema\n"
	out, err := (&Vitest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("a bare-major RUN heading must not authorize folding:\nin:\n%s\nout:\n%s", in, out)
	}
}

// TestVitestCapturedChecklistInsideRealReportSurvives: a failing test's
// captured stdout can itself print ✓-bulleted lines. The suite signature
// proves the BLOCK is a test report, not that every ✓ line is a reporter
// result — only duration-suffixed check lines fold; captured output stays.
func TestVitestCapturedChecklistInsideRealReportSurvives(t *testing.T) {
	in := " RUN  v1.6.0 /home/user/proj\n\n" +
		" ✓ src/a.test.ts > adds 2ms\n" +
		" × src/b.test.ts > setup flow 9ms\n" +
		"   → setup incomplete; steps done so far:\n" +
		" ✓ created the workspace\n" +
		" ✓ wrote the config\n\n" +
		" Tests  1 failed | 1 passed (2)\n"
	out, err := (&Vitest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{"✓ created the workspace", "✓ wrote the config"} {
		if !strings.Contains(out, must) {
			t.Errorf("captured checklist line %q must survive; got:\n%s", must, out)
		}
	}
	if strings.Contains(out, "src/a.test.ts > adds") {
		t.Errorf("the duration-suffixed passing check must still fold; got:\n%s", out)
	}
	if !strings.Contains(out, "[1 passed (collapsed), max 2ms]") {
		t.Errorf("marker must count ONLY reporter result lines; got:\n%s", out)
	}
}

// TestVitestParenDurationRequiresJestSpacing: jest prints "(30 ms)" with a
// space; a test NAME ending in "(30s)" has none and is not a duration —
// the line stays, and no max is fabricated.
func TestVitestParenDurationRequiresJestSpacing(t *testing.T) {
	in := " RUN  v1.6.0 /home/user/proj\n\n" +
		" ✓ handles timeout (30s)\n" +
		" ✓ src/a.test.ts > adds 2ms\n\n" +
		" Tests  2 passed (2)\n"
	out, err := (&Vitest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "✓ handles timeout (30s)") {
		t.Errorf("the duration-less line with a (30s) name suffix must survive; got:\n%s", out)
	}
	if !strings.Contains(out, "[1 passed (collapsed), max 2ms]") {
		t.Errorf("only the real duration folds and feeds the max; got:\n%s", out)
	}
}
