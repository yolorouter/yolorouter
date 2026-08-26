package compressors

import (
	"context"
	_ "embed"
	"strings"
	"testing"
)

//go:embed testdata/npm_install.txt
var npmInstallRaw string

//go:embed testdata/npm_fail.txt
var npmFailRaw string

//go:embed testdata/pnpm_install.txt
var pnpmInstallRaw string

//go:embed testdata/pnpm_fail.txt
var pnpmFailRaw string

func TestNpmFoldsDeprecationWarnings(t *testing.T) {
	out, err := (&Npm{}).Compress(context.Background(), npmInstallRaw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "npm warn deprecated inflight") {
		t.Error("deprecation warnings must be folded")
	}
	if !strings.Contains(out, "[5 deprecation warnings (collapsed)]") {
		t.Errorf("deprecation fold marker missing; got:\n%s", out)
	}
	// The install summary and the audit outcome are signal and survive.
	for _, must := range []string{
		"added 1247 packages, and audited 1248 packages in 43s",
		"6 moderate severity vulnerabilities",
	} {
		if !strings.Contains(out, must) {
			t.Errorf("summary line %q must be preserved; got:\n%s", must, out)
		}
	}
	if len(out) >= len(npmInstallRaw) {
		t.Fatalf("output should be shorter: out=%d raw=%d", len(out), len(npmInstallRaw))
	}
}

func TestNpmKeepsErrorLinesVerbatim(t *testing.T) {
	out, err := (&Npm{}).Compress(context.Background(), npmFailRaw)
	if err != nil {
		t.Fatal(err)
	}
	// Every npm error line describes the dependency conflict and survives.
	for _, must := range []string{
		"npm error code ERESOLVE",
		"npm error ERESOLVE unable to resolve dependency tree",
		`npm error peer react@"^18.0.0" from react-dom@18.2.0`,
		"npm error /home/user/.npm/_logs/2026-08-25T09_00_00_000Z-eresolve-report.txt",
	} {
		if !strings.Contains(out, must) {
			t.Errorf("error line %q must be preserved; got:\n%s", must, out)
		}
	}
}

func TestNpmIdempotent(t *testing.T) {
	once, err := (&Npm{}).Compress(context.Background(), npmInstallRaw)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := (&Npm{}).Compress(context.Background(), once)
	if err != nil {
		t.Fatal(err)
	}
	if twice != once {
		t.Fatalf("second compression must be a no-op:\nonce:\n%s\ntwice:\n%s", once, twice)
	}
}

func TestNpmForeignContentUntouched(t *testing.T) {
	in := "Progress: resolved 245, reused 240, downloaded 5, added 0\nDone in 12.3s\n"
	out, err := (&Npm{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("pnpm output is not npm's to touch:\nin:\n%s\nout:\n%s", in, out)
	}
}

func TestPnpmFoldsProgressKeepsFinal(t *testing.T) {
	out, err := (&Pnpm{}).Compress(context.Background(), pnpmInstallRaw)
	if err != nil {
		t.Fatal(err)
	}
	// Intermediate progress updates fold; the FINAL one (the resolved totals,
	// marked done) survives.
	if strings.Contains(out, "Progress: resolved 245") {
		t.Error("intermediate progress lines must be folded")
	}
	if !strings.Contains(out, "Progress: resolved 1247, reused 1200, downloaded 47, added 1247, done") {
		t.Errorf("final progress line must be preserved; got:\n%s", out)
	}
	if !strings.Contains(out, "progress lines (collapsed)]") {
		t.Errorf("progress fold marker missing; got:\n%s", out)
	}
	// Deprecation warnings fold like npm's.
	if strings.Contains(out, "WARN  deprecated inflight") {
		t.Error("deprecation warnings must be folded")
	}
	if !strings.Contains(out, "[2 deprecation warnings (collapsed)]") {
		t.Errorf("deprecation fold marker missing; got:\n%s", out)
	}
	// The dependency list and completion line are signal.
	for _, must := range []string{"+ vue 3.5.13", "Done in 12.3s", "Packages: +1247"} {
		if !strings.Contains(out, must) {
			t.Errorf("%q must be preserved; got:\n%s", must, out)
		}
	}
	if len(out) >= len(pnpmInstallRaw) {
		t.Fatalf("output should be shorter: out=%d raw=%d", len(out), len(pnpmInstallRaw))
	}
}

func TestPnpmKeepsErrorBlockVerbatim(t *testing.T) {
	out, err := (&Pnpm{}).Compress(context.Background(), pnpmFailRaw)
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{
		"ERR_PNPM_FETCH_404",
		"@myorg/missing-pkg is not in the npm registry, or you have no permission to fetch it.",
	} {
		if !strings.Contains(out, must) {
			t.Errorf("error detail %q must be preserved; got:\n%s", must, out)
		}
	}
}

func TestPnpmANSIColoredProgressStillFolds(t *testing.T) {
	in := "\x1b[2mProgress: resolved 1, reused 0, downloaded 0, added 0\x1b[0m\n" +
		"\x1b[2mProgress: resolved 90, reused 80, downloaded 10, added 0\x1b[0m\n" +
		"Progress: resolved 100, reused 90, downloaded 10, added 100, done\n" +
		"Done in 2.0s\n"
	out, err := (&Pnpm{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "resolved 90") {
		t.Errorf("ANSI-colored progress lines must still fold; got:\n%s", out)
	}
	if !strings.Contains(out, "Progress: resolved 100, reused 90, downloaded 10, added 100, done") {
		t.Error("final progress line must be preserved")
	}
}

func TestPnpmIdempotent(t *testing.T) {
	once, err := (&Pnpm{}).Compress(context.Background(), pnpmInstallRaw)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := (&Pnpm{}).Compress(context.Background(), once)
	if err != nil {
		t.Fatal(err)
	}
	if twice != once {
		t.Fatalf("second compression must be a no-op:\nonce:\n%s\ntwice:\n%s", once, twice)
	}
}

func TestPnpmForeignContentUntouched(t *testing.T) {
	in := "npm warn deprecated glob@7.2.3: unsupported\nadded 3 packages in 1s\n"
	out, err := (&Pnpm{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("npm output is not pnpm's to touch:\nin:\n%s\nout:\n%s", in, out)
	}
}

//go:embed testdata/npm_install_ansi.txt
var npmInstallAnsiRaw string

func TestNpmANSIColoredWarningsStillFold(t *testing.T) {
	out, err := (&Npm{}).Compress(context.Background(), npmInstallAnsiRaw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "deprecated inflight") {
		t.Errorf("ANSI-colored deprecation warnings must still fold; got:\n%s", out)
	}
	if !strings.Contains(out, "[3 deprecation warnings (collapsed)]") {
		t.Errorf("deprecation fold marker missing; got:\n%s", out)
	}
	if !strings.Contains(out, "added 312 packages, and audited 313 packages in 9s") {
		t.Error("install summary must be preserved")
	}
}

// TestPnpmFencedProgressDoesNotStealTheFinalLine: the last-progress search
// must be fence-aware like the fold walk — a fenced Progress example after
// the real final line must not demote the real totals to "intermediate".
func TestPnpmFencedProgressDoesNotStealTheFinalLine(t *testing.T) {
	in := "Progress: resolved 1, reused 0, downloaded 0, added 0\n" +
		"Progress: resolved 100, reused 90, downloaded 10, added 100, done\n" +
		"Done in 2.0s\n" +
		"```\nProgress: resolved 999, reused 0, downloaded 0, added 0\n```\n"
	out, err := (&Pnpm{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Progress: resolved 100, reused 90, downloaded 10, added 100, done") {
		t.Errorf("the real final progress line must survive; got:\n%s", out)
	}
	if !strings.Contains(out, "Progress: resolved 999") {
		t.Errorf("fenced content must survive verbatim; got:\n%s", out)
	}
}

// TestPnpmForeignPlusBarUntouched: a bare +++ separator inside content that
// carries no pnpm framing (no Progress/Packages/Done) is somebody else's
// text — folding it would delete a divider from a failure report.
func TestPnpmForeignPlusBarUntouched(t *testing.T) {
	in := "FAILED tests/test_api.py::test_update_user\n" +
		"++++++++++++++++\n" +
		"E       assert 404 == 200\n"
	out, err := (&Pnpm{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("foreign content with a plus bar must be untouched:\nin:\n%s\nout:\n%s", in, out)
	}
}

// TestPnpmIndentedAndTildeFencesRespected: CommonMark allows fences indented
// up to three spaces and tilde fences; a quoted pnpm example inside either
// must neither authorize folding nor lose its own lines.
func TestPnpmIndentedAndTildeFencesRespected(t *testing.T) {
	in := "How the installer reports progress:\n" +
		"   ```\n" +
		"Progress: resolved 1, reused 0, downloaded 0, added 0\n" +
		"Progress: resolved 9, reused 9, downloaded 0, added 9, done\n" +
		"   ```\n" +
		"~~~\nDone in 2.0s\n~~~\n"
	out, err := (&Pnpm{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("fenced-only pnpm examples must be untouched:\nin:\n%s\nout:\n%s", in, out)
	}
}

// TestNpmNonSuccessFetchLinesKept: http fetch traces are noise only when
// they SUCCEEDED — a 401/404/5xx fetch line names the failing registry,
// status and latency, which is exactly the diagnostic the error block may
// not repeat.
func TestNpmNonSuccessFetchLinesKept(t *testing.T) {
	in := "npm http fetch GET 200 https://registry.npmjs.org/ok-pkg 12ms (cache miss)\n" +
		"npm http fetch GET 401 https://registry.internal/priv-pkg 88ms\n" +
		"npm http fetch GET 404 https://registry.npmjs.org/missing-pkg 30ms\n" +
		"npm error code E404\n"
	out, err := (&Npm{}).Compress(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "GET 401 https://registry.internal/priv-pkg") ||
		!strings.Contains(out, "GET 404 https://registry.npmjs.org/missing-pkg") {
		t.Errorf("non-success fetch lines must survive; got:\n%s", out)
	}
	if strings.Contains(out, "GET 200") {
		t.Errorf("successful fetch lines fold; got:\n%s", out)
	}
	if !strings.Contains(out, "[1 http fetch lines (collapsed)]") {
		t.Errorf("fold marker counts only the successful traces; got:\n%s", out)
	}
}
