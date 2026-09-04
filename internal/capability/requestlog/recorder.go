// Package requestlog persists the audit trail for one exchange: the summary row
// and the captured bodies.
//
// It learns what happened from the reported timeline rather than by reading
// each capability's own state. That is the whole reason the timeline exists.
// The arrangement it replaces had one function reaching into a shared struct
// for every field it wanted, which meant a capability that renamed or stopped
// setting a field silently lost its column — the row still wrote, just with a
// zero where the number used to be, and nothing failed.
package requestlog

import (
	"context"
	"encoding/json"
	"strings"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/logger"
	"go.uber.org/zap"
)

// View is what the recorder reads off the exchange: the caller's identity and
// route, and the captured bodies.
//
// It is the widest view any capability declares, which is expected — an audit
// row summarises everything. The width being written down here, in the package
// that has it, is the improvement: it is a list someone can review, rather than
// an open licence to read any field.
//
// Everything that some other capability produced — usage, cost, attempts,
// compression outcome — is deliberately absent. Those arrive through the
// timeline.
type View interface {
	RequestID() string
	APIKeyID() uint
	UserID() uint
	OriginalModel() string
	ProviderID() *uint
	IsStream() bool
	IngressPath() string
	CallSource() string
	ParentRequestID() string
	UpstreamURL() string

	RequestHeaders() []byte
	RequestBody() []byte
	CompressedRequestBody() []byte
	UpstreamRequestBody() []byte
	ResponseBody() []byte
	UpstreamResponseBody() []byte
	StreamBodyPath() string
	StreamBodyTruncated() bool
	ImagePricingSnapshot() string
	AudioPricingSnapshot() string
}

// Recorder writes the audit trail.
type Recorder struct {
	db *gorm.DB
}

// New returns a recorder that writes through db.
func New(db *gorm.DB) *Recorder { return &Recorder{db: db} }

// Name identifies the recorder in logs.
func (*Recorder) Name() string { return "request_log" }

// Record writes the summary row and the body row.
//
// A failure to write bodies is logged and no more: the summary row carries the
// billing figures and is the authoritative one, so it must not be rolled back
// because a diagnostic capture could not be stored.
func (r *Recorder) Record(ctx context.Context, view View, out fact.Outcome, tl fact.Timeline) {
	s := summarise(tl)

	apiKeyID := view.APIKeyID()
	// Ownership is denormalized onto the row so per-user statistics never
	// join through api_keys. 0 (a key that predates ownership and somehow
	// escaped the migration backfill) is stored as NULL rather than as a
	// dangling id that matches no account.
	var userIDPtr *uint
	if uid := view.UserID(); uid != 0 {
		userIDPtr = &uid
	}
	var failPtr *string
	if out.FailReason != "" {
		fr := out.FailReason
		failPtr = &fr
	}

	// Compression savings only mean something if the request actually reached an
	// upstream. One that compressed a body and was then rejected before any
	// candidate ran must not inflate the savings metrics — it saved nothing,
	// because nothing was sent. The skip reason is kept either way: it is
	// audit-only and is most useful precisely on the paths that never dispatched.
	tokensSaved, costSaved, compressors := s.compressTokensSaved, s.compressCostSavedMicros, s.compressorsApplied
	if s.attempts == 0 {
		tokensSaved, costSaved, compressors = 0, 0, ""
	}

	row := &model.RequestLog{
		RequestID:                        view.RequestID(),
		APIKeyID:                         &apiKeyID,
		UserID:                           userIDPtr,
		ModelName:                        view.OriginalModel(),
		ProviderID:                       view.ProviderID(),
		IsStream:                         view.IsStream(),
		StatusCode:                       out.StatusCode,
		InputTokens:                      s.inputTokens,
		OutputTokens:                     s.outputTokens,
		CacheWriteTokens:                 s.cacheWriteTokens,
		CacheReadTokens:                  s.cacheReadTokens,
		ImageCount:                       s.imageCount,
		UsageCharacters:                  s.usageCharacters,
		CostMicros:                       s.costMicros,
		CostKnown:                        s.costKnown,
		CacheReadSavedMicros:             s.cacheReadSavedMicros,
		CacheWriteExtraMicros:            s.cacheWriteExtraMicros,
		CompressEstimatedTokensSaved:     tokensSaved,
		CompressEstimatedCostSavedMicros: costSaved,
		CompressSkipReason:               s.compressSkipReason,
		CompressorsApplied:               compressors,
		RequestPath:                      view.IngressPath(),
		UpstreamURL:                      view.UpstreamURL(),
		Source:                           view.CallSource(),
		ParentRequestID:                  view.ParentRequestID(),
		FailReason:                       failPtr,
		Attempts:                         s.attempts,
		DurationMs:                       out.Duration.Milliseconds(),
		FactsJSON:                        encodeOverflow(s.overflow, view.RequestID()),
	}
	if s.attemptsDetail != "" {
		detail := s.attemptsDetail
		row.AttemptsDetail = &detail // *string so empty stays SQL NULL, not ''
	}
	// The four snapshot columns are set together or not at all: a partial
	// snapshot could not re-price anything and would read as a data bug.
	if p := s.settledPrices; p != nil {
		inPrice, outPrice, cwPrice, crPrice := p.Input, p.Output, p.CacheWrite, p.CacheRead
		row.SettledInputPrice = &inPrice
		row.SettledOutputPrice = &outPrice
		row.SettledCacheWritePrice = &cwPrice
		row.SettledCacheReadPrice = &crPrice
	}

	row.ImagePricingSnapshot = view.ImagePricingSnapshot()
	row.AudioPricingSnapshot = view.AudioPricingSnapshot()

	if err := repository.CreateRequestLog(r.db.WithContext(ctx), row); err != nil {
		logger.Error("gateway: write request log failed",
			zap.String("request_id", view.RequestID()), zap.Error(err))
	}

	bodyRow := &model.RequestLogBody{
		RequestID:             view.RequestID(),
		RequestHeaders:        string(view.RequestHeaders()),
		RequestBody:           string(view.RequestBody()),
		UpstreamRequestBody:   string(view.UpstreamRequestBody()),
		ResponseBody:          string(view.ResponseBody()),
		UpstreamResponseBody:  string(view.UpstreamResponseBody()),
		StreamBodyPath:        view.StreamBodyPath(),
		StreamBodyTruncated:   view.StreamBodyTruncated(),
		CompressedRequestBody: string(view.CompressedRequestBody()),
	}
	if err := repository.UpsertRequestLogBody(r.db.WithContext(ctx), bodyRow); err != nil {
		logger.Error("gateway: write request log body failed",
			zap.String("request_id", view.RequestID()), zap.Error(err))
	}
}

// summary is what the timeline said, reduced to the columns this row has.
type summary struct {
	inputTokens, outputTokens         int
	cacheWriteTokens, cacheReadTokens int
	// imageCount is the delivered-image count of an image-unit usage report.
	// Unlike the pricing snapshot, it is volume, not a bill: it is written
	// even when no price resolved.
	imageCount int
	// usageCharacters is the counted characters of a character-unit usage
	// report, in the settling candidate's own billing meter. Same
	// volume-not-bill contract as imageCount.
	usageCharacters         int
	costKnown               bool
	costMicros              int64
	settledPrices           *fact.SettledPrices
	cacheReadSavedMicros    int64
	cacheWriteExtraMicros   int64
	compressCostSavedMicros int64
	compressTokensSaved     int
	compressorsApplied      string
	compressSkipReason      string
	attempts                int
	attemptsDetail          string
	// overflow holds every record this build has no column for, so it is
	// stored rather than dropped.
	overflow []overflowEntry
}

// usageNotInColumns is the part of a usage record this row cannot hold.
//
// A record type declared here rather than a bare map, for the same reason the
// vocabulary requires it everywhere else: a renamed field has to be a compile
// error, not a value that silently stops being written.
//
// It lists EVERY field of a usage record that has no column, not only the ones
// somebody happened to need. Four token counts have columns; what those counts
// count, where they came from, whether they contradict themselves, the total
// the upstream stated, and the tally of tool calls it ran on its own do not.
// The guarantee this row is written under is that the worst case is a fact
// without a column of its own — never a fact that disappeared.
type usageNotInColumns struct {
	fact.Base
	Unit                  string `json:"unit"`
	Source                string `json:"source"`
	Total                 int    `json:"total,omitempty"`
	CacheIncludedInPrompt bool   `json:"cache_included_in_prompt,omitempty"`
	Reasoning             int    `json:"reasoning,omitempty"`
	Incoherent            bool   `json:"incoherent,omitempty"`
	WebSearchCount        int    `json:"web_search_count,omitempty"`
	Count                 int    `json:"count,omitempty"`
	Requested             int    `json:"requested,omitempty"`
	Quality               string `json:"quality,omitempty"`
	Size                  string `json:"size,omitempty"`
}

func (usageNotInColumns) RecordName() string { return "usage_not_in_columns" }

// usageResidue reports what a usage record carries that this row has no column
// for, or nil when the columns hold all of it.
//
// Nil for the ordinary case — an upstream-reported, coherent, token-counted
// exchange with no provider-side tool calls and a total the columns already
// imply — so the common row does not carry a redundant second copy of numbers
// that are already in their own columns.
func usageResidue(rec fact.UsageReported) fact.Record {
	ordinary := rec.Unit == fact.UnitToken &&
		rec.Source == fact.UsageFromUpstream &&
		rec.WebSearchCount == 0 &&
		rec.Reasoning == 0 &&
		!rec.Incoherent &&
		!rec.CacheIncludedInPrompt &&
		rec.Total == rec.Prompt+rec.Completion+rec.CacheRead+rec.CacheWrite
	if ordinary {
		return nil
	}
	return usageNotInColumns{
		Unit:                  rec.Unit.String(),
		Source:                rec.Source.String(),
		Total:                 rec.Total,
		CacheIncludedInPrompt: rec.CacheIncludedInPrompt,
		Reasoning:             rec.Reasoning,
		Incoherent:            rec.Incoherent,
		WebSearchCount:        rec.WebSearchCount,
		Count:                 rec.Count,
		Requested:             rec.Requested,
		Quality:               rec.Quality,
		Size:                  rec.Size,
	}
}

// overflowEntry is one unrecognised record, kept under its stable name so a
// build that later grows a column for it can find what was already collected.
type overflowEntry struct {
	Attempt int         `json:"attempt"`
	Name    string      `json:"name"`
	Record  fact.Record `json:"record"`
}

// summarise reads the records this row has columns for and collects the rest.
//
// The default arm is the contract, not an oversight. A record this build has no
// column for must survive into the row anyway: dropping it is silent — the row
// still writes, just without the number — and leaves no way to tell an
// observation that never happened from one nobody made room for. Collecting it
// under its stable name turns a missing column into something an operator can
// find.
func summarise(tl fact.Timeline) summary {
	var s summary
	for _, e := range tl.All() {
		if e.Record == nil {
			continue // a routing fact; those steer the request, they are not columns
		}
		switch rec := e.Record.(type) {
		case fact.UsageReported:
			s.inputTokens = rec.Prompt
			s.outputTokens = rec.Completion
			s.cacheWriteTokens = rec.CacheWrite
			s.cacheReadTokens = rec.CacheRead
			// An image-unit report carries its quantity in Count; the column
			// is volume, so it is taken regardless of whether a price
			// resolved for it (the snapshot column is the one that exists
			// only when pricing succeeded).
			if rec.Unit == fact.UnitImage {
				s.imageCount = rec.Count
			}
			// A character-unit report follows the same volume rule, in the
			// settling candidate's own billing meter (the vendor's counting
			// rule) — the count is what the caller was metered on, priced or
			// not.
			if rec.Unit == fact.UnitCharacter {
				s.usageCharacters = rec.Count
			}
			// Recognising a record is not the same as having a column for all
			// of it. This row holds four token counts; the record also carries
			// what those counts COUNT, and a tally of provider-side tool calls
			// that is not a token count at all and is charged separately.
			//
			// Without this, being recognised is worse than being unknown: the
			// default branch below would have kept the whole record, and the
			// case above quietly drops whatever it did not copy. The residue
			// goes to the same place an unknown record would, so a build that
			// later grows a column can find what was already collected.
			if residue := usageResidue(rec); residue != nil {
				s.overflow = append(s.overflow, overflowEntry{
					Attempt: e.Attempt,
					Name:    residue.RecordName(),
					Record:  residue,
				})
			}
		case fact.CostComputed:
			s.costKnown = rec.Known
			s.costMicros = rec.Micros
			s.settledPrices = rec.Prices
			s.cacheReadSavedMicros = rec.CacheReadSavedMicros
			s.cacheWriteExtraMicros = rec.CacheWriteExtraMicros
			s.compressCostSavedMicros = rec.CompressCostSavedMicros
		case fact.TokensSaved:
			s.compressTokensSaved = rec.EstimatedTokens
			s.compressorsApplied = joinCompressors(rec.Compressors)
		case fact.CompressionSkipped:
			s.compressSkipReason = rec.Reason
		case fact.AttemptsRecorded:
			s.attempts = rec.Count
			s.attemptsDetail = rec.Detail
		default:
			s.overflow = append(s.overflow, overflowEntry{
				Attempt: e.Attempt,
				Name:    rec.RecordName(),
				Record:  rec,
			})
		}
	}
	return s
}

// encodeOverflow renders the unrecognised records for storage. A failure to
// encode is reported and the rest of the row is still written: losing the
// overflow is bad, losing the billing row with it would be worse.
func encodeOverflow(entries []overflowEntry, requestID string) string {
	if len(entries) == 0 {
		return ""
	}
	out, err := json.Marshal(entries)
	if err != nil {
		logger.Warn("gateway: could not encode unrecognised records",
			zap.String("request_id", requestID), zap.Error(err))
		return ""
	}
	return string(out)
}

// joinCompressors collapses the per-block list into the comma-joined string the
// column stores.
//
// Repeats are preserved, not deduped: the compress engine appends one entry per
// modified block, so a compressor appearing three times means it shrank three
// blocks, and that count is what downstream stats are counting.
func joinCompressors(applied []string) string {
	if len(applied) == 0 {
		return ""
	}
	parts := make([]string, 0, len(applied))
	for _, name := range applied {
		if name == "" {
			continue
		}
		parts = append(parts, name)
	}
	return strings.Join(parts, ",")
}
