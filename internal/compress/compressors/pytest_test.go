package compressors

import (
	"context"
	_ "embed"
	"strings"
	"testing"
)

//go:embed testdata/pytest_fail.txt
var pytestFailRaw string

//go:embed testdata/pytest_pass.txt
var pytestPassRaw string

func TestPytestKeepsFailuresFoldsPassed(t *testing.T) {
	out, err := (&Pytest{}).Compress(context.Background(), pytestFailRaw)
	if err != nil {
		t.Fatal(err)
	}
	// The failed test line, its traceback, and the assertion detail must
	// survive verbatim.
	for _, must := range []string{
		"test_update_user FAILED",
		"assert 404 == 200",
		"tests/test_api.py:57: AssertionError",
		"FAILED tests/test_api.py::test_update_user - assert 404 == 200",
	} {
		if !strings.Contains(out, must) {
			t.Errorf("failure detail %q must be preserved; got:\n%s", must, out)
		}
	}
	// The skip line must survive verbatim, reason included.
	if !strings.Contains(out, "SKIPPED (needs stripe key)") {
		t.Error("SKIPPED line with reason must be preserved")
	}
	// PASSED per-test lines fold into a count marker.
	if strings.Contains(out, "test_create_user PASSED") {
		t.Error("passing test lines must be folded")
	}
	if !strings.Contains(out, "[10 passed (collapsed)") {
		t.Errorf("fold marker with count missing; got:\n%s", out)
	}
	// The session summary line survives (total counts + wall time).
	if !strings.Contains(out, "1 failed, 10 passed, 1 skipped in 2.34s") {
		t.Error("session summary line must be preserved")
	}
	if len(out) >= len(pytestFailRaw) {
		t.Fatalf("output should be shorter: out=%d raw=%d", len(out), len(pytestFailRaw))
	}
}

func TestPytestAllPassMarkerCarriesMaxDuration(t *testing.T) {
	out, err := (&Pytest{}).Compress(context.Background(), pytestPassRaw)
	if err != nil {
		t.Fatal(err)
	}
	// Durations are present in the original output (slowest-durations block),
	// so the fold marker must carry the max passing-test duration.
	if !strings.Contains(out, "[10 passed (collapsed), max 0.30s]") {
		t.Errorf("marker with max duration missing; got:\n%s", out)
	}
	// The durations block itself is signal (per-test timing) and survives.
	if !strings.Contains(out, "0.30s call     tests/test_core.py::test_slow_pipeline") {
		t.Error("durations block must be preserved verbatim")
	}
}

func TestPytestMarkerWithoutDurationsIsCountOnly(t *testing.T) {
	in := "collected 2 items\n\n" +
		"tests/test_a.py::test_x PASSED                               [ 50%]\n" +
		"tests/test_a.py::test_y PASSED                               [100%]\n\n" +
		"============================== 2 passed in 0.10s ==============================\n"
	out, err := (&Pytest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[2 passed (collapsed)]") {
		t.Errorf("count-only marker missing; got:\n%s", out)
	}
	if strings.Contains(out, "max") {
		t.Errorf("no per-test durations in the input, so the marker must not invent one; got:\n%s", out)
	}
}

func TestPytestANSIColoredPassLinesStillFold(t *testing.T) {
	in := "collected 2 items\n\n" +
		"tests/test_a.py::test_x \x1b[32mPASSED\x1b[0m\x1b[36m [ 50%]\x1b[0m\n" +
		"tests/test_a.py::test_y \x1b[32mPASSED\x1b[0m\x1b[36m [100%]\x1b[0m\n\n" +
		"\x1b[32m============================== \x1b[32m2 passed\x1b[0m\x1b[32m in 0.05s\x1b[0m\x1b[32m ==============================\x1b[0m\n"
	out, err := (&Pytest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[2 passed (collapsed)]") {
		t.Errorf("ANSI-colored pass lines must still fold; got:\n%s", out)
	}
	if strings.Contains(out, "test_x") {
		t.Errorf("colored pass line must be folded away; got:\n%s", out)
	}
}

func TestPytestFencedBlockPreservedVerbatim(t *testing.T) {
	in := "collected 1 items\n\n" +
		"tests/test_a.py::test_x PASSED                               [100%]\n" +
		"```\ntests/test_b.py::test_inside_fence PASSED\n```\n" +
		"============================== 1 passed in 0.01s ==============================\n"
	out, err := (&Pytest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "tests/test_b.py::test_inside_fence PASSED") {
		t.Error("fenced content must be preserved verbatim, never folded")
	}
}

func TestPytestIdempotent(t *testing.T) {
	once, err := (&Pytest{}).Compress(context.Background(), pytestFailRaw)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := (&Pytest{}).Compress(context.Background(), once)
	if err != nil {
		t.Fatal(err)
	}
	if twice != once {
		t.Fatalf("second compression must be a no-op:\nonce:\n%s\ntwice:\n%s", once, twice)
	}
}

func TestPytestForeignContentUntouched(t *testing.T) {
	in := "=== RUN   TestA\n--- PASS: TestA (0.00s)\nPASS\nok  \tpkg\t0.1s"
	out, err := (&Pytest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("go test output is not pytest's to touch:\nin:\n%s\nout:\n%s", in, out)
	}
}

//go:embed testdata/pytest_ansi_fence.txt
var pytestAnsiFenceRaw string

func TestPytestANSIAndFenceCorpus(t *testing.T) {
	out, err := (&Pytest{}).Compress(context.Background(), pytestAnsiFenceRaw)
	if err != nil {
		t.Fatal(err)
	}
	// Colored PASSED lines fold; the fenced block inside the failure body is
	// preserved verbatim, PASSED text and all.
	if strings.Contains(out, "test_render") {
		t.Errorf("colored pass lines must be folded; got:\n%s", out)
	}
	if !strings.Contains(out, "[2 passed (collapsed)]") {
		t.Errorf("fold marker missing; got:\n%s", out)
	}
	if !strings.Contains(out, "tests/test_fake.py::test_inside_fence PASSED") {
		t.Error("fenced content must survive verbatim")
	}
	if !strings.Contains(out, "FAILED tests/test_cli.py::test_doc_example - AssertionError") {
		t.Error("failure summary must survive")
	}
}

// TestPytestStatusMustFollowTheNodeID: PASSED counts only as the status
// field right after the node id — a SKIP reason that merely CONTAINS the
// word PASSED must survive verbatim and must not bump the pass count.
func TestPytestStatusMustFollowTheNodeID(t *testing.T) {
	in := "collected 2 items\n\n" +
		"tests/test_a.py::test_x PASSED                               [ 50%]\n" +
		"tests/test_a.py::test_y SKIPPED (requires PASSED status upstream)   [100%]\n\n" +
		"========================= 1 passed, 1 skipped in 0.10s =========================\n"
	out, err := (&Pytest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "SKIPPED (requires PASSED status upstream)") {
		t.Errorf("skip reason containing the word PASSED must survive; got:\n%s", out)
	}
	if !strings.Contains(out, "[1 passed (collapsed)]") {
		t.Errorf("only the actual pass folds; got:\n%s", out)
	}
}

// TestPytestRequiresSessionFraming: a lone "path::test PASSED" line inside
// ANOTHER runner's report (a vitest failure capturing python subprocess
// output) is not pytest's to fold — real pytest output always carries its
// session framing (collected header or ===-bars).
func TestPytestRequiresSessionFraming(t *testing.T) {
	in := " RUN  v1.6.0 /home/user/proj\n\n" +
		" × src/py.test.ts > wraps pytest 12ms\n" +
		"   → subprocess reported:\n" +
		"tests/test_inner.py::test_ok PASSED\n" +
		" Tests  1 failed (1)\n"
	out, err := (&Pytest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("captured pytest lines inside a vitest report are not pytest's to fold:\nin:\n%s\nout:\n%s", in, out)
	}
}

// TestPytestGenericFailedBannerIsNotSessionFraming: an ===-framed error
// banner from another tool ("=== subprocess failed ===") is not pytest
// framing — only pytest's exact section titles and its counted "... in
// X.XXs" summary qualify.
func TestPytestGenericFailedBannerIsNotSessionFraming(t *testing.T) {
	in := " RUN  v1.6.0 /home/user/proj\n\n" +
		" × src/py.test.ts > wraps pytest 12ms\n" +
		"=== subprocess failed ===\n" +
		"tests/test_inner.py::test_ok PASSED\n" +
		" Tests  1 failed (1)\n"
	out, err := (&Pytest{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("a generic failed banner must not authorize pytest folding:\nin:\n%s\nout:\n%s", in, out)
	}
}
