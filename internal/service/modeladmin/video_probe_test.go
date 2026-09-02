package modeladmin_test

// The probe a video model gets is the video probe: a mapping that only
// serves video output runs a task conversation, and its capability flags
// describe behaviours it does not carry.

import (
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

func TestVideoModelProbesThroughTaskDialect(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	provider := seedEnabledProviderForModelTest(t, providerService, "video-probe-provider")
	now := time.Now().UTC()

	videoModel, err := svc.CreateModel(modeladmin.CreateModelInput{
		Name: "video-probe-model", OutputModalities: []string{model.OutputModalityVideo},
	}, now)
	if err != nil {
		t.Fatalf("create video model: %v", err)
	}

	basicBefore := client.CallCountFor("basic")
	videoBefore := client.CallCountFor("video")
	streamingBefore := client.CallCountFor("streaming")

	report, err := svc.TestAndCreateCandidate(t.Context(), videoModel.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "wan2.7-t2v",
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("test-and-create: %v", err)
	}
	if !report.Report.Basic.Passed() {
		t.Fatalf("basic probe did not pass: %+v", report.Report.Basic)
	}
	if got := client.CallCountFor("video") - videoBefore; got != 1 {
		t.Errorf("video probes = %d, want exactly 1", got)
	}
	if got := client.CallCountFor("basic") - basicBefore; got != 0 {
		t.Errorf("chat probes against a video model = %d, want 0", got)
	}
	if got := client.CallCountFor("streaming") - streamingBefore; got != 0 {
		t.Errorf("capability probes against a video model = %d, want 0", got)
	}
}
