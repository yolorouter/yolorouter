package modeladmin_test

// The standalone probe endpoint's service contract: the shape declaration
// picks the endpoint family (with [image,audio] resolving to speech, the
// overlap the stored-candidate probe switch gets backwards), the vision arm
// runs only for chat shapes and only on this path, the vision verdict reads
// through the conservative whitelist, and nothing — no model row, no
// candidate row, no probe debt — is ever written.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/internal/service/provider"
	"github.com/yolorouter/yolorouter/internal/service/providerclient"
	"github.com/yolorouter/yolorouter/internal/service/providerclient/providerclienttest"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// newProbeTestService builds a model service on its own fake (distinct from
// the provider service's, so key-verification traffic during setup never
// lands in the counts the assertions read).
func newProbeTestService(t *testing.T, providerName string) (svc *modeladmin.ModelService, db *gorm.DB, client *providerclienttest.Fake, providerID uint) {
	t.Helper()
	providerService, gdb, _ := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, providerName)
	client = &providerclienttest.Fake{Result: providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}}
	return modeladmin.NewModelService(gdb, testutil.ProviderSecrets(), client), gdb, client, prov.ID
}

func TestProbeProviderModelShapeFollowsTheDeclaration(t *testing.T) {
	tests := []struct {
		name string
		// declaration is the submitted output_modalities list.
		declaration []string
		// wantProbe is the probe kind the declaration must route to
		// ("basic" for the chat family, "speech"/"image"/"video" otherwise).
		wantProbe string
		// wantVision says whether the image-input arm may run.
		wantVision bool
	}{
		{"no declaration probes chat", nil, "basic", true},
		{"text-only declaration probes chat", []string{model.OutputModalityText}, "basic", true},
		{"text beside image stays chat", []string{model.OutputModalityText, model.OutputModalityImage}, "basic", true},
		{"audio-only declaration probes speech", []string{model.OutputModalityAudio}, "speech", false},
		{"image beside audio probes speech, not images", []string{model.OutputModalityImage, model.OutputModalityAudio}, "speech", false},
		{"image-only declaration probes images", []string{model.OutputModalityImage}, "image", false},
		{"video-only declaration probes video", []string{model.OutputModalityVideo}, "video", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, client, providerID := newProbeTestService(t, "shape-probe-provider")

			result, err := svc.ProbeProviderModel(t.Context(), providerID, "upstream-model", tt.declaration)
			if err != nil {
				t.Fatalf("ProbeProviderModel: %v", err)
			}
			if got := client.CallCountFor(tt.wantProbe); got != 1 {
				t.Errorf("%s probes = %d, want exactly 1", tt.wantProbe, got)
			}
			if got := client.CallCountFor("vision"); got != boolCount(tt.wantVision) {
				t.Errorf("vision probes = %d, want %d", got, boolCount(tt.wantVision))
			}
			// The basic report always carries the verdict of the probe that
			// ran, whichever family it spoke.
			if !result.Report.Basic.Passed() {
				t.Errorf("basic report did not pass: %+v", result.Report.Basic)
			}
			for _, other := range []string{"basic", "speech", "image", "video"} {
				if other == tt.wantProbe {
					continue
				}
				if got := client.CallCountFor(other); got != 0 {
					t.Errorf("%s probes = %d, want 0", other, got)
				}
			}
		})
	}
}

func boolCount(b bool) int {
	if b {
		return 1
	}
	return 0
}

func boolPtr(b bool) *bool { return &b }

// The image+audio overlap is the one place this mapping must disagree with
// the stored-candidate probe switch: the billing derivation settles that
// declaration on audio, so the probe has to ask the speech endpoint, not
// the images one.
func TestProbeProviderModelImageAudioOverlapLandsSpeech(t *testing.T) {
	svc, _, client, providerID := newProbeTestService(t, "overlap-probe-provider")

	if _, err := svc.ProbeProviderModel(t.Context(), providerID, "upstream-model",
		[]string{model.OutputModalityImage, model.OutputModalityAudio}); err != nil {
		t.Fatalf("ProbeProviderModel: %v", err)
	}
	if got := client.CallCountFor("image"); got != 0 {
		t.Errorf("image probes against an audio-billed declaration = %d, want 0", got)
	}
	if got := client.CallCountFor("speech"); got != 1 {
		t.Errorf("speech probes = %d, want exactly 1", got)
	}
}

// The vision arm is gated on the basic probe: a credential that cannot
// complete a text request cannot answer a vision question either, and the
// image request would only re-learn the failure.
func TestProbeProviderModelVisionDoesNotRunWhenBasicFails(t *testing.T) {
	svc, _, client, providerID := newProbeTestService(t, "vision-gate-provider")
	client.PerTestType = map[string]providerclienttest.TargetResponse{
		"basic": {Result: providerclient.TestResult{Outcome: providerclient.TestAuthFailed, DurationMs: 5, Detail: "HTTP 401: bad key"}},
	}

	result, err := svc.ProbeProviderModel(t.Context(), providerID, "upstream-model", nil)
	if err != nil {
		t.Fatalf("ProbeProviderModel with a failing basic probe: %v", err)
	}
	if result.Report.Basic.Passed() {
		t.Fatalf("basic report passed, want the seeded auth failure: %+v", result.Report.Basic)
	}
	if got := client.CallCountFor("vision"); got != 0 {
		t.Errorf("vision probes after a failed basic = %d, want 0", got)
	}
	if result.Vision.Ran {
		t.Errorf("vision report claims it ran despite the basic failure: %+v", result.Vision)
	}
}

func TestProbeProviderModelVisionVerdictFollowsTheWhitelist(t *testing.T) {
	tests := []struct {
		name string
		// vision is what the image-input probe answers.
		vision providerclient.TestResult
		// clientErr makes the probe return an error instead of a result
		// (a saturated test semaphore, a cancelled call).
		clientErr   error
		wantRan     bool
		wantSupport *bool
		// wantDetailIn, when set, must appear in the vision detail.
		wantDetailIn string
	}{
		{
			name:        "acceptance proves vision",
			vision:      providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 9},
			wantRan:     true,
			wantSupport: boolPtr(true),
		},
		{
			name:        "model not found is a decisive no",
			vision:      providerclient.TestResult{Outcome: providerclient.TestModelNotFound, DurationMs: 9, Detail: "HTTP 404"},
			wantRan:     true,
			wantSupport: boolPtr(false),
		},
		{
			name:         "request-shape rejection is a decisive no",
			vision:       providerclient.TestResult{Outcome: providerclient.TestRequestRejected, DurationMs: 9, Detail: "HTTP 400: image input is not supported"},
			wantRan:      true,
			wantSupport:  boolPtr(false),
			wantDetailIn: "image input is not supported",
		},
		{
			name:        "auth failure stays inconclusive",
			vision:      providerclient.TestResult{Outcome: providerclient.TestAuthFailed, DurationMs: 9},
			wantRan:     true,
			wantSupport: nil,
		},
		{
			name:        "rate limiting stays inconclusive",
			vision:      providerclient.TestResult{Outcome: providerclient.TestRateLimited, DurationMs: 9},
			wantRan:     true,
			wantSupport: nil,
		},
		{
			name:        "an upstream error stays inconclusive",
			vision:      providerclient.TestResult{Outcome: providerclient.TestUpstreamError, DurationMs: 9},
			wantRan:     true,
			wantSupport: nil,
		},
		{
			name:        "a client timeout never ran",
			vision:      providerclient.TestResult{Outcome: providerclient.TestTimeout, DurationMs: 60000, Detail: "response incomplete after 60000ms"},
			wantRan:     false,
			wantSupport: nil,
		},
		{
			name:      "a client-side error never ran",
			vision:    providerclient.TestResult{},
			clientErr: errors.New("too many concurrent provider test calls in flight"),
			wantRan:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, client, providerID := newProbeTestService(t, "vision-verdict-provider")
			client.PerTestType = map[string]providerclienttest.TargetResponse{
				"vision": {Result: tt.vision, Err: tt.clientErr},
			}

			result, err := svc.ProbeProviderModel(t.Context(), providerID, "upstream-model", nil)
			if err != nil {
				t.Fatalf("ProbeProviderModel: %v", err)
			}
			if result.Vision.Ran != tt.wantRan {
				t.Errorf("vision.Ran = %v, want %v (report %+v)", result.Vision.Ran, tt.wantRan, result.Vision)
			}
			switch {
			case tt.wantSupport == nil && result.Vision.Supported != nil:
				t.Errorf("vision.Supported = %v, want nil", *result.Vision.Supported)
			case tt.wantSupport != nil && result.Vision.Supported == nil:
				t.Errorf("vision.Supported = nil, want %v", *tt.wantSupport)
			case tt.wantSupport != nil && *result.Vision.Supported != *tt.wantSupport:
				t.Errorf("vision.Supported = %v, want %v", *result.Vision.Supported, *tt.wantSupport)
			}
			if tt.wantDetailIn != "" && !strings.Contains(result.Vision.Detail, tt.wantDetailIn) {
				t.Errorf("vision.Detail = %q, want it to carry %q", result.Vision.Detail, tt.wantDetailIn)
			}
		})
	}
}

// A provider whose keys are all unusable answers as a failed basic report,
// not an error: the probe dialog renders one shape no matter which side
// refused the credential.
func TestProbeProviderModelUnusableKeyIsABasicFailureNotAnError(t *testing.T) {
	// The setup fake refuses the credential check, so the provider ends up
	// without a single usable key.
	providerService, gdb, setupClient := newTestProviderService(t)
	setupClient.PerTestType = map[string]providerclienttest.TargetResponse{
		"basic": {Result: providerclient.TestResult{Outcome: providerclient.TestAuthFailed, DurationMs: 5, Detail: "HTTP 401: invalid api key"}},
	}
	prov, err := providerService.CreateProvider(t.Context(), provider.CreateProviderInput{
		Name: "no-key-probe-provider", BaseURL: "https://no-key-probe-provider.example.com",
		KeyLabel: "k1", KeyPlaintext: "sk-abcdefghijklmnopqrstuvwxyz1234", TestModel: "gpt-4o-mini",
		ManagementStatus: model.ProviderStatusEnabled,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	client := &providerclienttest.Fake{Result: providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}}
	svc := modeladmin.NewModelService(gdb, testutil.ProviderSecrets(), client)

	result, err := svc.ProbeProviderModel(t.Context(), prov.ID, "upstream-model", nil)
	if err != nil {
		t.Fatalf("a provider with no usable key must answer as a report, got error: %v", err)
	}
	if !result.Report.Basic.Ran || result.Report.Basic.Supported == nil || *result.Report.Basic.Supported {
		t.Errorf("basic report = %+v, want a failed basic", result.Report.Basic)
	}
	if result.Report.Basic.Detail == "" {
		t.Errorf("basic report carries no detail for the credential failure")
	}
	if result.Vision.Ran {
		t.Errorf("vision arm ran despite the missing credential: %+v", result.Vision)
	}
	if got := client.CallCountFor("basic"); got != 0 {
		t.Errorf("upstream probes sent with no usable key = %d, want 0", got)
	}
}

// An upstream that rejects the credential mid-probe lands in the report the
// same way, with the upstream's own message as the detail.
func TestProbeProviderModelUpstreamAuthFailureIsAReportNotAnHTTPError(t *testing.T) {
	svc, _, client, providerID := newProbeTestService(t, "auth-fail-probe-provider")
	client.PerTestType = map[string]providerclienttest.TargetResponse{
		"basic": {Result: providerclient.TestResult{Outcome: providerclient.TestAuthFailed, DurationMs: 5, Detail: "HTTP 401: invalid api key"}},
	}

	result, err := svc.ProbeProviderModel(t.Context(), providerID, "upstream-model", nil)
	if err != nil {
		t.Fatalf("an upstream credential refusal must ride the report, got error: %v", err)
	}
	if result.Report.Basic.Passed() {
		t.Errorf("basic report should read as failed: %+v", result.Report.Basic)
	}
	if !strings.Contains(result.Report.Basic.Detail, "HTTP 401") {
		t.Errorf("basic detail = %q, want the upstream diagnostic", result.Report.Basic.Detail)
	}
}

func TestProbeProviderModelRejectsUnknownModalityAndUnknownProvider(t *testing.T) {
	svc, _, _, providerID := newProbeTestService(t, "validation-probe-provider")

	if _, err := svc.ProbeProviderModel(t.Context(), providerID, "m", []string{"banana"}); !errors.Is(err, errcode.ErrModelOutputModalityInvalid) {
		t.Errorf("unknown modality id: err = %v, want ErrModelOutputModalityInvalid", err)
	}
	if _, err := svc.ProbeProviderModel(t.Context(), providerID, "m", []string{model.OutputModalityImage, model.OutputModalityImage}); !errors.Is(err, errcode.ErrModelOutputModalityInvalid) {
		t.Errorf("duplicate modality id: err = %v, want ErrModelOutputModalityInvalid", err)
	}
	if _, err := svc.ProbeProviderModel(t.Context(), 999999, "m", nil); !errors.Is(err, errcode.ErrProviderNotFound) {
		t.Errorf("unknown provider: err = %v, want ErrProviderNotFound", err)
	}
}

// The endpoint's promise: probing stores nothing. No model row, no
// candidate row, before or after.
func TestProbeProviderModelWritesNothing(t *testing.T) {
	svc, gdb, _, providerID := newProbeTestService(t, "write-nothing-probe-provider")

	if _, err := svc.ProbeProviderModel(t.Context(), providerID, "brand-new-upstream-model",
		[]string{model.OutputModalityText, model.OutputModalityImage}); err != nil {
		t.Fatalf("ProbeProviderModel: %v", err)
	}

	models, err := svc.ListModels()
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 0 {
		t.Errorf("probe left %d model rows, want 0", len(models))
	}
	var candidates int64
	if err := gdb.Model(&model.ModelCandidate{}).Count(&candidates).Error; err != nil {
		t.Fatalf("count candidates: %v", err)
	}
	if candidates != 0 {
		t.Errorf("probe left %d candidate rows, want 0", candidates)
	}
}

// The vision arm and the images probe belong to the standalone endpoint
// alone. The background queue and the manual retest verify stored candidates
// through their stored shapes' own probes, and neither may ever spend an
// image request — there is no vision column to put a verdict in, and an
// image generation against a chat mapping would measure nothing the row
// records.
func TestQueueAndRetestPathsNeverSendImageRequests(t *testing.T) {
	providerService, gdb, _ := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "no-image-path-provider")
	seedSvc := modeladmin.NewModelService(gdb, testutil.ProviderSecrets(), &providerclienttest.Fake{})
	id := seedUntestedCandidate(t, seedSvc, gdb, prov.ID, "queue-vision-model")

	client := &providerclienttest.Fake{Result: providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 5}}
	svc := modeladmin.NewModelService(gdb, testutil.ProviderSecrets(), client)

	queue := modeladmin.NewProbeQueue(svc, 1)
	queue.Start(t.Context())
	defer queue.Stop()
	queue.Enqueue(id)
	waitFor(t, "the queued probe to commit", func() bool {
		c := loadCandidate(t, gdb, id)
		return c.LastTestedAt != nil
	})
	assertNoImageTraffic(t, client, "the queue path")

	if _, _, err := svc.RetestModelCandidate(t.Context(), id, time.Now().UTC()); err != nil {
		t.Fatalf("RetestModelCandidate: %v", err)
	}
	assertNoImageTraffic(t, client, "the retest path")
	if got := client.CallCountFor("basic"); got < 2 {
		t.Errorf("chat probes across queue+retest = %d, want at least 2 (both paths must have actually run)", got)
	}
}

func assertNoImageTraffic(t *testing.T, client *providerclienttest.Fake, path string) {
	t.Helper()
	for _, kind := range []string{"image", "vision"} {
		if got := client.CallCountFor(kind); got != 0 {
			t.Errorf("%s requests on %s = %d, want 0", kind, path, got)
		}
	}
}
