package maxtokens

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/protocols"
)

type stubView struct{ limit int }

func (v stubView) CandidateMaxOutput() int { return v.limit }

type recordingSink struct{ records []fact.Record }

func (s *recordingSink) Report(...fact.Fact)   {}
func (s *recordingSink) Note(r ...fact.Record) { s.records = append(s.records, r...) }
func (s *recordingSink) clamped() (fact.MaxTokensClamped, bool) {
	for _, r := range s.records {
		if rec, ok := r.(fact.MaxTokensClamped); ok {
			return rec, true
		}
	}
	return fact.MaxTokensClamped{}, false
}

func run(t *testing.T, limit int, proto protocols.ProtocolID, body string) ([]byte, *recordingSink) {
	t.Helper()
	sink := &recordingSink{}
	out, err := New().RewriteEgress(context.Background(), stubView{limit: limit}, proto, []byte(body), sink)
	if err != nil {
		t.Fatalf("RewriteEgress errored, but a ceiling this capability cannot apply must never stop the request: %v", err)
	}
	return out, sink
}

// ceilingIn digs the output ceiling back out of an encoded request, so a test
// asserts on the number the upstream would read rather than on a substring.
func ceilingIn(t *testing.T, proto protocols.ProtocolID, body []byte) (int, bool) {
	t.Helper()
	paths := ceilingFields(proto)
	if len(paths) == 0 {
		t.Fatalf("no ceiling field known for %s", proto)
	}
	field, nested := paths[0].field, paths[0].nested
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	container := doc
	if nested != "" {
		inner := map[string]json.RawMessage{}
		if err := json.Unmarshal(doc[nested], &inner); err != nil {
			t.Fatalf("nested object is not JSON: %v", err)
		}
		container = inner
	}
	raw, ok := container[field]
	if !ok {
		return 0, false
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		t.Fatalf("ceiling is not a number: %v", err)
	}
	return n, true
}

// Every protocol spells the ceiling differently, and each spelling has to be
// found — a clamp that reaches only some of them lets the rest through at
// whatever the caller asked for.
func TestClampsAnAskAboveTheCandidateLimitInEveryProtocol(t *testing.T) {
	for _, tc := range []struct {
		name  string
		proto protocols.ProtocolID
		body  string
	}{
		{"openai", protocols.ProtocolOpenAI, `{"model":"m","max_tokens":9000,"messages":[]}`},
		{"claude", protocols.ProtocolClaude, `{"model":"m","max_tokens":9000,"messages":[]}`},
		{"responses", protocols.ProtocolResponses, `{"model":"m","max_output_tokens":9000,"input":[]}`},
		{"gemini", protocols.ProtocolGemini, `{"generationConfig":{"maxOutputTokens":9000},"contents":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, sink := run(t, 4096, tc.proto, tc.body)

			got, ok := ceilingIn(t, tc.proto, out)
			if !ok {
				t.Fatal("the ceiling field went missing from the rewritten body")
			}
			if got != 4096 {
				t.Fatalf("ceiling = %d, want the candidate's 4096: the request would be sent asking for more than the provider allows", got)
			}
			rec, reported := sink.clamped()
			if !reported {
				t.Fatal("the clamp was not reported, so nothing in the audit row explains a truncated answer")
			}
			if rec.Asked != 9000 || rec.Allowed != 4096 {
				t.Errorf("reported %+v, want asked 9000 allowed 4096", rec)
			}
		})
	}
}

// The rest of the request has to survive the edit. Re-encoding JSON is exactly
// where a field quietly goes missing.
func TestClampingLeavesTheRestOfTheRequestIntact(t *testing.T) {
	out, _ := run(t, 100, protocols.ProtocolOpenAI,
		`{"model":"m","max_tokens":9000,"temperature":0.5,"messages":[{"role":"user","content":"hi"}]}`)

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	for _, key := range []string{"model", "temperature", "messages"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("%q was dropped by the rewrite", key)
		}
	}
}

// A ceiling under the candidate's is the caller wanting less, which is theirs
// to want. Raising it would turn a modest request into a demand for the
// maximum, and upstreams that reserve by the stated ceiling charge for that.
func TestAnAskBelowTheLimitIsNeverRaised(t *testing.T) {
	out, sink := run(t, 4096, protocols.ProtocolOpenAI, `{"model":"m","max_tokens":16,"messages":[]}`)

	got, _ := ceilingIn(t, protocols.ProtocolOpenAI, out)
	if got != 16 {
		t.Fatalf("ceiling = %d, want the caller's 16 left alone", got)
	}
	if _, reported := sink.clamped(); reported {
		t.Error("a request that was never clamped reported a clamp")
	}
}

// A request that states no ceiling is not the same as one asking for zero, and
// must not have one written in for it.
func TestARequestWithNoCeilingIsLeftAlone(t *testing.T) {
	const body = `{"model":"m","messages":[]}`
	out, sink := run(t, 4096, protocols.ProtocolOpenAI, body)

	if string(out) != body {
		t.Fatalf("body was rewritten to %s, want it untouched", out)
	}
	if _, reported := sink.clamped(); reported {
		t.Error("a request with no ceiling reported a clamp")
	}
}

// A candidate that states no limit has not stated a limit of zero. Reading it
// that way would clamp every request to nothing.
func TestACandidateWithNoStatedLimitClampsNothing(t *testing.T) {
	const body = `{"model":"m","max_tokens":9000,"messages":[]}`
	out, sink := run(t, 0, protocols.ProtocolOpenAI, body)

	if string(out) != body {
		t.Fatalf("body was rewritten to %s, want it untouched", out)
	}
	if _, reported := sink.clamped(); reported {
		t.Error("a candidate with no limit reported a clamp")
	}
}

// Fail-open, on both shapes of unusable input: the request goes upstream as the
// caller wrote it, which is what happened before this capability existed.
func TestAnUnusableBodyIsPassedThroughUntouched(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"not json", `{"model":"m","max_tokens":`},
		{"ceiling is not a number", `{"model":"m","max_tokens":"lots"}`},
		{"gemini without its config object", `{"contents":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proto := protocols.ProtocolOpenAI
			if tc.name == "gemini without its config object" {
				proto = protocols.ProtocolGemini
			}
			out, sink := run(t, 100, proto, tc.body)

			if string(out) != tc.body {
				t.Fatalf("body was rewritten to %s, want it untouched", out)
			}
			if _, reported := sink.clamped(); reported {
				t.Error("an untouched body reported a clamp")
			}
		})
	}
}

// OpenAI has two live spellings of the ceiling. The reasoning models take
// max_completion_tokens, and current SDKs send it — so reading only the older
// name lets exactly the newest and priciest requests through unclamped.
func TestClampsBothOpenAISpellingsOfTheCeiling(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"only the modern name", `{"model":"m","max_completion_tokens":65536,"messages":[]}`},
		{"only the legacy name", `{"model":"m","max_tokens":65536,"messages":[]}`},
		{"both present", `{"model":"m","max_tokens":65536,"max_completion_tokens":65536,"messages":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, sink := run(t, 4096, protocols.ProtocolOpenAI, tc.body)

			var doc map[string]json.RawMessage
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatalf("result is not JSON: %v", err)
			}
			// Whichever names the request carried must all come back clamped:
			// which one the upstream honours is its choice, not ours.
			for _, field := range []string{"max_tokens", "max_completion_tokens"} {
				raw, present := doc[field]
				if !present {
					continue
				}
				var n int
				if err := json.Unmarshal(raw, &n); err != nil {
					t.Fatalf("%s is not a number: %v", field, err)
				}
				if n != 4096 {
					t.Errorf("%s = %d, want the candidate's 4096", field, n)
				}
			}
			if _, reported := sink.clamped(); !reported {
				t.Error("the clamp was not reported")
			}
		})
	}
}

// Anthropic requires the thinking budget to sit strictly below max_tokens.
// Lowering the ceiling under a budget that was legal on the way in turns a
// servable request into an upstream 400 — worse than not clamping at all.
func TestLoweringTheCeilingBringsTheThinkingBudgetWithIt(t *testing.T) {
	out, _ := run(t, 4096, protocols.ProtocolClaude,
		`{"model":"m","max_tokens":9000,"thinking":{"type":"enabled","budget_tokens":8000},"messages":[]}`)

	var doc struct {
		MaxTokens int `json:"max_tokens"`
		Thinking  struct {
			Type   string `json:"type"`
			Budget int    `json:"budget_tokens"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	if doc.MaxTokens != 4096 {
		t.Fatalf("max_tokens = %d, want 4096", doc.MaxTokens)
	}
	if doc.Thinking.Budget >= doc.MaxTokens {
		t.Fatalf("thinking budget %d is not below max_tokens %d: the upstream rejects this request outright",
			doc.Thinking.Budget, doc.MaxTokens)
	}
	if doc.Thinking.Type != "enabled" {
		t.Errorf("thinking.type = %q, want the caller's own setting preserved", doc.Thinking.Type)
	}
}

// A budget that already sits below the new ceiling is the caller's choice and
// stays exactly as they wrote it — including a budget below what Anthropic
// considers usable. The floor exists to keep a budget this capability had to
// LOWER from becoming pointless; applying it to a budget nobody touched would
// raise what the caller asked for, which this capability never does in either
// direction.
func TestAThinkingBudgetAlreadyUnderTheCeilingIsUntouched(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget int
	}{
		{"comfortably under", 2000},
		{"under the usable floor", 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"model":"m","max_tokens":9000,"thinking":{"type":"enabled","budget_tokens":%d},"messages":[]}`, tc.budget)
			out, _ := run(t, 4096, protocols.ProtocolClaude, body)

			var doc struct {
				Thinking struct {
					Budget int `json:"budget_tokens"`
				} `json:"thinking"`
			}
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatalf("result is not JSON: %v", err)
			}
			if doc.Thinking.Budget != tc.budget {
				t.Fatalf("thinking budget = %d, want the caller's %d left alone", doc.Thinking.Budget, tc.budget)
			}
		})
	}
}

// Anthropic's manual thinking has two constraints, and a candidate ceiling at
// or under the usable-budget floor leaves no number satisfying both. Clamping
// anyway produces a request the upstream must reject — and a 400 is read as the
// caller's own mistake, ending the exchange without trying a candidate that had
// room. Sending it over-large is the lesser harm, and it is reported so an
// operator can see why a response exceeded the ceiling.
func TestARequestThatCannotBeHeldDownIsSentAsWrittenAndReported(t *testing.T) {
	for _, limit := range []int{1, 512, 1024} {
		t.Run(fmt.Sprintf("ceiling %d", limit), func(t *testing.T) {
			const body = `{"model":"m","max_tokens":9000,"thinking":{"type":"enabled","budget_tokens":8000},"messages":[]}`
			out, sink := run(t, limit, protocols.ProtocolClaude, body)

			if string(out) != body {
				t.Fatalf("the request was rewritten into something the upstream rejects: %s", out)
			}
			var skipped fact.MaxTokensClampSkipped
			var found bool
			for _, r := range sink.records {
				if rec, ok := r.(fact.MaxTokensClampSkipped); ok {
					skipped, found = rec, true
				}
			}
			if !found {
				t.Fatal("nothing was reported, so an over-ceiling response has no explanation on the row")
			}
			// The reported figure is the ceiling the caller stated, the same
			// thing MaxTokensClamped.Asked means — not the thinking budget that
			// happened to make this one impossible.
			if skipped.Asked != 9000 || skipped.Allowed != limit {
				t.Errorf("reported %+v, want asked 9000 allowed %d", skipped, limit)
			}
			if _, clamped := sink.clamped(); clamped {
				t.Error("a request that was left alone also reported a clamp")
			}
		})
	}
}

// One above the floor is the first ceiling that CAN be satisfied, and it must
// be: the decline is for the impossible case only, not a licence to give up
// near it.
func TestTheFirstSatisfiableCeilingIsStillClamped(t *testing.T) {
	out, sink := run(t, minUsableThinkingBudget+1, protocols.ProtocolClaude,
		`{"model":"m","max_tokens":9000,"thinking":{"type":"enabled","budget_tokens":8000},"messages":[]}`)

	var doc struct {
		MaxTokens int `json:"max_tokens"`
		Thinking  struct {
			Budget int `json:"budget_tokens"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	if doc.MaxTokens != minUsableThinkingBudget+1 {
		t.Fatalf("max_tokens = %d, want %d", doc.MaxTokens, minUsableThinkingBudget+1)
	}
	if doc.Thinking.Budget < minUsableThinkingBudget || doc.Thinking.Budget >= doc.MaxTokens {
		t.Fatalf("budget %d must be at or above %d and below max_tokens %d — both constraints hold at this ceiling",
			doc.Thinking.Budget, minUsableThinkingBudget, doc.MaxTokens)
	}
	if _, clamped := sink.clamped(); !clamped {
		t.Error("a satisfiable ceiling was not clamped")
	}
}

// A low ceiling with no thinking at all is ordinary: nothing makes it
// impossible, so it clamps like any other request.
func TestALowCeilingWithoutThinkingStillClamps(t *testing.T) {
	out, sink := run(t, 512, protocols.ProtocolClaude, `{"model":"m","max_tokens":9000,"messages":[]}`)

	got, _ := ceilingIn(t, protocols.ProtocolClaude, out)
	if got != 512 {
		t.Fatalf("max_tokens = %d, want 512", got)
	}
	if _, clamped := sink.clamped(); !clamped {
		t.Error("the clamp was not reported")
	}
}

// "Cannot be held down" only applies to a request that needed holding down. A
// Claude request whose ceiling already fits has nothing to decline, and
// reporting a skip for it would put an unexplained line on a row where nothing
// went wrong.
func TestNothingIsReportedWhenTheRequestWasNeverOverTheCeiling(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"ceiling already fits", `{"model":"m","max_tokens":100,"thinking":{"type":"enabled","budget_tokens":8000},"messages":[]}`},
		{"no ceiling stated", `{"model":"m","thinking":{"type":"enabled","budget_tokens":8000},"messages":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, sink := run(t, 512, protocols.ProtocolClaude, tc.body)

			if string(out) != tc.body {
				t.Fatalf("body was rewritten to %s, want it untouched", out)
			}
			if len(sink.records) != 0 {
				t.Fatalf("reported %d records for a request that was never over the ceiling, want none: %+v", len(sink.records), sink.records)
			}
		})
	}
}

// A ceiling is a number, not an integer literal. Clients whose arithmetic is
// floating point send 9000.0 or 9e3, and reading only 9000 treats those as no
// ceiling at all — which for a cap means the request goes out uncapped.
func TestEveryJSONSpellingOfANumberIsACeiling(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  int
	}{
		{"integer literal", "9000", 4096},
		{"trailing zero", "9000.0", 4096},
		{"exponent", "9e3", 4096},
		{"beyond any int", "1e30", 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","max_tokens":` + tc.value + `,"messages":[]}`
			out, sink := run(t, 4096, protocols.ProtocolOpenAI, body)

			got, ok := ceilingIn(t, protocols.ProtocolOpenAI, out)
			if !ok {
				t.Fatal("the ceiling field went missing")
			}
			if got != tc.want {
				t.Fatalf("ceiling = %d, want %d: written as %s it went upstream uncapped", got, tc.want, tc.value)
			}
			if _, reported := sink.clamped(); !reported {
				t.Error("the clamp was not reported")
			}
		})
	}
}

// A fractional ceiling truncates toward zero rather than rounding up: asking
// for at most 4095.9 tokens is asking for at most 4095 whole ones, and rounding
// up would have this code request more than the caller did.
func TestAFractionalCeilingTruncatesRatherThanRounding(t *testing.T) {
	out, sink := run(t, 4096, protocols.ProtocolOpenAI, `{"model":"m","max_tokens":4095.9,"messages":[]}`)

	if _, reported := sink.clamped(); reported {
		t.Fatal("4095.9 truncates to 4095, which is under the ceiling — nothing to clamp")
	}
	if !bytes.Contains(out, []byte("4095.9")) {
		t.Fatalf("the caller's own value was rewritten: %s", out)
	}
}

// Still not a number: a string or a boolean is not a ceiling stated oddly, it
// is a malformed request, and this capability leaves those to the upstream.
func TestSomethingThatIsNotANumberIsStillNotACeiling(t *testing.T) {
	for _, value := range []string{`"lots"`, `true`, `null`, `[9000]`} {
		t.Run(value, func(t *testing.T) {
			body := `{"model":"m","max_tokens":` + value + `,"messages":[]}`
			out, sink := run(t, 4096, protocols.ProtocolOpenAI, body)

			if string(out) != body {
				t.Fatalf("body was rewritten to %s, want it untouched", out)
			}
			if len(sink.records) != 0 {
				t.Errorf("reported %+v for a request with no ceiling", sink.records)
			}
		})
	}
}

// countingView records how often the candidate's limit was asked for. What that
// answer costs is the view's business, not this capability's — a deployment may
// have to reach a database for it — so a request that states no ceiling, and
// therefore cannot be clamped whatever the limit is, must not ask at all.
type countingView struct {
	limit int
	asked int
}

func (v *countingView) CandidateMaxOutput() int {
	v.asked++
	return v.limit
}

func TestABodyWithNoCeilingNeverAsksForTheLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"no ceiling stated", `{"model":"m","messages":[]}`},
		{"unparseable", `{"model":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := &countingView{limit: 4096}

			out, err := New().RewriteEgress(context.Background(), view, protocols.ProtocolOpenAI, []byte(tc.body), &recordingSink{})
			if err != nil {
				t.Fatalf("RewriteEgress: %v", err)
			}
			if string(out) != tc.body {
				t.Fatalf("body = %s, want it untouched", out)
			}
			if view.asked != 0 {
				t.Fatalf("the limit was asked for %d times: a request that cannot be clamped is paying for an answer that changes nothing", view.asked)
			}
		})
	}
}

// The mirror: a request that does state a ceiling must ask, exactly once.
func TestABodyWithACeilingAsksOnce(t *testing.T) {
	view := &countingView{limit: 4096}

	if _, err := New().RewriteEgress(context.Background(), view, protocols.ProtocolOpenAI,
		[]byte(`{"model":"m","max_tokens":9000,"messages":[]}`), &recordingSink{}); err != nil {
		t.Fatalf("RewriteEgress: %v", err)
	}
	if view.asked != 1 {
		t.Fatalf("the limit was asked for %d times, want exactly 1", view.asked)
	}
}

// A fraction just above the limit is above the limit. Truncating it first reads
// 4096.5 as 4096, counts it as within a 4096 ceiling, and forwards the original
// 4096.5 — which the upstream, expecting an integer, rejects. That matters most
// on the same-protocol path, where nothing re-encodes the body on the way out.
func TestAFractionJustAboveTheLimitIsClamped(t *testing.T) {
	view := &countingView{limit: 4096}
	body := []byte(`{"model":"m","max_tokens":4096.5,"messages":[]}`)

	out, err := New().RewriteEgress(context.Background(), view, protocols.ProtocolOpenAI, body, &recordingSink{})
	if err != nil {
		t.Fatalf("RewriteEgress: %v", err)
	}
	if !bytes.Contains(out, []byte(`"max_tokens":4096`)) || bytes.Contains(out, []byte("4096.5")) {
		t.Fatalf("body = %s, want the ceiling brought down to 4096", out)
	}
}

// The boundary on the other side: a fraction below the limit states less than
// the limit and must be left exactly as written.
func TestAFractionBelowTheLimitIsLeftAlone(t *testing.T) {
	view := &countingView{limit: 4096}
	body := []byte(`{"model":"m","max_tokens":4095.5,"messages":[]}`)

	out, err := New().RewriteEgress(context.Background(), view, protocols.ProtocolOpenAI, body, &recordingSink{})
	if err != nil {
		t.Fatalf("RewriteEgress: %v", err)
	}
	if !bytes.Equal(out, body) {
		t.Fatalf("body = %s, want it untouched", out)
	}
}
