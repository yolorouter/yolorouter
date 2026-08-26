package compressors

import (
	"context"
	_ "embed"
	"strings"
	"testing"
)

//go:embed testdata/gotest_pass.txt
var gotestRaw string

func TestGoTestKeepsFailuresDropsPassNames(t *testing.T) {
	out, err := (&GoTest{}).Compress(context.Background(), gotestRaw)
	if err != nil {
		t.Fatal(err)
	}
	// Failure signal lines must be preserved verbatim.
	if !strings.Contains(out, "--- FAIL") {
		t.Fatal("failure case lines must be preserved")
	}
	// Per-line PASS boilerplate should be collapsed, so the output is
	// substantially shorter than the input.
	if len(out) >= len(gotestRaw) {
		t.Fatalf("output should be much shorter: out=%d raw=%d", len(out), len(gotestRaw))
	}
	// The total pass count must still be represented (summary or count).
	if !strings.Contains(out, "passed") && !strings.Contains(out, "PASS") {
		t.Fatal("pass summary/count must be preserved")
	}
}

func TestGoTestPreservesSkipXfailNames(t *testing.T) {
	in := "=== RUN   TestA\n--- PASS: TestA (0.00s)\n=== RUN   TestB\n--- SKIP: TestB (0.00s)\n    b_test.go:9: needs net\nPASS\nok  \tpkg\t0.1s\n"
	out, _ := (&GoTest{}).Compress(context.Background(), in)
	if !strings.Contains(out, "TestB") {
		t.Fatal("SKIP case name must be preserved")
	}
}

func TestGoTestJSONOutputEventPreservedOnFail(t *testing.T) {
	// go test -json output events carry failure-assertion detail and must
	// not be folded.
	in := `{"Action":"run","Test":"TestFoo"}
{"Action":"output","Test":"TestFoo","Output":"=== RUN   TestFoo\n"}
{"Action":"output","Test":"TestFoo","Output":"    foo_test.go:42: expected 1 got 0\n"}
{"Action":"output","Test":"TestFoo","Output":"--- FAIL: TestFoo (0.00s)\n"}
{"Action":"fail","Test":"TestFoo","Elapsed":0.001}
{"Action":"pass","Test":"TestBar","Elapsed":0.001}
{"Action":"fail","Package":"pkg/foo","Elapsed":0.01}
`
	out, err := (&GoTest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	// The failure detail line (an output event) must be preserved.
	if !strings.Contains(out, "foo_test.go:42") {
		t.Fatal("go test -json output event with failure detail must not be folded")
	}
	// run/pass events should be folded.
	if strings.Contains(out, `"Action":"run"`) || strings.Contains(out, `"Action":"pass"`) {
		t.Fatal("run/pass events should be folded")
	}
}

func TestGoTestAdversarialKeepsCodeBlock(t *testing.T) {
	// Adversarial mixed content: a fenced code block must not be mistaken
	// for go test output and deleted.
	in := "ok  \tpkg\t0.1s\n```go\nfunc Foo() { panic(\"x\") }\n```\n"
	out, _ := (&GoTest{}).Compress(context.Background(), in)
	if !strings.Contains(out, "func Foo()") {
		t.Fatal("fenced code block content must not be deleted (mixed-content adversarial)")
	}
}

// === gotest duration-marker upgrade ======================================

func TestGoTestMarkerCarriesMaxPassDuration(t *testing.T) {
	in := "=== RUN   TestA\n--- PASS: TestA (0.05s)\n=== RUN   TestB\n--- PASS: TestB (1.23s)\n=== RUN   TestC\n--- PASS: TestC (0.40s)\nPASS\nok  \tpkg\t1.7s\n"
	out, err := (&GoTest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[3 passed (collapsed), max 1.23s]") {
		t.Errorf("marker must carry the max folded pass duration; got:\n%s", out)
	}
}

func TestGoTestJSONPassEventsFeedTheMaxDuration(t *testing.T) {
	in := `{"Action":"pass","Test":"TestBar","Elapsed":0.75}
{"Action":"pass","Test":"TestBaz","Elapsed":0.2}
{"Action":"output","Test":"TestBar","Output":"done\n"}
{"Action":"pass","Package":"pkg/bar","Elapsed":0.96}
`
	out, err := (&GoTest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "max 0.75s") {
		t.Errorf("-json Elapsed must feed the marker's max; got:\n%s", out)
	}
}

func TestGoTestMarkerWithoutDurationsStaysCountOnly(t *testing.T) {
	in := "=== RUN   TestA\n--- PASS: TestA\n=== RUN   TestB\n--- PASS: TestB\nPASS\n"
	out, err := (&GoTest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[2 passed (collapsed)]") {
		t.Errorf("count-only marker missing; got:\n%s", out)
	}
	if strings.Contains(out, "max") {
		t.Errorf("no durations in the input, so the marker must not invent one; got:\n%s", out)
	}
}

func TestGoTestIdempotentWithDurationMarker(t *testing.T) {
	in := "=== RUN   TestA\n--- PASS: TestA (0.05s)\n--- FAIL: TestB (0.10s)\nFAIL\n"
	once, err := (&GoTest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := (&GoTest{}).Compress(context.Background(), once)
	if err != nil {
		t.Fatal(err)
	}
	if twice != once {
		t.Fatalf("second compression must be a no-op:\nonce:\n%s\ntwice:\n%s", once, twice)
	}
}

// TestGoTestUnrecognizedContentUntouched: content with no go test shape must
// come back byte-identical — trailing-newline trimming alone must never
// register as a "win", or GoTest (the chain head) swallows every later
// compressor's input.
func TestGoTestUnrecognizedContentUntouched(t *testing.T) {
	in := "tests/test_a.py::test_x PASSED [ 50%]\n==== 2 passed in 0.1s ====\n"
	out, err := (&GoTest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("pytest output is not gotest's to touch:\nin:  %q\nout: %q", in, out)
	}
}

// TestGoTestJSONCountsOnlyTestLevelPasses: a passing test emits BOTH a run
// and a pass event, and the package itself emits a Test-less pass at the end.
// Only the test-level pass counts; run events fold uncounted; the package
// event survives verbatim (it is the -json equivalent of the "ok" summary
// line) and its Elapsed must not pose as a test duration.
func TestGoTestJSONCountsOnlyTestLevelPasses(t *testing.T) {
	in := `{"Action":"run","Test":"TestA"}
{"Action":"run","Test":"TestB"}
{"Action":"pass","Test":"TestA","Elapsed":0.25}
{"Action":"pass","Test":"TestB","Elapsed":0.05}
{"Action":"pass","Package":"pkg/x","Elapsed":9.99}
`
	out, err := (&GoTest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[2 passed (collapsed), max 0.25s]") {
		t.Errorf("marker must count the 2 test-level passes with max 0.25s; got:\n%s", out)
	}
	if !strings.Contains(out, `"Package":"pkg/x"`) {
		t.Errorf("package-level pass event is the run summary and must survive; got:\n%s", out)
	}
}

// TestGoTestJSONFailedRunNotCountedAsPass: the run event of a test that then
// FAILS must not bump the pass count.
func TestGoTestJSONFailedRunNotCountedAsPass(t *testing.T) {
	in := `{"Action":"run","Test":"TestBad"}
{"Action":"output","Test":"TestBad","Output":"--- FAIL: TestBad (0.01s)\n"}
{"Action":"fail","Test":"TestBad","Elapsed":0.01}
`
	out, err := (&GoTest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "passed (collapsed)") {
		t.Errorf("nothing passed, so no pass marker; got:\n%s", out)
	}
}

// TestGoTestRequiresPackageFraming: === RUN / --- PASS lines appearing
// inside ANOTHER runner's output (pytest captured stdout, quoted logs) are
// not gotest's to fold. Real go test output always carries package framing
// (PASS/FAIL/ok lines, or -json package events); without it the content
// must come back byte-identical so the chain can hand it to the right
// compressor instead of deleting failure evidence.
func TestGoTestRequiresPackageFraming(t *testing.T) {
	in := "tests/test_tooling.py::test_go_wrapper FAILED\n" +
		"----------------------------- Captured stdout call -----------------------------\n" +
		"=== RUN   TestInner\n" +
		"--- PASS: TestInner (0.01s)\n" +
		"assertion detail: wrapper exited 2\n"
	out, err := (&GoTest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("captured go-test lines inside pytest output are not gotest's to fold:\nin:\n%s\nout:\n%s", in, out)
	}
}

// TestGoTestJestFailLineIsNotPackageFraming: jest's "FAIL src/x.test.ts"
// file lines share the FAIL prefix but not go test's package-line shape
// (path + duration / (cached) / bracketed status). They must not authorize
// GoTest to fold captured go-test lines out of a jest report.
func TestGoTestJestFailLineIsNotPackageFraming(t *testing.T) {
	in := "FAIL src/store/auth.test.ts\n" +
		"  ● auth store › wraps the go helper\n" +
		"    Captured output:\n" +
		"=== RUN   TestHelper\n" +
		"--- PASS: TestHelper (0.01s)\n" +
		"    helper exited 2\n"
	out, err := (&GoTest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("a jest FAIL file line must not authorize gotest folding:\nin:\n%s\nout:\n%s", in, out)
	}
}

// TestGoTestNestedFenceNotClosedByShorterRun: a four-backtick fence quoting
// a three-backtick fence stays open until a matching-length closer — the
// inner run is content, and the go-test lines inside must stay verbatim.
func TestGoTestNestedFenceNotClosedByShorterRun(t *testing.T) {
	in := "ok  \tpkg\t0.1s\n" +
		"````\n" +
		"```\n" +
		"--- PASS: TestQuoted (0.10s)\n" +
		"=== RUN   TestQuoted\n" +
		"````\n"
	out, err := (&GoTest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "--- PASS: TestQuoted (0.10s)") || !strings.Contains(out, "=== RUN   TestQuoted") {
		t.Fatalf("content inside the four-backtick fence must stay verbatim; got:\n%s", out)
	}
}

// TestGoTestUnclosedFenceFailsOpen: a fence that never closes runs to EOF —
// appending a fold marker there would land it INSIDE the quoted span, so
// the only safe move is to leave the whole block alone.
func TestGoTestUnclosedFenceFailsOpen(t *testing.T) {
	in := "=== RUN   TestA\n--- PASS: TestA (0.05s)\nPASS\nok  \tpkg\t0.1s\n" +
		"```\nquoted tail without a closer\n"
	out, err := (&GoTest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("unclosed fence must fail open:\nin:\n%s\nout:\n%s", in, out)
	}
}

// TestGoTestDurationNeverReadFromSubtestName: the reporter duration is the
// LAST parenthesized figure on a PASS line. A subtest name that itself
// contains "(30s)" must not be read as the duration.
func TestGoTestDurationNeverReadFromSubtestName(t *testing.T) {
	in := "=== RUN   TestA/timeout(30s)\n--- PASS: TestA/timeout(30s) (0.01s)\nPASS\nok  \tpkg\t0.1s\n"
	out, err := (&GoTest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[1 passed (collapsed), max 0.01s]") {
		t.Errorf("max must come from the trailing reporter duration (0.01s), never the name; got:\n%s", out)
	}
}

// TestGoTestNameEndingInDurationShapeWithoutReporterDuration: a PASS line
// whose NAME ends in "(30s)" but carries no reporter duration must fold
// count-only — the name suffix has no separating space and is not a duration.
func TestGoTestNameEndingInDurationShapeWithoutReporterDuration(t *testing.T) {
	in := "=== RUN   TestA/timeout(30s)\n--- PASS: TestA/timeout(30s)\nPASS\nok  \tpkg\t0.1s\n"
	out, err := (&GoTest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[1 passed (collapsed)]") {
		t.Errorf("count-only marker expected; got:\n%s", out)
	}
	if strings.Contains(out, "max") {
		t.Errorf("the name suffix must not become a duration; got:\n%s", out)
	}
}
