// Package maxtokens holds a request's output ceiling down to what the
// candidate about to serve it will actually honour.
//
// A caller asking for more output than a candidate supports is not refused —
// the request is good, the number is simply larger than this provider allows.
// Upstreams disagree about what to do with it: some clamp silently, some reject
// the request outright, and some accept it and bill for a generation the
// operator never budgeted for. Clamping here makes the answer the same
// everywhere, and makes it the operator's configured limit rather than the
// provider's mood.
//
// It is an egress rewriter because the ceiling belongs to the candidate, not to
// the exchange: failover moves the request to a provider with its own limit,
// and the body has to be re-clamped for it. A request rewritten once, before
// the candidate was known, would carry the wrong ceiling everywhere after the
// first hop.
package maxtokens

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/protocols"
)

// minUsableThinkingBudget is Anthropic's floor for a thinking budget that does
// anything at all. The number is taken from the Claude request normalisation in
// this repository, but only the number: where a ceiling cannot accommodate the
// floor, that code drops thinking and keeps the ceiling, while this capability
// keeps thinking and gives up the ceiling. The two answers point opposite ways
// and the difference is not a mirror of anything — it comes from position.
// Dropping thinking means restoring the sampling parameters that were forced
// when it was written, and those original values are gone by the time a body
// has been encoded, which is where this capability runs.
const minUsableThinkingBudget = 1024

// View is what this capability needs of the exchange: the ceiling of whichever
// candidate is being tried right now.
type View interface {
	CandidateMaxOutput() int
}

// Clamp is the capability. It holds no per-request state.
type Clamp struct{}

// New builds the capability.
func New() *Clamp { return &Clamp{} }

func (*Clamp) Name() string { return "max_tokens_clamp" }

// RewriteEgress lowers the request's output ceiling to the candidate's when the
// caller asked for more, and leaves the body alone otherwise.
//
// Raising is never done. A caller who asked for less wants less, and a ceiling
// is a limit rather than an instruction; writing the candidate's number into a
// request that never carried one would turn "no preference" into a demand for
// the maximum, which upstreams that reserve capacity by the stated ceiling
// charge for.
//
// A body it cannot parse comes back untouched: this capability declining is a
// request that goes upstream as the caller wrote it, which is the same thing
// that happened before it existed.
func (c *Clamp) RewriteEgress(_ context.Context, view View, egress protocols.ProtocolID, body []byte, sink fact.Sink) ([]byte, error) {
	paths := ceilingFields(egress)
	if len(paths) == 0 {
		return body, nil
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return body, nil
	}

	// What the caller asked for is read before the candidate's limit is
	// requested, and the order is deliberate: a request that states no ceiling
	// cannot be clamped whatever the limit turns out to be, so asking would be
	// paying for an answer that changes nothing. What that costs depends on who
	// answers — a view may reach a database to find out — and embeddings and
	// completions, which never carry a ceiling, are exactly the traffic that
	// would pay it on every attempt.
	//
	// Reading first also keeps the reported figure honest: it is the ceiling the
	// caller stated, not whatever else the body happened to carry.
	highestAsked := 0
	for _, p := range paths {
		if asked, ok := readAt(doc, p); ok && asked > highestAsked {
			highestAsked = asked
		}
	}
	if highestAsked == 0 {
		return body, nil
	}

	limit := view.CandidateMaxOutput()
	if limit <= 0 {
		// No stated limit is not a limit of zero — it means this candidate
		// never told us one, and inventing it would clamp every request to
		// nothing.
		return body, nil
	}
	if highestAsked <= limit {
		return body, nil
	}

	// Some ceilings cannot be honoured without producing a request the upstream
	// is obliged to reject, and sending an invalid request is worse than sending
	// an over-large one: an over-large request is served, while a 400 is read as
	// the caller's own mistake and ends the exchange without trying the next
	// candidate — one of which may have had room for it.
	if reason, unsatisfiable := cannotBeHeldDown(doc, egress, limit); unsatisfiable {
		sink.Note(fact.MaxTokensClampSkipped{Reason: reason, Asked: highestAsked, Allowed: limit})
		return body, nil
	}

	for _, p := range paths {
		clampField(doc, p, limit)
	}

	// Lowering the ceiling can invalidate a body that was valid on the way in:
	// Anthropic requires a thinking budget strictly below max_tokens, so a
	// request whose budget sat under the caller's larger ceiling now sits above
	// ours and the upstream answers 400. A clamp that turns a servable request
	// into an error is worse than no clamp, so the budget comes down with it.
	if egress == protocols.ProtocolClaude {
		keepThinkingBudgetBelow(doc, limit)
	}

	out, err := json.Marshal(doc)
	if err != nil {
		// The body was parseable and is not any more, which is this
		// capability's own fault; send what the caller wrote rather than
		// nothing.
		return body, nil
	}

	sink.Note(fact.MaxTokensClamped{Asked: highestAsked, Allowed: limit})
	return out, nil
}

// fieldPath locates one number in a request: its name, and the object it sits
// in when that is not the top level.
type fieldPath struct{ field, nested string }

// ceilingFields names every way one protocol can state an output ceiling.
//
// OpenAI has two live spellings, not one: max_completion_tokens is what the
// reasoning models take and what current SDKs send, while max_tokens remains
// for everything older. Reading only the older name lets exactly the newest and
// most expensive requests through unclamped, which is the opposite of what this
// capability is for. Both are clamped when both appear, because which one the
// upstream honours is its choice, not ours.
//
// The clamp runs after encoding rather than on the shared intermediate form
// because the passthrough path never builds that form — a clamp there would
// reach only the requests that happened to be converted.
func ceilingFields(p protocols.ProtocolID) []fieldPath {
	switch p {
	case protocols.ProtocolOpenAI:
		return []fieldPath{{field: "max_tokens"}, {field: "max_completion_tokens"}}
	case protocols.ProtocolClaude:
		return []fieldPath{{field: "max_tokens"}}
	case protocols.ProtocolResponses:
		return []fieldPath{{field: "max_output_tokens"}}
	case protocols.ProtocolGemini:
		return []fieldPath{{field: "maxOutputTokens", nested: "generationConfig"}}
	default:
		return nil
	}
}

// readAt returns what one field states, or false when it is absent, unreadable,
// or nested inside an object the body does not carry.
func readAt(doc map[string]json.RawMessage, p fieldPath) (int, bool) {
	container, ok := objectAt(doc, p.nested)
	if !ok {
		return 0, false
	}
	return readInt(container, p.field)
}

// clampField lowers one field to limit. Fields that are absent, unreadable, or
// already within the limit are left exactly as they are.
func clampField(doc map[string]json.RawMessage, p fieldPath, limit int) {
	container, ok := objectAt(doc, p.nested)
	if !ok {
		return
	}
	asked, ok := readInt(container, p.field)
	if !ok || asked <= limit {
		return
	}
	writeInt(container, p.field, limit)
	putObject(doc, p.nested, container)
}

// cannotBeHeldDown reports whether holding this request down to limit would
// make it invalid, and why.
//
// One case exists today. Anthropic's manual thinking has TWO constraints, not
// one: the budget must sit below max_tokens AND at or above what Anthropic
// considers a usable budget. A candidate whose ceiling is at or under that
// floor leaves no number satisfying both, so any clamp here yields a request
// the upstream rejects. The Claude request normalisation elsewhere in this
// repository reaches the same wall and answers it by dropping thinking
// entirely and restoring the sampling parameters it had overridden — an answer
// that needs the caller's original values, which are no longer visible by the
// time a body has been encoded. Declining is the honest option from here.
func cannotBeHeldDown(doc map[string]json.RawMessage, egress protocols.ProtocolID, limit int) (reason string, unsatisfiable bool) {
	if egress != protocols.ProtocolClaude || limit > minUsableThinkingBudget {
		return "", false
	}
	thinking, ok := objectAt(doc, "thinking")
	if !ok {
		return "", false
	}
	budget, ok := readInt(thinking, "budget_tokens")
	if !ok || budget < limit {
		// Nothing to hold down, or it already fits.
		return "", false
	}
	return "thinking_budget_below_candidate_ceiling", true
}

// keepThinkingBudgetBelow restores Anthropic's budget_tokens < max_tokens
// invariant after the ceiling moved under the budget.
//
// Only the upper bound is applied. Anthropic's other constraint — a budget at
// or above what it considers usable — is what makes some ceilings impossible to
// honour at all, and cannotBeHeldDown has already turned those away; every
// request reaching here has room for a budget that satisfies both.
func keepThinkingBudgetBelow(doc map[string]json.RawMessage, limit int) {
	thinking, ok := objectAt(doc, "thinking")
	if !ok {
		return
	}
	budget, ok := readInt(thinking, "budget_tokens")
	if !ok || budget < limit {
		return
	}
	writeInt(thinking, "budget_tokens", limit-1)
	putObject(doc, "thinking", thinking)
}

// objectAt returns the top-level document itself when name is empty, and the
// object under that name otherwise. It reports false when the name is absent or
// does not hold an object.
func objectAt(doc map[string]json.RawMessage, name string) (map[string]json.RawMessage, bool) {
	if name == "" {
		return doc, true
	}
	raw, ok := doc[name]
	if !ok {
		return nil, false
	}
	var inner map[string]json.RawMessage
	if err := json.Unmarshal(raw, &inner); err != nil || inner == nil {
		return nil, false
	}
	return inner, true
}

// putObject writes a nested object back. Nothing to do for the top level: that
// map is the document.
//
// The re-encode cannot fail: every value in obj is either a fragment this
// package took out of a document json.Unmarshal accepted, or a number it wrote
// itself. An error return here would be a branch no test could ever enter.
func putObject(doc map[string]json.RawMessage, name string, obj map[string]json.RawMessage) {
	if name == "" {
		return
	}
	encoded, _ := json.Marshal(obj)
	doc[name] = encoded
}

// readInt returns what one field states as a whole number, reporting false only
// when the field is absent or is not a number at all.
//
// Every JSON spelling of a number counts, not just integer literals. A ceiling
// arrives as 9000.0 or 9e3 whenever the client computed it in a language whose
// arithmetic is floating point, and reading only 9000 treats those as no
// ceiling at all — which, for a cap, means the request goes out uncapped. The
// same hole as reading only one of the two field names, in a different disguise.
//
// A fractional ceiling truncates toward zero: "at most 4095.9" is a request for
// at most 4095 whole tokens, and rounding up would be this code asking for more
// than the caller did.
//
// A number too large to hold saturates rather than being discarded. Discarding
// it would be the bypass again, and at that magnitude it is over every limit
// there is — which is exactly what saturating says.
func readInt(obj map[string]json.RawMessage, field string) (int, bool) {
	raw, ok := obj[field]
	if !ok {
		return 0, false
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	if i, err := n.Int64(); err == nil {
		return saturate(float64(i)), true
	}
	f, err := n.Float64()
	if err == nil {
		// Rounded up, not truncated. A fraction is read here to answer one
		// question — is this above the limit — and truncating answers it wrong
		// at exactly the boundary: 4096.5 against a limit of 4096 would read as
		// 4096, count as within the limit, and travel on as 4096.5 for the
		// upstream to reject. Rounding up cannot understate what was asked for.
		return saturate(math.Ceil(f)), true
	}
	// Out of range is still a stated ceiling — an enormous one, over every
	// limit there is, so it saturates. Any other failure means this was not a
	// number after all: null unmarshals into a json.Number without complaint
	// and only fails here, and reading it as an enormous ceiling would clamp a
	// request that never stated one.
	if !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	if strings.HasPrefix(strings.TrimSpace(n.String()), "-") {
		return math.MinInt, true
	}
	return math.MaxInt, true
}

// saturate converts a number to int, pinning it at the ends of the range rather
// than wrapping. A wrapped ceiling reads as a small one and lets a huge request
// through untouched.
func saturate(f float64) int {
	if f >= float64(math.MaxInt) {
		return math.MaxInt
	}
	if f <= float64(math.MinInt) {
		return math.MinInt
	}
	return int(f)
}

// writeInt sets one field to a number. Encoding an int cannot fail.
func writeInt(obj map[string]json.RawMessage, field string, n int) {
	encoded, _ := json.Marshal(n)
	obj[field] = encoded
}
