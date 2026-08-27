package fact

import (
	"strconv"
	"time"
)

// The concrete type roster of the record vocabulary. The mechanism half —
// the Record interface and the Base marker — lives in record.go; this file
// is the open half, where a deployment grows the record types its own
// capabilities report without touching the mechanism.

// UsageReported carries the billable quantities for one exchange.
//
// Unit is what lets a modality redefine what is being counted without a
// parallel set of fields: a text exchange counts tokens, a speech exchange
// counts characters through the same Prompt field, and settlement reads Unit to
// know which price column applies.
type UsageReported struct {
	Base
	Unit                  Unit
	Source                UsageSource
	Prompt                int
	Completion            int
	Total                 int
	CacheRead             int
	CacheWrite            int
	CacheIncludedInPrompt bool
	// Reasoning is the share of the completion the model spent thinking.
	// Carried for the same reason Incoherent below is: settlement re-derives
	// its coherence verdict from this record, and one of the rules — reasoning
	// cannot exceed the completion — reads this count. A hop that drops it
	// quietly weakens that verdict on the copy that is actually priced.
	Reasoning int
	// Incoherent marks counts that contradict themselves — a negative token
	// count, a cache read larger than the prompt it was read from.
	//
	// It travels WITH the counts rather than being re-derived downstream,
	// because the evidence does not survive: a stream folds every frame into
	// one accumulated record, so the impossible frame that condemned it is
	// gone by the time anything could look again. A consumer that re-judges
	// what it receives sees only the plausible-looking remainder and prices it.
	//
	// Unknown is not zero. A marked record must not be billed at all — not
	// billed as free, which is a different claim and one a dashboard adds up.
	Incoherent bool
	// WebSearchCount is how many searches the provider performed on its own
	// initiative during the exchange. Not a token count and not priced here —
	// it is carried because it is the only evidence that they happened.
	//
	// Providers charge for these separately, and the number arrives once, in
	// the usage the response ends with. A capability that adds the surcharge
	// sees the exchange after it has been delivered; a quantity dropped on the
	// way to that point is one nobody downstream can re-derive, because the
	// frames it came from are gone.
	WebSearchCount int
}

func (UsageReported) RecordName() string { return "usage_reported" }

// UsageIncoherent reports that the upstream's own usage numbers contradict
// themselves. Settlement must not bill on them; the audit row keeps them so the
// contradiction stays visible instead of being silently normalised away.
type UsageIncoherent struct {
	Base
	Reason string
}

func (UsageIncoherent) RecordName() string { return "usage_incoherent" }

// Unit names what a usage count counts.
type Unit uint8

const (
	UnitToken Unit = iota
	UnitCharacter
	UnitImage
	UnitSecond
)

// String renders a unit for persistence and for logs.
//
// Next to the constants on purpose: a switch over them living in whichever
// package happened to need a name is a switch nobody updates when a unit is
// added here. The strings are stored, so they must not change once shipped —
// which is also why the numeric values are not stored, since inserting a unit
// renumbers every one after it.
//
// An unrecognised unit renders as its number rather than falling back to a
// plausible name. A new unit persisted as "token" is a wrong value that reads
// as a right one; "unit_4" is obviously something this build did not know.
func (u Unit) String() string {
	switch u {
	case UnitToken:
		return "token"
	case UnitCharacter:
		return "character"
	case UnitImage:
		return "image"
	case UnitSecond:
		return "second"
	default:
		return "unit_" + strconv.Itoa(int(u))
	}
}

// UsageSource records where the numbers came from. An upstream-reported count
// and one the gateway derived from the request are both legitimate, but an
// audit needs to tell them apart.
type UsageSource uint8

const (
	UsageAbsent UsageSource = iota
	UsageFromUpstream
	UsageFromRequest
)

// String renders a usage source for persistence, under the same rule as Unit:
// stored as a name, and an unrecognised one shows as its number rather than
// borrowing a name that would read as deliberate.
func (s UsageSource) String() string {
	switch s {
	case UsageAbsent:
		return "absent"
	case UsageFromUpstream:
		return "upstream"
	case UsageFromRequest:
		return "request"
	default:
		return "source_" + strconv.Itoa(int(s))
	}
}

// Several record types below have no producer in this build. They stay for
// the same reason the unread snapshot accessors do: this vocabulary is the
// contract capabilities report through, and the capabilities that produce
// these records are the ones not yet built against this kernel. A consumer
// meeting an unknown record already has a defined path (the overflow column),
// so an unproduced type costs nothing at run time.

// TokensSaved reports a successful input compression pass.
type TokensSaved struct {
	Base
	Compressors     []string
	EstimatedTokens int
}

func (TokensSaved) RecordName() string { return "tokens_saved" }

// MaxTokensClamped reports that a request asked for more output than the
// candidate serving it allows, and was held down to that candidate's ceiling.
//
// Both numbers are kept because only the pair explains the row: the ceiling
// alone says what the provider allows, and the asked-for figure is what an
// operator compares it against when a caller reports being cut short.
type MaxTokensClamped struct {
	Base
	Asked   int
	Allowed int
}

func (MaxTokensClamped) RecordName() string { return "max_tokens_clamped" }

// MaxTokensClampSkipped reports that a request was over the candidate's ceiling
// and was sent anyway, because holding it down would have produced a request
// the upstream must reject.
//
// Deliberately a separate type rather than a flag on MaxTokensClamped, for the
// same reason CompressionSkipped is separate from TokensSaved: "held down" and
// "could not be held down" are different events, and an operator investigating
// an over-budget response needs to see the second one rather than infer it from
// the absence of the first.
//
// Asked and Allowed carry the same two numbers as MaxTokensClamped and mean the
// same things — the ceiling the caller stated and the one the candidate allows.
// Reason names what made the gap unbridgeable, which is the only part the two
// records do not share.
type MaxTokensClampSkipped struct {
	Base
	Reason  string
	Asked   int
	Allowed int
}

func (MaxTokensClampSkipped) RecordName() string { return "max_tokens_clamp_skipped" }

// CompressionSkipped reports that compression was enabled but declined to act.
// Deliberately a separate type from TokensSaved rather than a zero value of it:
// "compressed nothing" and "did not compress" are different events, and a
// consumer that has to tell them apart by inspecting a count will eventually
// get it wrong.
type CompressionSkipped struct {
	Base
	Reason string
}

func (CompressionSkipped) RecordName() string { return "compression_skipped" }

// SystemPromptInjected reports that a configured system prompt was added, and
// how much text it contributed — the pre-request cost estimate needs the size,
// not the content.
type SystemPromptInjected struct {
	Base
	Site       string // where it landed in the request body
	ExtraChars int
}

func (SystemPromptInjected) RecordName() string { return "system_prompt_injected" }

// ModelRewritten reports that a model name was substituted.
type ModelRewritten struct {
	Base
	From  string
	To    string
	Where string // request or response
}

func (ModelRewritten) RecordName() string { return "model_rewritten" }

// FirstTokenAt reports how long the caller waited for the first meaningful byte
// of a streamed response.
//
// Elapsed is measured from the start of the ATTEMPT that produced the byte,
// not from the request's arrival. The two differ by whole seconds when
// earlier attempts failed and were retried, and the attempt baseline is the
// one the audit row has always meant: it grades the serving provider's
// latency, not the router's failover history. Exactly one report per
// exchange — the first attempt to produce a byte wins; retries never
// re-report.
type FirstTokenAt struct {
	Base
	Elapsed time.Duration
}

func (FirstTokenAt) RecordName() string { return "first_token_at" }

// RateLimitSnapshot carries the caller's remaining allowance at the moment the
// response was produced, for the compatibility headers and the audit row.
type RateLimitSnapshot struct {
	Base
	Limit     int
	Remaining int
	Unlimited bool
}

func (RateLimitSnapshot) RecordName() string { return "rate_limit_snapshot" }

// CostComputed carries what an exchange cost, once the kernel has priced it.
//
// It is a record rather than something a consumer recomputes because pricing
// and persistence must not be able to disagree: the number written to the audit
// row has to be the same number that was charged, and the only way to guarantee
// that is for both to read one value.
//
// Known distinguishes "cost nothing" from "could not be priced" — a request
// that never reached a priced candidate has no cost, which is not the same as a
// free one, and a dashboard that sums them together reports revenue that never
// existed.
type CostComputed struct {
	Base
	Known                   bool
	Micros                  int64
	CacheReadSavedMicros    int64
	CacheWriteExtraMicros   int64
	CompressCostSavedMicros int64
	// Prices is the snapshot of the four unit prices the pricing ran with —
	// present exactly when Known is true. It travels on the same record as the
	// cost because the two must not be able to disagree: a row whose cost and
	// snapshot came from different attempts could not be re-priced back to its
	// own cost_micros.
	Prices *SettledPrices
}

func (CostComputed) RecordName() string { return "cost_computed" }

// SettledPrices is the four unit prices (per million tokens) a settlement
// billed with. The cache prices are the effective ones — after the fallback
// that bills unconfigured cache tokens at the input price — because the
// snapshot answers "what was billed", not "what was configured".
type SettledPrices struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheWrite float64 `json:"cache_write"`
	CacheRead  float64 `json:"cache_read"`
}

// AttemptsRecorded carries how many upstream attempts ran and their detail.
//
// The count is what separates a request that reached an upstream from one
// rejected before any candidate was tried, which several consumers need:
// compression savings from a request that never left the building would inflate
// every savings metric that counts it.
type AttemptsRecorded struct {
	Base
	Count  int
	Detail string // JSON, empty when nothing ran
}

func (AttemptsRecorded) RecordName() string { return "attempts_recorded" }

// VisionFallbackApplied reports that the request's images were rewritten
// into text descriptions through the vision fallback model before dispatch.
// Described counts the images that actually got a description; a shortfall
// against Images means some fell back to a placeholder.
type VisionFallbackApplied struct {
	Base
	Model     string
	Images    int
	Described int
}

func (VisionFallbackApplied) RecordName() string { return "vision_fallback_applied" }

// VisionFallbackStripped reports that images were replaced with placeholders
// WITHOUT any describe call — either no fallback model is configured, or
// every image fell outside the describe limits. Stripping is what keeps an
// upstream that rejects image parts from failing the whole request.
type VisionFallbackStripped struct {
	Base
	Images int
}

func (VisionFallbackStripped) RecordName() string { return "vision_fallback_stripped" }

// VisionFallbackSkipped reports that the capability looked at an
// image-bearing request and left it alone, and why ("parse_failed",
// "patch_failed", "describe_failed") — leaving the caller's bytes untouched
// is the designed degrade, but an unexplained pass-through would be
// indistinguishable from the capability never running.
type VisionFallbackSkipped struct {
	Base
	Reason string
}

func (VisionFallbackSkipped) RecordName() string { return "vision_fallback_skipped" }
