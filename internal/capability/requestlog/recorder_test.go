package requestlog

import (
	"strings"
	"testing"

	"github.com/yolorouter/yolorouter/internal/fact"
)

func TestJoinCompressors(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"nil", nil, ""},
		{"empty", []string{}, ""},
		{"single", []string{"whitespace"}, "whitespace"},
		{"distinct", []string{"whitespace", "contractions"}, "whitespace,contractions"},
		{"duplicates preserved", []string{"whitespace", "contractions", "whitespace", "whitespace"}, "whitespace,contractions,whitespace,whitespace"},
		{"blanks filtered", []string{"", "whitespace", "", ""}, "whitespace"},
		{"repeats counted", []string{"log", "log", "diff"}, "log,log,diff"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinCompressors(tc.in); got != tc.want {
				t.Errorf("joinCompressors(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// unfamiliarRecord is what a capability this build has no column for would
// report. Declared in this package's test rather than in fact, because the
// point is that the recorder handles a type it was never taught about.
type unfamiliarRecord struct {
	fact.Base
	Detail string
}

func (unfamiliarRecord) RecordName() string { return "unfamiliar" }

// TestUnrecognisedRecordsSurviveIntoOverflow is the contract that makes the
// recording half of the vocabulary open.
//
// Dropping an unrecognised record is silent: the row still writes, just without
// the number, and nothing afterwards can tell an observation that never
// happened from one nobody made room for. Keeping it under its stable name
// turns a missing column into something an operator can find.
func TestUnrecognisedRecordsSurviveIntoOverflow(t *testing.T) {
	var tl fact.Timeline
	tl.Append(fact.Entry{Attempt: 1, Record: fact.UsageReported{Unit: fact.UnitToken, Source: fact.UsageFromUpstream, Prompt: 7, Completion: 3, Total: 10}})
	tl.Append(fact.Entry{Attempt: 1, Record: unfamiliarRecord{Detail: "kept"}})
	tl.Append(fact.Entry{Attempt: 1, Record: fact.UsageIncoherent{Reason: "contradictory"}})

	s := summarise(tl)

	// The recognised one still lands in its column.
	if s.inputTokens != 7 || s.outputTokens != 3 {
		t.Errorf("recognised record lost: input=%d output=%d", s.inputTokens, s.outputTokens)
	}
	if len(s.overflow) != 2 {
		t.Fatalf("want both unrecognised records collected, got %d: %+v", len(s.overflow), s.overflow)
	}
	names := map[string]bool{}
	for _, e := range s.overflow {
		names[e.Name] = true
	}
	// UsageIncoherent has no column of its own, so it belongs here too: an
	// audit row that simply shows zero tokens cannot say whether the upstream
	// reported nothing or reported something impossible.
	for _, want := range []string{"unfamiliar", "usage_incoherent"} {
		if !names[want] {
			t.Errorf("record %q was dropped", want)
		}
	}

	encoded := encodeOverflow(s.overflow, "req-1")
	if encoded == "" {
		t.Fatal("overflow encoded to nothing")
	}
	if !strings.Contains(encoded, "unfamiliar") || !strings.Contains(encoded, "kept") {
		t.Errorf("encoded overflow lost the record's content: %s", encoded)
	}
}

// TestARecognisedRecordDoesNotSwallowWhatThisRowCannotHold is the other half of
// the openness contract, and the easier half to lose.
//
// The unrecognised case above is loud by construction — nothing knows the type,
// so everything is kept. A recognised type is the dangerous one: the case
// arm copies the fields that have columns and whatever it did not name is gone,
// silently, with no default branch left to catch it. Being recognised then
// costs MORE than being unknown.
//
// Web searches are the live instance. The provider runs them on its own
// initiative and charges for them separately from tokens, states the count once
// in the usage the response ends with, and this row has no column for it. If
// the case arm eats the record, the count reaches the timeline and dies there —
// and nothing can re-derive it, because the response body is long gone.
func TestARecognisedRecordDoesNotSwallowWhatThisRowCannotHold(t *testing.T) {
	var tl fact.Timeline
	tl.Append(fact.Entry{Attempt: 1, Record: fact.UsageReported{
		Unit: fact.UnitToken, Source: fact.UsageFromUpstream,
		Prompt: 7, Completion: 3, Total: 10, WebSearchCount: 4,
	}})

	s := summarise(tl)

	if s.inputTokens != 7 || s.outputTokens != 3 {
		t.Errorf("the columns lost their counts: input=%d output=%d", s.inputTokens, s.outputTokens)
	}
	if len(s.overflow) != 1 {
		t.Fatalf("want the uncolumned part kept, got %d entries: %+v", len(s.overflow), s.overflow)
	}
	encoded := encodeOverflow(s.overflow, "req-searches")
	if !strings.Contains(encoded, "web_search_count") || !strings.Contains(encoded, "4") {
		t.Errorf("the search count did not reach the stored row: %s", encoded)
	}
}

// TestATokenCountedExchangeStoresNoResidue keeps the ordinary row clean. The
// residue above exists for what the columns cannot hold; a plain token exchange
// is entirely held by them, and duplicating those numbers into the overflow
// would make every row carry a second copy of its own token counts.
func TestATokenCountedExchangeStoresNoResidue(t *testing.T) {
	var tl fact.Timeline
	tl.Append(fact.Entry{Attempt: 1, Record: fact.UsageReported{
		Unit: fact.UnitToken, Source: fact.UsageFromUpstream,
		Prompt: 7, Completion: 3, Total: 10,
	}})

	if s := summarise(tl); len(s.overflow) != 0 {
		t.Errorf("an ordinary token exchange stored residue: %+v", s.overflow)
	}
}

// TestAnExchangeCountedInSomethingOtherThanTokensSaysSo covers the second thing
// the columns cannot express. Four columns named "tokens" hold the counts but
// not what was counted, so a character-counted exchange is indistinguishable
// from a token-counted one at rest — and the unit is spelled out rather than
// stored as its numeric constant, which renumbers whenever a unit is inserted.
func TestAnExchangeCountedInSomethingOtherThanTokensSaysSo(t *testing.T) {
	var tl fact.Timeline
	tl.Append(fact.Entry{Attempt: 1, Record: fact.UsageReported{
		Unit: fact.UnitCharacter, Source: fact.UsageFromUpstream, Prompt: 120, Total: 120,
	}})

	s := summarise(tl)
	if len(s.overflow) != 1 {
		t.Fatalf("want the unit kept, got %d entries", len(s.overflow))
	}
	if encoded := encodeOverflow(s.overflow, "req-chars"); !strings.Contains(encoded, "character") {
		t.Errorf("the row cannot say what it counted: %s", encoded)
	}
}

// TestAContradictionSurvivesIntoTheStoredRow is the residue field that matters
// most, because the row otherwise reads as ordinary.
//
// An impossible record still has four token counts, and they still land in
// their columns. What says they cannot be trusted has no column of its own, and
// the frame that proved it was folded away long before this runs. Losing the
// verdict here leaves a row that is indistinguishable from a good one — the
// numbers look fine, and nothing anywhere says otherwise.
func TestAContradictionSurvivesIntoTheStoredRow(t *testing.T) {
	var tl fact.Timeline
	tl.Append(fact.Entry{Attempt: 1, Record: fact.UsageReported{
		Unit: fact.UnitToken, Source: fact.UsageFromUpstream,
		Prompt: 1000, Completion: 2000, Total: 3000, Incoherent: true,
	}})

	s := summarise(tl)
	if len(s.overflow) != 1 {
		t.Fatalf("want the verdict kept, got %d entries: %+v", len(s.overflow), s.overflow)
	}
	if encoded := encodeOverflow(s.overflow, "req-impossible"); !strings.Contains(encoded, "incoherent") {
		t.Errorf("the stored row cannot say the counts were judged impossible: %s", encoded)
	}
}

// TestEveryColumnCarriesItsOwnNumber pins the mapping from record fields to
// row columns, with every value distinct.
//
// The columns are assigned one by one from same-typed fields, so reading the
// wrong field compiles, passes every test that reuses a value, and is only
// discovered by whoever reconciles an invoice. Distinct values everywhere is
// what makes each assignment falsifiable on its own.
func TestEveryColumnCarriesItsOwnNumber(t *testing.T) {
	var tl fact.Timeline
	tl.Append(fact.Entry{Attempt: 1, Record: fact.UsageReported{
		Unit: fact.UnitToken, Source: fact.UsageFromUpstream,
		Prompt: 11, Completion: 13, Total: 41, CacheRead: 17, CacheWrite: 19,
	}})
	tl.Append(fact.Entry{Attempt: 1, Record: fact.CostComputed{
		Known: true, Micros: 23, CacheReadSavedMicros: 29, CacheWriteExtraMicros: 31,
		CompressCostSavedMicros: 37,
	}})

	s := summarise(tl)

	checks := []struct {
		name string
		got  int64
		want int64
	}{
		{"inputTokens", int64(s.inputTokens), 11},
		{"outputTokens", int64(s.outputTokens), 13},
		{"cacheReadTokens", int64(s.cacheReadTokens), 17},
		{"cacheWriteTokens", int64(s.cacheWriteTokens), 19},
		{"costMicros", s.costMicros, 23},
		{"cacheReadSavedMicros", s.cacheReadSavedMicros, 29},
		{"cacheWriteExtraMicros", s.cacheWriteExtraMicros, 31},
		{"compressCostSavedMicros", s.compressCostSavedMicros, 37},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d: this column is being filled from some other field",
				c.name, c.got, c.want)
		}
	}
}

// TestRoutingFactsAreNotRecords guards the split: a fact that steers the
// request is not an observation with a column, and must not end up in the
// overflow pretending to be one.
func TestRoutingFactsAreNotRecords(t *testing.T) {
	var tl fact.Timeline
	tl.Append(fact.Entry{Attempt: 1, Fact: &fact.Fact{Kind: fact.KindPayloadRefused}})

	if s := summarise(tl); len(s.overflow) != 0 {
		t.Errorf("a routing fact leaked into the overflow: %+v", s.overflow)
	}
}

// TestNoOverflowEncodesToEmpty keeps the common case out of the column: a row
// where everything was recognised should store nothing, not "[]".
func TestNoOverflowEncodesToEmpty(t *testing.T) {
	if got := encodeOverflow(nil, "req-1"); got != "" {
		t.Errorf("encodeOverflow(nil) = %q, want empty", got)
	}
}

// TestDeliveryObservedReachesTheOverflowColumn proves the audit channel for
// Delivery.Complete actually exists.
//
// Complete is being collected precisely because nobody knows yet how often each
// kind of incomplete delivery happens. A field that is reported but never
// persisted answers no question at all, and the failure is silent: the bit sits
// in memory and the request ends. This test is what stops that from being the
// outcome.
func TestDeliveryObservedReachesTheOverflowColumn(t *testing.T) {
	var tl fact.Timeline
	tl.Append(fact.Entry{
		Attempt: 0,
		Record: fact.Truncated(200, 499, fact.FaultClient, "client_disconnected", nil).
			Observed(),
	})

	s := summarise(tl)
	if len(s.overflow) != 1 {
		t.Fatalf("want the delivery record collected, got %d: %+v", len(s.overflow), s.overflow)
	}
	if s.overflow[0].Name != "delivery_observed" {
		t.Errorf("overflow name = %q, want %q", s.overflow[0].Name, "delivery_observed")
	}

	encoded := encodeOverflow(s.overflow, "req-delivery")
	if encoded == "" {
		t.Fatal("delivery record encoded to nothing: it would never reach the column")
	}
	// The two statuses are the reason the record exists; losing either in
	// encoding would make the stored row unable to answer the question.
	for _, want := range []string{"200", "499", "client"} {
		if !strings.Contains(encoded, want) {
			t.Errorf("encoded overflow lost %q: %s", want, encoded)
		}
	}
}

// TestImageUnitReportCarriesItsCount: an image-unit report's delivered count
// lands in its column — priced or not, because the column is volume, not the
// bill (the pricing snapshot is the bill's explanation). A token-unit report
// must not touch it, even when it happens to carry a nonzero Count.
func TestImageUnitReportCarriesItsCount(t *testing.T) {
	var tl fact.Timeline
	tl.Append(fact.Entry{Attempt: 1, Record: fact.UsageReported{
		Unit: fact.UnitImage, Source: fact.UsageFromUpstream, Count: 3, Requested: 4,
	}})
	if s := summarise(tl); s.imageCount != 3 {
		t.Fatalf("imageCount = %d, want 3", s.imageCount)
	}

	var tlToken fact.Timeline
	tlToken.Append(fact.Entry{Attempt: 1, Record: fact.UsageReported{
		Unit: fact.UnitToken, Source: fact.UsageFromUpstream, Prompt: 5, Completion: 7, Total: 12,
	}})
	if s := summarise(tlToken); s.imageCount != 0 {
		t.Fatalf("token report touched imageCount: %d", s.imageCount)
	}
}
