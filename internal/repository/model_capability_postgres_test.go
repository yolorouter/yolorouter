package repository

import (
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
)

// The capability-metadata columns' Postgres arm. Every repository test runs
// on SQLite; this is the one that drives the five new models columns —
// including the DOUBLE PRECISION temperature and the BOOLEAN tri-states,
// whose GORM binding shapes differ between the backends — against a real
// Postgres. Gated on TEST_POSTGRES_DSN like the admin-audit Postgres suite
// above.

func TestModelCapabilityMetadataRoundTripOnPostgres(t *testing.T) {
	db := newPostgresRepositoryDB(t)
	now := time.Now().UTC()

	m := &model.Model{
		Name: "cap-pg", ManagementStatus: model.ModelStatusEnabled,
		SchedulingMode: model.ModelSchedulingModeFailover, OutputModalities: model.DefaultOutputModalitiesJSON,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := CreateModel(db, m); err != nil {
		t.Fatalf("create: %v", err)
	}

	// A fresh row reads all-undeclared — the five columns round-trip NULLs.
	fresh, err := FindModelByID(db, m.ID)
	if err != nil {
		t.Fatalf("read fresh: %v", err)
	}
	if fresh.ContextLength != nil || fresh.SupportsTools != nil || fresh.SupportsReasoning != nil ||
		fresh.Temperature != nil || //nolint:staticcheck // SA1019: asserting the deprecated mirror column is the test's purpose
		fresh.SupportsImageInput != nil || fresh.InputModalityList() != nil {
		t.Fatalf("fresh row must read all-undeclared, got %+v", fresh)
	}

	// Declare everything, then read it back through the same GORM mapping
	// the runtime uses.
	ctxLen := 1000000
	tools := true
	reasoning := false
	// 0.705 is deliberate: three decimals pin the float64-faithful caliber.
	// A scaled NUMERIC column would round it to 0.71 on this backend while
	// the SQLite arm kept the exact bits — the two trees must agree, and the
	// exact != below is what proves it.
	temp := 0.705
	if err := UpdateModel(db, m.ID, ModelUpdate{
		Name: m.Name, Status: m.ManagementStatus,
		ContextLengthSet: true, ContextLength: &ctxLen,
		SupportsToolsSet: true, SupportsTools: &tools,
		SupportsReasoningSet: true, SupportsReasoning: &reasoning,
		TemperatureSet: true, Temperature: &temp,
		InputModalitiesSet: true, InputModalities: `["text","image"]`,
	}, now); err != nil {
		t.Fatalf("declare: %v", err)
	}
	declared, err := FindModelByID(db, m.ID)
	if err != nil {
		t.Fatalf("read declared: %v", err)
	}
	if declared.ContextLength == nil || *declared.ContextLength != ctxLen {
		t.Fatalf("context_length = %v, want %d", declared.ContextLength, ctxLen)
	}
	if declared.SupportsTools == nil || !*declared.SupportsTools {
		t.Fatalf("supports_tools = %v, want true", declared.SupportsTools)
	}
	if declared.SupportsReasoning == nil || *declared.SupportsReasoning {
		t.Fatalf("supports_reasoning = %v, want false", declared.SupportsReasoning)
	}
	if declared.Temperature == nil || *declared.Temperature != temp {
		t.Fatalf("temperature = %v, want %v (DOUBLE PRECISION bit-exact round-trip)", declared.Temperature, temp)
	}
	if list := declared.InputModalityList(); len(list) != 2 || list[1] != "image" {
		t.Fatalf("input_modalities read as %v, want [text image]", list)
	}
	// The mirror landed in the same statement on this backend too.
	//nolint:staticcheck // SA1019: asserting the deprecated mirror column is the test's purpose
	if declared.SupportsImageInput == nil || !*declared.SupportsImageInput {
		t.Fatalf("supports_image_input = %v, want the derived true", declared.SupportsImageInput)
	}

	// Clear everything back to undeclared — nil must survive the write, not
	// collapse to a zero value.
	if err := UpdateModel(db, m.ID, ModelUpdate{
		Name: m.Name, Status: m.ManagementStatus,
		ContextLengthSet: true, SupportsToolsSet: true, SupportsReasoningSet: true, TemperatureSet: true,
		InputModalitiesSet: true, InputModalities: "",
	}, now); err != nil {
		t.Fatalf("clear: %v", err)
	}
	cleared, err := FindModelByID(db, m.ID)
	if err != nil {
		t.Fatalf("read cleared: %v", err)
	}
	if cleared.ContextLength != nil || cleared.SupportsTools != nil || cleared.SupportsReasoning != nil ||
		cleared.Temperature != nil || //nolint:staticcheck // SA1019: asserting the deprecated mirror column is the test's purpose
		cleared.SupportsImageInput != nil || cleared.InputModalityList() != nil {
		t.Fatalf("cleared row must read all-undeclared on Postgres, got %+v", cleared)
	}
}
