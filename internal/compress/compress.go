package compress

import (
	"context"
	"encoding/json"

	"github.com/yolorouter/yolorouter/internal/compress/compressors"
	"github.com/yolorouter/yolorouter/internal/protocols"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// Pre-allocated compressor chains. Every compressor is a stateless zero-byte
// struct, so the chains are reused across requests without allocation.
var (
	// Build-output candidates are ordered most-specific first: each framework
	// compressor only acts on output it recognizes (returning foreign content
	// unchanged, which the accept gate rejects), so the first one that
	// actually shrinks the block wins and the generic log pass stays last.
	buildCompressors = []compressors.Compressor{
		&compressors.GoTest{},
		&compressors.Pytest{},
		&compressors.Vitest{},
		&compressors.Npm{},
		&compressors.Pnpm{},
		&compressors.Log{},
	}
	diffCompressors   = []compressors.Compressor{&compressors.Diff{}}
	searchCompressors = []compressors.Compressor{&compressors.Grep{}}
)

// compressorsFor returns the compressor chain for a content type. The chain
// is tried in order and the first compressor that yields a shorter output
// wins for that block. A nil return means the content type has no compressor
// (a real coverage gap that callers surface as NoMatchingCompressor).
func compressorsFor(ct ContentType) []compressors.Compressor {
	switch ct {
	case ContentBuildOutput:
		return buildCompressors
	case ContentGitDiff:
		return diffCompressors
	case ContentSearchResults:
		return searchCompressors
	default:
		return nil
	}
}

// ByProtocol compresses a request body using the protocol's live-zone
// locator from the table below. An unrecognized protocol returns the body
// unchanged with a no-op result (Skipped=true) so an unknown ingress never
// breaks a caller.
func ByProtocol(ctx context.Context, proto protocols.ProtocolID, body []byte, opts CompressOptions) (out []byte, res CompressResult) {
	locate, ok := liveZoneLocators[proto]
	if !ok {
		return body, CompressResult{Skipped: true, SkipReason: SkipReasonNoLiveZone}
	}
	return runCompress(ctx, body, opts, locate)
}

// liveZoneLocators maps each wire protocol to its live-zone locator — the
// only per-protocol fact in this package. Adding a protocol here is what
// makes ByProtocol cover it.
var liveZoneLocators = map[protocols.ProtocolID]func([]byte) []LiveBlock{
	protocols.ProtocolOpenAI:    locateChatLiveZone,
	protocols.ProtocolClaude:    locateClaudeLiveZone,
	protocols.ProtocolResponses: locateResponsesLiveZone,
	protocols.ProtocolGemini:    locateGeminiLiveZone,
}

// runCompress is the shared orchestration for every protocol entrypoint:
// locate live blocks -> size gate -> detect content type -> run compressors
// -> accept gate -> splice. The whole pass is wrapped in a recover so a panic
// or timeout fails open by returning the original body untouched.
func runCompress(ctx context.Context, body []byte, opts CompressOptions, locate func([]byte) []LiveBlock) (out []byte, res CompressResult) {
	out = body
	defer func() {
		if r := recover(); r != nil {
			out = body
			res = CompressResult{Skipped: true, SkipReason: SkipReasonFailOpen}
		}
	}()

	// Validate up front: a body that fails json.Valid cannot be located
	// reliably, and a nil locate result would otherwise be indistinguishable
	// from a well-formed request with no live zone.
	if !json.Valid(body) {
		return body, CompressResult{Skipped: true, SkipReason: SkipReasonParseError}
	}
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	blocks := locate(body)
	if len(blocks) == 0 {
		return body, CompressResult{Skipped: true, SkipReason: SkipReasonNoLiveZone}
	}

	// Total live-zone byte budget: exceeding it skips the whole request so a
	// pathological input cannot stall the relay.
	total := 0
	for _, b := range blocks {
		total += len(b.Text)
	}
	if total > opts.MaxLiveZoneBytes {
		return body, CompressResult{Skipped: true, SkipReason: SkipReasonTotalSizeExceeded}
	}

	var reps []Replacement
	var applied []string
	saved := 0
	// sawNoCompressor distinguishes "live blocks exist but their content type
	// has no compressor" (a real coverage gap) from "compressors ran but every
	// block was too small or did not shrink".
	sawNoCompressor := false
	for _, blk := range blocks {
		select {
		case <-ctx.Done():
			return body, CompressResult{Skipped: true, SkipReason: SkipReasonTimeout}
		default:
		}
		if !shouldAttempt(len(blk.Text), opts.MinBlockBytes) {
			continue
		}
		ct := detectContentType(blk.Text)
		cs := compressorsFor(ct)
		if cs == nil {
			sawNoCompressor = true
			continue
		}
		compressed := blk.Text
		var name string
		for _, c := range cs {
			r, err := c.Compress(ctx, blk.Text)
			if err != nil {
				// A dead context is not a candidate failure: continuing
				// would misreport the ending as "nothing shrank", and in a
				// multi-block pass could splice the EARLIER blocks'
				// replacements into a body the deadline already gave up on.
				// Timeout fails open whole: original body, timeout reason.
				if ctx.Err() != nil {
					return body, CompressResult{Skipped: true, SkipReason: SkipReasonTimeout}
				}
				continue
			}
			if acceptCompressed(blk.Text, r) {
				compressed, name = r, c.Name()
				break
			}
		}
		if name == "" {
			continue // no compressor shrank this block; leave it untouched
		}
		enc, err := jsonMarshal(compressed)
		if err != nil {
			continue
		}
		// Encoding byte gate: acceptCompressed compares decoded lengths; this
		// second gate compares the JSON-encoded replacement against the
		// original quoted-literal byte range so an escaped form that is not
		// actually shorter is rejected too.
		if len(enc) >= blk.Range[1]-blk.Range[0] {
			continue
		}
		reps = append(reps, Replacement{Range: blk.Range, Replacement: enc})
		applied = append(applied, name)
		saved += estimateTokensSaved(len(blk.Text), len(compressed))
	}

	// Final deadline check before committing: replacements collected before
	// a cancellation must not be spliced into a partially-processed body.
	if ctx.Err() != nil {
		return body, CompressResult{Skipped: true, SkipReason: SkipReasonTimeout}
	}
	if len(reps) == 0 {
		reason := SkipReasonNoEffectiveReplacement
		if sawNoCompressor {
			// A coverage gap takes precedence: it tells the operator which
			// content type still needs a compressor, which is actionable
			// unlike "nothing shrank".
			reason = SkipReasonNoMatchingCompressor
		}
		return body, CompressResult{Skipped: true, SkipReason: reason}
	}
	return SpliceBody(body, reps), CompressResult{
		EstimatedTokensSaved: saved,
		CompressorsApplied:   applied,
		BlocksModified:       len(reps),
	}
}
