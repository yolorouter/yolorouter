package compress

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/compress/compressors"
	"github.com/yolorouter/yolorouter/internal/protocols"
)

// panicCompressor is a test double that always panics inside Compress. It is
// used to exercise runCompress's defer-recover fail-open path: a panicking
// leaf compressor must never escape the pass — the original body is returned
// untouched with SkipReasonFailOpen.
type panicCompressor struct{}

func (*panicCompressor) Name() string { return "panic" }
func (*panicCompressor) Compress(context.Context, string) (string, error) {
	panic("intentional panic from panicCompressor")
}

func TestCompressClaudeShrinksToolResultKeepsRest(t *testing.T) {
	big := "=== RUN   TestA\n--- PASS: TestA (0.00s)\n" + strings.Repeat("=== RUN   TestX\n--- PASS: TestX (0.00s)\n", 200) + "PASS\nok  \tpkg\t0.1s\n"
	body := []byte(`{"model":"claude","system":"SYS","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":` + mustJSONStr(big) + `}]}]}`)
	out, res := ByProtocol(protocols.ProtocolClaude, context.Background(), body, DefaultOptions())
	if res.Skipped {
		t.Fatalf("expected compression, got skip=%v", res.SkipReason)
	}
	if len(out) >= len(body) {
		t.Fatal("output should be shorter than input")
	}
	// The frozen prefix (everything outside the live zone) must be preserved
	// byte-for-byte; only the live-zone text is rewritten.
	if !bytes.Contains(out, []byte(`"system":"SYS"`)) {
		t.Fatal("system prefix must be preserved byte-for-byte")
	}
	if res.EstimatedTokensSaved <= 0 {
		t.Fatal("expected positive token savings")
	}
}

func TestCompressClaudeNoLiveZoneIsIdentity(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`) // no tool_result
	out, res := ByProtocol(protocols.ProtocolClaude, context.Background(), body, DefaultOptions())
	if !bytes.Equal(out, body) {
		t.Fatal("no-op must return bytes.Equal output")
	}
	if !res.Skipped {
		t.Fatal("expected Skipped=true")
	}
}

func TestCompressClaudeParseErrorIsIdentity(t *testing.T) {
	body := []byte(`{not json`)
	out, res := ByProtocol(protocols.ProtocolClaude, context.Background(), body, DefaultOptions())
	if !bytes.Equal(out, body) || res.SkipReason != SkipReasonParseError {
		t.Fatal("invalid JSON must be returned verbatim with ParseError")
	}
}

func TestCompressSkipReasonNoLiveZone(t *testing.T) {
	// Claude live zone collects only tool_result blocks; a user message with
	// bare-string content has none, so locate returns an empty slice and the
	// true skip reason is NoLiveZone.
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	_, res := ByProtocol(protocols.ProtocolClaude, context.Background(), body, DefaultOptions())
	if !res.Skipped || res.SkipReason != SkipReasonNoLiveZone {
		t.Fatalf("expected NoLiveZone, got skip=%v reason=%v", res.Skipped, res.SkipReason)
	}
}

func TestCompressSkipReasonNoMatchingCompressor(t *testing.T) {
	// A large prose block (no diff/gotest/grep/log anchors) is detected as
	// PlainText, for which compressorsFor returns nil, so the pass surfaces
	// a real coverage gap as NoMatchingCompressor.
	prose := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 100)
	body := []byte(`{"model":"claude","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":` + mustJSONStr(prose) + `}]}]}`)
	_, res := ByProtocol(protocols.ProtocolClaude, context.Background(), body, DefaultOptions())
	if !res.Skipped || res.SkipReason != SkipReasonNoMatchingCompressor {
		t.Fatalf("expected NoMatchingCompressor, got skip=%v reason=%v", res.Skipped, res.SkipReason)
	}
}

func TestCompressSkipReasonNoEffectiveReplacement(t *testing.T) {
	// A block smaller than MinBlockBytes (512) is skipped by shouldAttempt
	// before content-type detection runs, so sawNoCompressor stays false and
	// the skip reason resolves to NoEffectiveReplacement rather than
	// NoMatchingCompressor.
	tiny := "=== RUN   TestA\n--- PASS: TestA (0.00s)\nPASS\n"
	body := []byte(`{"model":"claude","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":` + mustJSONStr(tiny) + `}]}]}`)
	_, res := ByProtocol(protocols.ProtocolClaude, context.Background(), body, DefaultOptions())
	if !res.Skipped || res.SkipReason != SkipReasonNoEffectiveReplacement {
		t.Fatalf("expected NoEffectiveReplacement, got skip=%v reason=%v", res.Skipped, res.SkipReason)
	}
}

func TestCompressSkipReasonFailOpenOnPanic(t *testing.T) {
	// Swap the package-level build-compressor chain for a single panicking
	// compressor. runCompress's defer-recover must catch the panic and return
	// the ORIGINAL body untouched with SkipReasonFailOpen. This test is serial:
	// it mutates package state (buildCompressors) and must not run in parallel
	// with any other test that reads the same chain.
	saved := buildCompressors
	buildCompressors = []compressors.Compressor{&panicCompressor{}}
	t.Cleanup(func() { buildCompressors = saved })

	// Build-output anchors (=== RUN, --- PASS, ok) cause detectContentType to
	// return ContentBuildOutput, which routes to buildCompressors.
	big := "=== RUN   TestA\n--- PASS: TestA (0.00s)\n" +
		strings.Repeat("=== RUN   TestX\n--- PASS: TestX (0.00s)\n", 200) +
		"PASS\nok  \tpkg\t0.1s\n"
	body := []byte(`{"messages":[{"role":"user","content":` + mustJSONStr(big) + `}]}`)

	out, res := ByProtocol(protocols.ProtocolOpenAI, context.Background(), body, DefaultOptions())
	if !bytes.Equal(out, body) {
		t.Fatal("panic must fail open: returned body must be bytes.Equal to original")
	}
	if !res.Skipped || res.SkipReason != SkipReasonFailOpen {
		t.Fatalf("expected Skipped=true SkipReason=FailOpen, got skip=%v reason=%v", res.Skipped, res.SkipReason)
	}
}

// slowCompressor burns a fixed amount of real wall time, then hands the content
// back unchanged. Wall time is what makes the timeout tests below deterministic:
// they never depend on a sub-tick deadline being observable, only on more time
// having genuinely passed than the budget allowed.
type slowCompressor struct{ delay time.Duration }

func (c *slowCompressor) Name() string { return "slow" }

func (c *slowCompressor) Compress(_ context.Context, content string) (string, error) {
	time.Sleep(c.delay)
	return content, nil
}

// withSlowBuildCompressor swaps the build-output chain for one that stalls, and
// restores it afterwards.
func withSlowBuildCompressor(t *testing.T, delay time.Duration) {
	t.Helper()
	original := buildCompressors
	buildCompressors = []compressors.Compressor{&slowCompressor{delay: delay}}
	t.Cleanup(func() { buildCompressors = original })
}

// twoBlockBody builds a request with two live blocks, so the per-block timeout
// guard is reached a second time after the first block has been worked on.
func twoBlockBody(t *testing.T) []byte {
	t.Helper()
	big := "=== RUN   TestA\n--- PASS: TestA (0.00s)\n" +
		strings.Repeat("=== RUN   TestX\n--- PASS: TestX (0.00s)\n", 200) +
		"PASS\nok  \tpkg\t0.1s\n"
	return []byte(`{"messages":[` +
		`{"role":"user","content":` + mustJSONStr(big) + `},` +
		`{"role":"user","content":` + mustJSONStr(big) + `}]}`)
}

// The budget configured through CompressOptions.Timeout must stop the work.
// The first block's compressor sleeps well past that budget, so by the time the
// guard at the head of the second block runs, the deadline has genuinely
// elapsed — no reliance on a 1ns deadline being observable, which is what made
// the original form fail on Windows, where the clock granularity is coarse
// enough that time.Now() and the time.Until() inside context.WithDeadline can
// land on the same tick and leave the deadline in the future.
func TestCompressOptionsTimeoutStopsTheWork(t *testing.T) {
	withSlowBuildCompressor(t, 200*time.Millisecond)
	body := twoBlockBody(t)

	opts := DefaultOptions()
	opts.Timeout = 20 * time.Millisecond

	out, res := ByProtocol(protocols.ProtocolOpenAI, context.Background(), body, opts)
	if !bytes.Equal(out, body) {
		t.Fatal("timeout must return original body untouched")
	}
	if !res.Skipped || res.SkipReason != SkipReasonTimeout {
		t.Fatalf("expected Skipped=true SkipReason=Timeout, got skip=%v reason=%v", res.Skipped, res.SkipReason)
	}
}

// The same guard, reached through the caller's own context instead of the
// option. An already-elapsed deadline is deterministic everywhere:
// context.WithDeadline cancels immediately, without arming a timer, when the
// deadline has already passed.
func TestCompressSkipReasonTimeout(t *testing.T) {
	body := twoBlockBody(t)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	out, res := ByProtocol(protocols.ProtocolOpenAI, ctx, body, DefaultOptions())
	if !bytes.Equal(out, body) {
		t.Fatal("timeout must return original body untouched")
	}
	if !res.Skipped || res.SkipReason != SkipReasonTimeout {
		t.Fatalf("expected Skipped=true SkipReason=Timeout, got skip=%v reason=%v", res.Skipped, res.SkipReason)
	}
}

func mustJSONStr(s string) string { b, _ := jsonMarshal(s); return string(b) }

// TestCompressCanceledContextReportsTimeout: a context that dies mid-pass
// must surface as the timeout skip, with the body untouched — classifying it
// as "nothing shrank" hides real deadline pressure from the operator.
func TestCompressCanceledContextReportsTimeout(t *testing.T) {
	big := "=== RUN   TestA\n--- PASS: TestA (0.00s)\n" + strings.Repeat("=== RUN   TestX\n--- PASS: TestX (0.00s)\n", 200) + "PASS\nok  \tpkg\t0.1s\n"
	body := []byte(`{"model":"claude","system":"SYS","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":` + mustJSONStr(big) + `}]}]}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, res := ByProtocol(protocols.ProtocolClaude, ctx, body, DefaultOptions())
	if !res.Skipped || res.SkipReason != SkipReasonTimeout {
		t.Fatalf("skip = %v / %q, want timeout", res.Skipped, res.SkipReason)
	}
	if !bytes.Equal(out, body) {
		t.Fatal("a timed-out pass must return the body untouched")
	}
}

// cancelingCompressor cancels the pass from INSIDE a Compress call — the
// deterministic stand-in for a deadline expiring mid-pass — and returns the
// context's error like a well-behaved compressor.
type cancelingCompressor struct{ cancel context.CancelFunc }

func (*cancelingCompressor) Name() string { return "canceling" }
func (c *cancelingCompressor) Compress(ctx context.Context, _ string) (string, error) {
	c.cancel()
	return "", ctx.Err()
}

// TestCompressMidPassCancelFailsOpenWhole: a cancellation that lands while a
// LATER block is being compressed must throw away the earlier block's
// replacement too — the contract is original body + timeout, never a
// partially-rewritten body reported as success.
func TestCompressMidPassCancelFailsOpenWhole(t *testing.T) {
	shrinkable := "=== RUN   TestA\n--- PASS: TestA (0.00s)\n" + strings.Repeat("=== RUN   TestX\n--- PASS: TestX (0.00s)\n", 120) + "PASS\nok  \tpkg\t0.1s\n"
	other := strings.Repeat("npm warn deprecated pkg@1.0.0: unsupported\n", 120) + "added 3 packages in 1s\n"
	body := []byte(`{"model":"claude","system":"SYS","messages":[{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"t1","content":` + mustJSONStr(shrinkable) + `},` +
		`{"type":"tool_result","tool_use_id":"t2","content":` + mustJSONStr(other) + `}]}]}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Chain: the real gotest compressor first (block 1 shrinks), then the
	// canceler (fires on block 2, where gotest declines the npm content).
	saved := buildCompressors
	buildCompressors = []compressors.Compressor{&compressors.GoTest{}, &cancelingCompressor{cancel: cancel}}
	defer func() { buildCompressors = saved }()

	out, res := ByProtocol(protocols.ProtocolClaude, ctx, body, DefaultOptions())
	if !res.Skipped || res.SkipReason != SkipReasonTimeout {
		t.Fatalf("skip = %v / %q, want timeout", res.Skipped, res.SkipReason)
	}
	if !bytes.Equal(out, body) {
		t.Fatal("a mid-pass cancel must return the ORIGINAL body — no partial splice")
	}
}
