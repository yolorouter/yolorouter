package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/logger"
	"go.uber.org/zap"
)

// generateRequestID returns a fresh 16-hex-char id for one gateway request
// (every failed response carries this so the admin can find the
// row). crypto/rand keeps it unguessable — it's surfaced to the caller, so a
// predictable counter would leak request volume / ordering.
func generateRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Fallback so a rand failure can never break request routing: epoch
		// nanos is still unique enough to find a log row.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// microsPerUnit is the fixed-point scale for stored money: one CNY = 1e6
// integer micros, i.e. 6 decimal places. Mirrors the frontend's MICROS_PER_UNIT
// (utils/money.ts) — the two must agree.
const microsPerUnit = 1_000_000

// costBreakdown is the per-request cost result: the billed cost plus the two
// cache-economics figures the cost view surfaces. Known=false means usage or
// pricing was missing — every field is 0 but the row must NOT read as "free".
type costBreakdown struct {
	CostMicros int64
	// CacheReadSavedMicros: how much cheaper the cache-read tokens were than
	// reprocessing them at the input price. CacheWriteExtraMicros: the premium
	// paid to establish the cache versus billing those tokens at the input
	// price. Both non-negative; net cache saving = read saved − write extra.
	CacheReadSavedMicros  int64
	CacheWriteExtraMicros int64
	// CompressCostSavedMicros is the ESTIMATED input-cost reduction from
	// input compression — tokensSaved priced at the candidate's input unit
	// rate. It is an estimate for reporting only; it does NOT participate in
	// billing (CostMicros above is the billed amount for tokens actually
	// sent). Zero when usage/pricing is unknown or no tokens were saved.
	CompressCostSavedMicros int64
	Known                   bool
}

// netPromptTokens returns the billable/loggable net input token count: the
// prompt tokens with the cache-read portion removed. When the source protocol
// reports prompt tokens inclusive of cache reads (OpenAI/Gemini/Responses,
// CacheIncludedInPrompt=true), the cache-read count is subtracted; when it
// reports a net input already (Anthropic input_tokens, flag false),
// PromptTokens is returned unchanged. Floored at 0 so an upstream reporting
// cache > prompt can't produce a negative. This is the single source of truth
// for "net input" — both computeCost and the request_logs.input_tokens value
// go through it, so billing and the persisted count stay consistent across
// protocols.
//
// Cache WRITE is subtracted only under the inclusive convention, and only
// because this gateway now puts it there. It is NOT subtracted from a net
// (Anthropic) count, where cache_creation_input_tokens sits alongside
// input_tokens rather than inside it — deducting it there would remove tokens
// the prompt never contained.
//
// The inclusive case used to be unconditionally safe to ignore, since no
// standard protocol has a cache-write concept: OpenAI's prompt_tokens_details
// carries only cached_tokens and Gemini's promptTokenCount folds in only
// cachedContentTokenCount. That stopped being true once the gateway began
// emitting and accepting the cache_creation_input_tokens alias (see
// protocols.CacheWriteAliasField) on those wires, where the write portion
// genuinely IS part of the reported prompt total. Leaving it in would count
// those tokens twice: once as cache write, once as fresh input.
//
// Delegates to protocols.IRUsage.NetPromptTokens rather than repeating the
// subtraction, because the Claude egress encoder derives the same quantity
// through that method. Two implementations of one definition is how the
// input_tokens a client is shown and the input_tokens persisted to request_logs
// come to disagree about the same request.
func netPromptTokens(usage *protocols.IRUsage) int {
	if usage == nil {
		return 0
	}
	return usage.NetPromptTokens()
}

// normalizeCacheConvention settles which cache-accounting convention a usage
// record actually follows, before anything prices or persists it.
//
// The decision itself lives in protocols.CacheSitsOutsidePrompt, which the
// egress encoders reach through IRUsage.PromptIncludesCache. Both sides MUST
// keep asking that one function: this gateway settles the question once per
// request against the persisted copy of the usage, while an encoder settles it
// per frame against the IR copy, long before this runs. A second implementation
// here would let the input_tokens a client is shown and the input_tokens
// written to request_logs disagree about the same request.
//
// Applied by mutating the flag rather than deferring to the predicate at every
// read, because the counts are consumed several times below (pricing, the
// persisted row, the compression denominator) and a partially-normalized record
// is exactly the inconsistency this exists to prevent.
func normalizeCacheConvention(u *protocols.IRUsage) {
	if u == nil {
		return
	}
	if protocols.CacheSitsOutsidePrompt(*u) {
		u.CacheIncludedInPrompt = false
	}
}

// usageIsCoherent reports whether the upstream-reported counts describe a
// physically possible request. Counts arrive from third-party upstreams over
// the wire, so a buggy or hostile provider can send negatives (JSON numbers are
// signed) or a cache-read larger than the prompt it was supposedly read from.
// Either would flow straight into pricing: a negative cache count produces a
// negative line item, and an oversized one bills cache reads the request never
// performed. Incoherent usage is treated exactly like absent usage — unknown,
// never zero and never billed.
//
// Delegates to the single IR-level verdict (protocols.IRUsage.IsIncoherent) so
// the wire encoders and this billing gate read the SAME answer instead of each
// re-judging with its own predicate. The verdict was already computed at the
// decoder exit and carried on Invalid; re-evaluating the same predicate here
// catches the records where Merge erased the evidence before the mark was set.
// Both callers run normalizeCacheConvention first, so PromptIncludesCache (which
// IsIncoherent relies on) has the settled convention to read.
func usageIsCoherent(u *protocols.IRUsage) bool {
	if u == nil {
		return false
	}
	return !u.IsIncoherent()
}

// compressTokensSaved reads the input-compression estimate back off the
// timeline.
//
// Pricing it needs the count, and only the kernel knows the rate — but the
// count belongs to whichever capability did the shrinking, and it arrives the
// same way every other capability observation does. Reading it here, from the
// one record that carries it, is what keeps the priced saving and the audit
// row describing the same pass: they are two readings of a single report
// rather than two fields that can drift apart.
//
// Zero when nothing compressed, which prices to nothing.
func compressTokensSaved(t fact.Timeline) int {
	for _, e := range t.All() {
		if rec, ok := e.Record.(fact.TokensSaved); ok {
			return rec.EstimatedTokens
		}
	}
	return 0
}

// computeCost returns the cost breakdown in integer micros (major-unit × 1e6,
// i.e. CNY to 6 decimal places) and whether the cost is "known".
// Unknown = usage missing — the row records cost_micros=0 with
// cost_known=false so the dashboard never shows it as a free request.
// compressTokensSaved is the input-compression estimate (chars-saved/4); the
// matching CompressCostSavedMicros line prices it at the candidate's input
// rate so the saving is reported on the same basis as the billed cost. It is
// forced to 0 whenever usage/pricing is unknown, matching CostKnown=false.
// Candidate prices are CNY per million tokens.
func computeCost(cand *model.ModelCandidate, usage *protocols.IRUsage, compressTokensSaved int) costBreakdown {
	if cand == nil || !usageIsCoherent(usage) {
		return costBreakdown{} // Known=false: no cost recorded, no budget consumed
	}
	// cost = net_input × input_price
	//      + cache_read × cache_read_price
	//      + cache_write × cache_write_price
	//      + completion × output_price
	// net_input is the non-cached input (netPromptTokens): cache tokens are
	// billed on their own lines below, so they must not also be charged at the
	// input price. Candidate prices are CNY per million tokens.
	cacheRead := usage.CacheReadTokens
	cacheWrite := usage.CacheWriteTokens
	nonCacheInput := netPromptTokens(usage)
	// Candidate without a configured cache price bills cache tokens at the
	// input price (when a candidate has no cache price configured, its cache tokens are billed at the input unit price).
	cacheReadPrice := cand.InputPrice
	if cand.CacheReadPrice != nil {
		cacheReadPrice = *cand.CacheReadPrice
	}
	cacheWritePrice := cand.InputPrice
	if cand.CacheWritePrice != nil {
		cacheWritePrice = *cand.CacheWritePrice
	}
	cost := float64(nonCacheInput)/1_000_000*cand.InputPrice +
		float64(cacheRead)/1_000_000*cacheReadPrice +
		float64(cacheWrite)/1_000_000*cacheWritePrice +
		float64(usage.CompletionTokens)/1_000_000*cand.OutputPrice
	// Cache economics, both against the input price as the "no cache" baseline.
	// Reads save when priced below input; writes cost a premium when priced
	// above it. Each is floored at 0 so a candidate whose cache price sits on
	// the wrong side of input never turns into a negative on the other line.
	cacheReadSaved := max(0, float64(cacheRead)/1_000_000*(cand.InputPrice-cacheReadPrice))
	cacheWriteExtra := max(0, float64(cacheWrite)/1_000_000*(cacheWritePrice-cand.InputPrice))
	// Scale CNY to integer micros so cumulative budget accounting stays
	// exact-integer (no float drift) while keeping 6-decimal cost precision.
	// (microsPerUnit is a distinct constant from the /1_000_000 above, which is
	// the "price per million tokens" divisor — same literal, different meaning.)
	// Compress savings use the same input rate: tokens × cand.InputPrice/M-tokens
	// — the /1e6 from per-M pricing and the ×1e6 to micros cancel, leaving
	// tokens × InputPrice (in micros). Reported only; not added to CostMicros.
	var compressSavedMicros int64
	if compressTokensSaved > 0 {
		compressSavedMicros = int64(float64(compressTokensSaved)*cand.InputPrice + 0.5)
	}
	// Converting an out-of-range float64 to int64 is undefined in Go — the
	// result is whatever the platform produces, which for a budget counter
	// means an arbitrary (possibly negative) charge. Coherent token counts can
	// still multiply into an out-of-range figure against an absurd unit price,
	// so the conversion is guarded rather than assumed safe.
	micros := cost*microsPerUnit + 0.5
	if micros < 0 || micros > math.MaxInt64 {
		return costBreakdown{}
	}
	return costBreakdown{
		CostMicros:              int64(micros),
		CacheReadSavedMicros:    int64(cacheReadSaved*microsPerUnit + 0.5),
		CacheWriteExtraMicros:   int64(cacheWriteExtra*microsPerUnit + 0.5),
		CompressCostSavedMicros: compressSavedMicros,
		Known:                   true,
	}
}

// safeUpstreamMessage produces the message shown to the caller for a 4xx
// non-auth upstream failure. The upstream body is NOT forwarded verbatim —
// it can echo back parts of the request (including the rewritten model) and,
// for some providers, fragments of credential detail. A bare
// "upstream returned status N" is enough for the caller to act on.
func safeUpstreamMessage(status int) string {
	return fmt.Sprintf("upstream returned status %d", status)
}

// finalize writes the request_logs row and, when cost is known and positive,
// accumulates the spend onto the API key's budget_spent_micros. Called on
// every exit path (success, every failure class) so each gateway request
// produces exactly one row. rc.attempt.Candidate()/Provider/Usage may be nil
// on early failures (before any candidate was tried); finalize is nil-safe
// for all of them.
//
// Budget accumulation uses the COST from this request, not a re-read of the
// row — two concurrent requests that each compute their own cost and add it
// atomically (repository.IncrementAPIKeyBudgetSpent is a single
// budget_spent_micros = budget_spent_micros + ? UPDATE) cannot lose updates to
// each other.
func (s *Service) finalize(rc *Exchange, usage *protocols.IRUsage, statusCode int, failReason string, start time.Time) {
	if rc.logWritten.Swap(true) {
		return // already finalized (e.g. Handle's panic-recovery defer after a normal finalize)
	}
	rc.statusCode = statusCode
	// Stop the clock before any persistence runs. What follows writes to the
	// database, and a budget update waiting on a row lock would otherwise be
	// billed to the caller as request latency — inflating exactly the dashboard
	// someone consults to find out whether the gateway is slow.
	duration := time.Since(start)

	sink := newKernelSink(rc)

	// Settle the cache convention once, here, before either consumer of the
	// usage reads it: pricing below and the persisted counts must not be able to
	// disagree about whether the prompt includes the cache. One normalization
	// point covers every ingress protocol and both the streaming and
	// non-streaming paths.
	//
	// A truncated stream can reach this with a partial record (the pumps keep
	// whatever usage arrived before the cut, deliberately), so the
	// reclassification must not depend on the record being complete — it
	// doesn't: a partial record whose stated total no longer corroborates the
	// net reading simply stays inclusive and is rejected as incoherent, exactly
	// as it was before.
	normalizeCacheConvention(usage)
	cost := computeCost(rc.attempt.Candidate(), usage, compressTokensSaved(rc.timeline))

	billedUsage := s.reportUsage(rc, usage, sink)
	sink.Note(fact.CostComputed{
		Known:                   cost.Known,
		Micros:                  cost.CostMicros,
		CacheReadSavedMicros:    cost.CacheReadSavedMicros,
		CacheWriteExtraMicros:   cost.CacheWriteExtraMicros,
		CompressCostSavedMicros: cost.CompressCostSavedMicros,
	})
	sink.Note(attemptsRecord(rc))

	// Charging happens here rather than in a recorder: what the caller is billed
	// is not an audit concern, and a deployment that swapped out its audit trail
	// must not be able to stop collecting money by accident.
	if cost.Known && cost.CostMicros > 0 {
		if err := repository.IncrementAPIKeyBudgetSpent(s.db, rc.apiKeyID, cost.CostMicros); err != nil {
			logger.Error("gateway: increment budget spent failed",
				zap.String("request_id", rc.requestID), zap.Error(err))
		}
	}

	// The outcome is stored rather than handed straight to the recorders,
	// because recording must be the last thing that happens: admissions release
	// after this returns, and one that reports its own final accounting would
	// otherwise report it into a timeline nobody reads again. Handle arms the
	// recorder pass before it arms anything else, so it unwinds last.
	// This is the ONE outcome for the exchange. The recorders read it here and
	// the admission releases read it from Handle's defer; building a second one
	// at either site would let the two drift — a different duration, a URL one
	// of them never saw — and a capability implementing both Release and Record
	// would reconcile a request against two accounts of the same ending.
	rc.outcome = fact.Outcome{
		StatusCode:  statusCode,
		FailReason:  failReason,
		Duration:    duration,
		Attempts:    len(rc.attempts),
		Delivered:   rc.firstByteSent,
		UpstreamURL: rc.attempt.UpstreamURL(),
		// The settlement half: what the exchange is billed as, from the same
		// records the audit trail keeps. Usage nil (nothing billable
		// delivered, or counts too incoherent to bill) is what a Release
		// reverses on; usage present is what it settles.
		Usage:      billedUsage,
		CostMicros: cost.CostMicros,
		CostKnown:  cost.Known,
	}
	rc.outcomeSettled = true
}

// recordTerminal hands the settled exchange to the recorders.
//
// It is separate from finalize, and runs later, because recording must be the
// last thing that happens: admissions release after finalize returns, and one
// that reports its own final accounting would otherwise report it into a
// timeline nobody reads again.
//
// A no-op when nothing settled the exchange, which means no work was ever done
// on its behalf and there is nothing to describe.
func (s *Service) recordTerminal(rc *Exchange) {
	if !rc.outcomeSettled {
		return
	}
	s.runRecorders(rc.requestCtx, rc, rc.outcome)
}

// reportUsage puts the billable counts on the timeline, or records that the
// upstream's own numbers contradicted themselves.
//
// An incoherent record is reported as such rather than persisted raw: a
// negative or impossible count poisons every SUM() the dashboard runs, and the
// same rejection is applied to pricing, so the stored counts can never disagree
// with the billing decision.
func (s *Service) reportUsage(rc *Exchange, usage *protocols.IRUsage, sink fact.Sink) *fact.UsageReported {
	if usage == nil {
		return nil
	}
	if !usageIsCoherent(usage) {
		logger.Warn("gateway: upstream reported incoherent token usage, counts dropped",
			zap.String("request_id", rc.requestID),
			zap.Int("prompt_tokens", usage.PromptTokens),
			zap.Int("completion_tokens", usage.CompletionTokens),
			zap.Int("cache_read_tokens", usage.CacheReadTokens),
			zap.Int("cache_write_tokens", usage.CacheWriteTokens))
		sink.Note(fact.UsageIncoherent{Reason: "upstream counts do not corroborate each other"})
		return nil
	}
	// The NET input (cache excluded) is what is persisted, so every
	// SUM(input_tokens) shares one convention across protocols and cache tokens
	// stay in their own columns.
	billed := fact.UsageReported{
		Unit:       fact.UnitToken,
		Source:     fact.UsageFromUpstream,
		Prompt:     netPromptTokens(usage),
		Completion: usage.CompletionTokens,
		Total:      usage.TotalTokens,
		CacheRead:  usage.CacheReadTokens,
		CacheWrite: usage.CacheWriteTokens,
		Reasoning:  usage.ReasoningTokens,
		// Carried even though nothing here prices it. This is the row that gets
		// persisted, and the searches are stated once, in a response body that
		// is gone by the time anyone could ask again. A record that omits them
		// is a record that says none happened.
		WebSearchCount: usage.WebSearchCount,
	}
	sink.Note(billed)
	// Returned as well as noted, and it is the SAME record on purpose: the
	// outcome the admission releases settle from must not be able to disagree
	// with the row the audit trail persists.
	return &billed
}

// attemptsRecord carries the attempt count and, when anything ran, its detail.
// The detail is JSON so a query page can render it without a schema change.
func attemptsRecord(rc *Exchange) fact.AttemptsRecorded {
	rec := fact.AttemptsRecorded{Count: len(rc.attempts)}
	if len(rc.attempts) > 0 {
		if detail, err := json.Marshal(rc.attempts); err == nil {
			rec.Detail = string(detail)
		}
	}
	return rec
}

// settleCheckedDelivery settles a delivery that has already been through
// checkAndNote, and reports back how far the chain may still go.
//
// This is the one place a delivery becomes a settled request. The check is left
// to the caller because it has to happen before the attempt is labelled — a
// delivery about to be judged impossible would otherwise leave a "success" row
// behind it. Checking again here would be harmless to the verdict and would
// record the observation twice, which is one audit fact too many.
func (s *Service) settleCheckedDelivery(c *gin.Context, rc *Exchange, d fact.Delivery, sink *exchangeSink, start time.Time) attemptResult {
	if d.Verdict != fact.VerdictSettled {
		// Nothing reached the caller, so the chain continues and there is
		// nothing to settle yet.
		return attemptNextCandidate
	}
	if d.FailReason == invalidDeliveryReason && c != nil && !c.Writer.Written() {
		// The substitution above ends the request, but the delivery it replaced
		// was going to continue the chain — so nobody has written anything to
		// the caller, and nobody will. Without this they would receive an empty
		// implicit 200 while the row recorded a 500.
		WriteIngressError(c, rc.ingress, http.StatusInternalServerError, errTypeServer,
			"internal error", rc.requestID)
	}
	s.settleChecked(rc, d, sink, start)
	return attemptSuccess
}

// settle ends a request that was not produced by a delivery attempt.
//
// Everything that ends a request goes through here: a delivered response, a
// rejection before any candidate was tried, a chain that ran out, a caller that
// hung up. They used to end in twenty-eight separate places, each deciding for
// itself what status to settle on and what to call the failure, and nothing
// held them to the same answer.
//
// Use this only when nothing was appended to the attempt list in the same
// breath — a rejection before any candidate ran, or a chain that ended after
// the loop had already recorded everything it tried. A settlement that
// follows its own append passes settleOptions{againstRecordedAttempt: true}.
func (s *Service) settle(rc *Exchange, d fact.Delivery, start time.Time, opts settleOptions) {
	sink := newSettlementSink(rc, opts.againstRecordedAttempt)
	s.checkAndNote(rc, &d, sink)
	s.settleChecked(rc, d, sink, start)
}

// settleOptions states what a settlement is filed against. The zero value is
// right whenever no attempt record was appended in the same breath as the
// settlement; the one option covers the opposite case.
type settleOptions struct {
	// againstRecordedAttempt files the settlement against the attempt record
	// appended immediately before the call, rather than the one that would
	// come next. The sink's default numbering assumes the record for the
	// attempt in progress does not exist yet, which is what a capability
	// reporting from inside an attempt sees. A settlement that follows its
	// own append sees the opposite, and the request ends there — so the
	// number the default produces belongs to an attempt that never runs.
	againstRecordedAttempt bool
}

// settleChecked settles a delivery that has already been through checkAndNote.
// Callers that must read the verdict AFTER the check — because the check can
// change it — use this instead of settle, so the delivery is not checked and
// recorded twice.
func (s *Service) settleChecked(rc *Exchange, d fact.Delivery, sink *exchangeSink, start time.Time) {
	s.observeDelivery(rc, d, sink)
	s.logSettlement(rc, d)
	s.finalize(rc, usageFromReport(d.Usage), d.BillingStatus, d.FailReason, start)
}

// logSettlement records how a request ended, at the level its ending deserves.
//
// What decides that is the delivery's own account of itself — who is at fault
// and what the request is billed as — not whether the constructor that built it
// happened to have an error in hand. Keying on the error made two ordinary
// endings shout: a provider that reliably omits the stream terminator produced
// a warning per SUCCESSFUL request, and a caller pressing stop produced one per
// cancellation. It also made the same event log at two different levels
// depending on which construction site it came through, since the other
// disconnect path builds its delivery without an error at all.
//
// The routine half goes to debug, which a default deployment does not print at
// all — deliberately, because the audit row is where every request is recorded
// whatever its ending. What this function decides is only which endings are
// worth interrupting somebody over.
func (s *Service) logSettlement(rc *Exchange, d fact.Delivery) {
	fields := []zap.Field{
		zap.String("request_id", rc.requestID),
		zap.String("fail_reason", d.FailReason),
		zap.Int("billing_status", d.BillingStatus),
		zap.String("fault", d.Fault.String()),
	}
	if d.Err != nil {
		// The reason code is what gets persisted; the error itself only ever
		// existed to be read by a human.
		fields = append(fields, zap.Error(d.Err))
	}
	if settlementIsRoutine(d) {
		logger.Debug("gateway: request settled", fields...)
		return
	}
	logger.Warn("gateway: request settled on a failure", fields...)
}

// settlementIsRoutine reports whether an ending is one an operator has no
// reason to look at.
//
// A caller who leaves is not a malfunction — it is the most common way a
// streaming request ends, and their leaving says nothing about this gateway or
// the provider. Below that, a request billed under 500 was answered, and a
// refusal the caller received whole is as much an answer as a completion: an
// upstream 400 describes the request, not an outage, and one warning per
// malformed request is one per caller mistake.
//
// What that leaves — and what is deliberately NOT routine — is the ending in
// between: a caller shown a success status who then did not get the rest of it.
// It is billed 200, settles 200, and reads like a success in every column that
// gets persisted, so this line is the only place it can be seen at all. New
// truncation sites therefore default to loud, which is the safe direction to be
// wrong in.
//
// The one exception is a stream that ended without its terminator. It is not
// recorded as complete because the end was never seen, but the caller very
// likely holds the whole completion — some providers simply never send it, and
// keying on that would produce a warning per SUCCESSFUL request.
func settlementIsRoutine(d fact.Delivery) bool {
	if d.Fault == fact.FaultClient {
		return true
	}
	if d.BillingStatus >= 500 {
		return false
	}
	if d.Committed && !d.Complete && d.ClientStatus < 400 {
		return d.FailReason == noDoneTerminatorReason
	}
	return true
}

// invalidDeliveryReason marks a settlement the kernel substituted for one it
// could not read. Named because two places have to agree on it.
const invalidDeliveryReason = "invalid_delivery"

// checkAndNote refuses a delivery that cannot describe anything real, and
// records the shape of the one that survives.
//
// A delivery that cannot describe anything real is a bug on the reporting side,
// and acting on it would settle the request against values that mean nothing.
// It is replaced by one that blames us and ends the request — the caller has
// already been served whatever actually went out, so the only open question is
// what to record. Callers must read the verdict only AFTER calling this: the
// replacement changes it.
func (s *Service) checkAndNote(rc *Exchange, d *fact.Delivery, sink *exchangeSink) {
	if err := d.Validate(); err != nil {
		logger.Error("gateway: refusing an impossible delivery",
			zap.String("request_id", rc.requestID), zap.Error(err))
		*d = fact.Undelivered(http.StatusInternalServerError, fact.VerdictSettled,
			fact.FaultGateway, invalidDeliveryReason, err)
	}
	sink.Note(d.Observed())
}

// rejectRequest answers a request that never reached an upstream and settles it.
//
// The status is given once. It used to be written twice — once into the error
// the caller sees and once into what the request settles as — with nothing
// keeping the two the same.
func (s *Service) rejectRequest(c *gin.Context, rc *Exchange, status int, errType, message, failReason string, at fact.Fault, start time.Time) {
	WriteIngressError(c, rc.ingress, status, errType, message, rc.requestID)
	s.settle(rc, fact.Rejected(status, at, failReason, nil), start, settleOptions{})
}

// abandonRequest settles a request whose caller is already gone. Nothing is
// written, because there is nobody left to write to. The disconnects noticed
// inside a candidate — which record the attempt they died on before
// settling — pass settleOptions{againstRecordedAttempt: true}.
func (s *Service) abandonRequest(rc *Exchange, failReason string, start time.Time, opts settleOptions) {
	s.settle(rc, callerGone(failReason), start, opts)
}

// callerGone describes a request nobody is waiting for any more: nothing was
// delivered, and no other candidate would have anywhere to deliver it.
func callerGone(failReason string) fact.Delivery {
	return fact.Undelivered(499, fact.VerdictSettled, fact.FaultClient, failReason, nil)
}
