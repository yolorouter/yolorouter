package modeladmin

// The standalone model probe: POST /providers/:id/models/probe. It answers
// one question an admin asks BEFORE anything is stored — "does this upstream
// model name work on this provider, and what is it good at" — and writes
// nothing: no model row, no candidate, no mapping. The verdicts live only in
// the response, which is why the vision arm exists only here: the stored
// candidate columns have no vision column, and a probe whose answer is never
// persisted must not spend image-sized upstream requests on paths (queue,
// retest) that would have nowhere to put them.

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/gateway"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/providerproto"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/providerclient"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// ProviderModelProbe is what the standalone probe endpoint returns: the same
// report shape the test-and-create flow shows, plus the vision arm's own
// verdict. The shared report structure is deliberately unchanged — the
// stored-candidate flows keep their three probes, and their consumers keep
// reading exactly those.
type ProviderModelProbe struct {
	Report CandidateTestReport `json:"report"`
	Vision ProbeReport         `json:"vision"`
}

// ProbeProviderModel resolves the provider and its highest-priority usable
// key, then probes the named upstream model — through the endpoint family
// the submitted output-modalities declaration says the model serves, and,
// for a chat-shaped model only, through one extra image-input request that
// answers whether the mapping accepts vision input.
//
// The result never touches the database. A provider id that does not exist
// is an error; a provider whose keys are all unusable is NOT — it comes back
// as a 200 carrying a failed basic report with the reason, the same
// presentation an upstream-refused probe gets, so the probe dialog renders
// one shape no matter which side refused the credential.
func (s *ModelService) ProbeProviderModel(ctx context.Context, providerID uint, providerModelName string, outputModalities []string) (ProviderModelProbe, error) {
	if len(outputModalities) > 0 {
		if _, err := model.CanonicalOutputModalities(outputModalities); err != nil {
			return ProviderModelProbe{}, fmt.Errorf("%w: %v", errcode.ErrModelOutputModalityInvalid, err)
		}
	}
	provider, err := repository.FindProviderByID(s.db, providerID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ProviderModelProbe{}, errcode.ErrProviderNotFound
		}
		return ProviderModelProbe{}, err
	}
	keys, err := repository.ListProviderKeysByProvider(s.db, providerID)
	if err != nil {
		return ProviderModelProbe{}, err
	}
	plaintext, err := s.decryptHighestPriorityAvailableKey(keys, provider.DestinationVersion)
	if err != nil {
		if errors.Is(err, errcode.ErrProviderNoTestableModel) {
			return probeWithoutCredential(err), nil
		}
		return ProviderModelProbe{}, err
	}

	shape := probeShapeForDeclaration(outputModalities)
	// Same rule as every other mapping probe: ride the provider's proxy
	// stance so what this measures is the channel routed traffic through the
	// provider actually uses.
	ctx = gateway.WithUpstreamProxyStance(ctx, gateway.UpstreamProxyStance{Mode: provider.ProxyMode, URL: provider.ProxyURL})
	proto := providerproto.TypeOf(provider.ProviderType)

	report, err := s.runCandidateProbes(ctx, proto, provider.BaseURL, plaintext, providerModelName, shape)
	if err != nil {
		return ProviderModelProbe{}, err
	}

	// The vision arm is this endpoint's exclusive: it runs only for a
	// chat-shaped model (a speech/image/video mapping carries no vision
	// question) and only once the basic probe proved the fundamentals —
	// a key that cannot complete a text request cannot answer a vision
	// question either, and spending the image request would only re-learn
	// the credential failure. The queue, the retest and the test-and-create
	// paths never reach here, so they never send an image request.
	var vision ProbeReport
	if shape == probeShapeChat && report.Basic.Passed() {
		vision = s.runVisionProbe(ctx, proto, provider.BaseURL, plaintext, providerModelName)
	}
	return ProviderModelProbe{Report: report, Vision: vision}, nil
}

// probeWithoutCredential presents a provider with no usable key the way an
// upstream-refused probe presents: a failed basic report carrying the reason
// verbatim, no candidate-shaped outcome code, and an untouched vision arm.
// The endpoint stays HTTP 200 — the request was answered; it is the
// provider's credential that is not usable, and the report is how this
// surface says so.
func probeWithoutCredential(err error) ProviderModelProbe {
	failed := false
	return ProviderModelProbe{
		Report: CandidateTestReport{
			Basic: ProbeReport{Ran: true, Supported: &failed, Detail: err.Error()},
		},
	}
}

// probeShapeForDeclaration maps a submitted output-modality declaration to
// the endpoint family the probe should speak. It mirrors the billing
// derivation's exclusive ladder — a mapping bills in one mode, so it must
// probe through the endpoint that mode's traffic actually reaches — with one
// deliberate divergence: audio is decided FIRST, because the exclusive
// helpers overlap on a hand-written ["image","audio"] declaration
// (OutputImageExclusive and OutputAudioExclusive are both true there) and
// the billing derivation resolves that overlap to audio by checking audio
// last. Deciding audio first lands the same verdict, and keeps this mapping
// from repeating the shared probe switch's ordering, which checks image
// first and would probe a billing-audio mapping through the images endpoint.
//
//   - any audio with no text (["audio"], ["image","audio"]) → speech,
//     because that mapping bills per character and serves a speech dialect;
//   - video alone → video, the per-second task dialect;
//   - image with no text (and no audio, already claimed above) → images;
//   - anything containing text, or nothing declared at all → chat: a model
//     that also serves text keeps chat semantics, and an undeclared model
//     gets the chat probe every mapping predating the column was verified
//     through.
func probeShapeForDeclaration(list []string) probeShape {
	var text, image, video, audio bool
	for _, id := range list {
		switch id {
		case model.OutputModalityText:
			text = true
		case model.OutputModalityImage:
			image = true
		case model.OutputModalityVideo:
			video = true
		case model.OutputModalityAudio:
			audio = true
		}
	}
	switch {
	case !text && audio:
		return probeShapeAudio
	case video && len(list) == 1: // the OutputVideoExclusive reading: video and nothing else
		return probeShapeVideo
	case !text && image:
		return probeShapeImage
	default:
		return probeShapeChat
	}
}

// runVisionProbe sends the one image-input request and reduces the answer to
// a ProbeReport under the vision whitelist:
//
//   - a certified completion → Supported=true: the model accepted an image
//     and answered it, which is the whole question;
//   - a model-not-found or a request-shape rejection (400/415) →
//     Supported=false: against a basic probe that just passed, these are the
//     upstream saying "not this model here" or "I do not take image parts" —
//     decisive negatives;
//   - everything else (auth, quota, rate limits, 5xx, unparseable 200s) →
//     Supported=null: those answers describe the provider's health or the
//     credential, not the vision capability, and the conservative reading
//     leaves the verdict unclaimed rather than guessing;
//   - a client timeout → Ran=false: no verdict was obtained, and "the
//     upstream never answered" must not read as an answer.
func (s *ModelService) runVisionProbe(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, providerModelName string) ProbeReport {
	result, err := s.client.TestImageInputChat(ctx, proto, baseURL, apiKey, providerModelName)
	if err != nil {
		// Same rule as the capability probes: an error here means the probe
		// never reached the upstream (saturated test semaphore, cancelled
		// request). Reporting it as having run would present "the provider
		// gave no clear answer" when the provider was never asked.
		return ProbeReport{Ran: false, Supported: nil, DurationMs: result.DurationMs}
	}
	if result.Outcome == providerclient.TestTimeout {
		return ProbeReport{Ran: false, Supported: nil, DurationMs: result.DurationMs, Detail: result.Detail}
	}
	outcome := int(result.Outcome)
	return ProbeReport{
		Ran:        true,
		Supported:  classifyVisionResult(result),
		Outcome:    &outcome,
		DurationMs: result.DurationMs,
		Detail:     result.Detail,
	}
}

// classifyVisionResult reduces an image-input probe result to the tri-state
// verdict the whitelist above spells out.
func classifyVisionResult(result providerclient.TestResult) *bool {
	switch result.Outcome {
	case providerclient.TestSuccess:
		supported := true
		return &supported
	case providerclient.TestModelNotFound, providerclient.TestRequestRejected:
		supported := false
		return &supported
	default:
		return nil
	}
}
