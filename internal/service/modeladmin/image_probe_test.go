package modeladmin_test

// The probe an image model gets is the image probe: a mapping that only
// serves image output cannot be verified by a chat request, and its
// capability flags describe behaviours it does not carry.

import (
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

func TestImageModelProbesThroughImageEndpoint(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	provider := seedEnabledProviderForModelTest(t, providerService, "image-probe-provider")
	now := time.Now().UTC()

	imageModel, err := svc.CreateModel(modeladmin.CreateModelInput{
		Name: "image-probe-model", OutputModalities: []string{model.OutputModalityImage},
	}, now)
	if err != nil {
		t.Fatalf("create image model: %v", err)
	}
	textModel, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "text-probe-model"}, now)
	if err != nil {
		t.Fatalf("create text model: %v", err)
	}

	basicBefore := client.CallCountFor("basic")
	imageBefore := client.CallCountFor("image")
	streamingBefore := client.CallCountFor("streaming")

	report, err := svc.TestAndCreateCandidate(t.Context(), imageModel.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "wan2.2-image",
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now)
	if err != nil {
		t.Fatalf("test-and-create: %v", err)
	}
	if !report.Report.Basic.Passed() {
		t.Fatalf("basic probe did not pass: %+v", report.Report.Basic)
	}
	if got := client.CallCountFor("image") - imageBefore; got != 1 {
		t.Errorf("image probes = %d, want exactly 1", got)
	}
	if got := client.CallCountFor("basic") - basicBefore; got != 0 {
		t.Errorf("chat probes against an image model = %d, want 0", got)
	}
	if got := client.CallCountFor("streaming") - streamingBefore; got != 0 {
		t.Errorf("capability probes against an image model = %d, want 0", got)
	}

	// A text model keeps the chat probe and its capability pair.
	if _, err := svc.TestAndCreateCandidate(t.Context(), textModel.ID, modeladmin.CreateCandidateInput{
		ProviderID: provider.ID, ProviderModelName: "gpt-4o",
		ManagementStatus: model.ModelCandidateStatusEnabled,
	}, now); err != nil {
		t.Fatalf("text test-and-create: %v", err)
	}
	if got := client.CallCountFor("basic") - basicBefore; got != 1 {
		t.Errorf("chat probes for the text model = %d, want 1", got)
	}
	if got := client.CallCountFor("image") - imageBefore; got != 1 {
		t.Errorf("image probes total = %d, want still 1", got)
	}
}
