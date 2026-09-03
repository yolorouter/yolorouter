// Package providerclienttest provides the shared test double for
// providerclient.ProviderClient. The provider and model service tests both
// drive their subjects through this one Fake, so the canned-response
// vocabulary (per-target, per-probe-kind, side effects, call bookkeeping)
// exists once instead of one drifting copy per test suite.
package providerclienttest

import (
	"context"
	"sync"

	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/service/providerclient"
)

// Fake is a canned providerclient.ProviderClient. Fields are set directly
// by tests before use; the bookkeeping fields (Calls, LastModel, LastProto)
// are read directly after the exercised code returns. Reads and writes from
// the test goroutine are not synchronized with in-flight probes — set up
// before, assert after, exactly like the tests always have.
type Fake struct {
	// mu guards the mutable bookkeeping below. The candidate probe pipeline
	// runs the streaming and function-calling probes concurrently, so two of
	// these methods can be in flight at once.
	mu        sync.Mutex
	Result    providerclient.TestResult
	Err       error
	Calls     int
	LastModel string
	LastProto protocols.ProtocolID
	// SideEffect runs on every probe, outside the lock — side effects reach
	// into the database, and a probe holding the mutex while doing so would
	// serialize the concurrent capability probes the pipeline deliberately
	// overlaps.
	SideEffect func()
	// PerTarget canned responses keyed by "<proto>|<baseURL>".
	PerTarget map[string]TargetResponse
	// PerTestType canned responses keyed by probe kind — "basic" /
	// "streaming" / "function_calling" / "image" / "video" — for tests
	// that need the probes to disagree (the key-verification fallbacks
	// read "basic" against "video"/"image"). Falls back to Result/Err when
	// a probe has no entry.
	PerTestType map[string]TargetResponse
	// callsByTestType counts invocations per probe kind, letting a test
	// assert that a probe was skipped rather than merely that it failed.
	callsByTestType map[string]int
	// Models is returned verbatim by ListModels; Result.Outcome/Detail drive
	// its outcome, so a test can pin both a catalogue and a failure shape.
	Models []string
}

// TargetResponse is one canned (TestResult, error) pair for PerTarget /
// PerTestType entries.
type TargetResponse struct {
	Result providerclient.TestResult
	Err    error
}

// record does the bookkeeping every probe method shares and returns the
// canned response for that probe kind.
func (f *Fake) record(testType string, proto protocols.ProtocolID, baseURL, model string) (providerclient.TestResult, error) {
	f.mu.Lock()
	f.Calls++
	f.LastModel = model
	f.LastProto = proto
	if f.callsByTestType == nil {
		f.callsByTestType = map[string]int{}
	}
	f.callsByTestType[testType]++
	sideEffect := f.SideEffect
	resp, ok := f.PerTestType[testType]
	f.mu.Unlock()

	if sideEffect != nil {
		sideEffect()
	}
	if ok {
		return resp.Result, resp.Err
	}
	return f.responseFor(proto, baseURL)
}

// CallCountFor reports how many times the given probe kind ("basic" /
// "streaming" / "function_calling" / "image" / "video") was invoked.
func (f *Fake) CallCountFor(testType string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callsByTestType[testType]
}

func (f *Fake) responseFor(proto protocols.ProtocolID, baseURL string) (providerclient.TestResult, error) {
	if f.PerTarget != nil {
		if resp, ok := f.PerTarget[string(proto)+"|"+baseURL]; ok {
			return resp.Result, resp.Err
		}
	}
	return f.Result, f.Err
}

func (f *Fake) ListModels(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey string) (providerclient.ListModelsResult, error) {
	f.mu.Lock()
	f.Calls++
	f.LastProto = proto
	res := providerclient.ListModelsResult{Models: f.Models, Outcome: f.Result.Outcome, DurationMs: f.Result.DurationMs, Detail: f.Result.Detail}
	err := f.Err
	f.mu.Unlock()
	return res, err
}

func (f *Fake) TestChatCompletion(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	return f.record("basic", proto, baseURL, model)
}

func (f *Fake) TestImageGeneration(ctx context.Context, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	return f.record("image", protocols.ProtocolOpenAI, baseURL, model)
}

func (f *Fake) TestVideoGeneration(ctx context.Context, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	return f.record("video", protocols.ProtocolOpenAI, baseURL, model)
}

func (f *Fake) TestStreamingCompletion(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	return f.record("streaming", proto, baseURL, model)
}

func (f *Fake) TestFunctionCalling(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (providerclient.TestResult, error) {
	return f.record("function_calling", proto, baseURL, model)
}
