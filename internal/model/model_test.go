package model

import "testing"

func TestModelTableName(t *testing.T) {
	if (Model{}).TableName() != "models" {
		t.Fatalf("expected table name 'models', got %q", (Model{}).TableName())
	}
}

func TestModelCandidateTableName(t *testing.T) {
	if (ModelCandidate{}).TableName() != "model_candidates" {
		t.Fatalf("expected table name 'model_candidates', got %q", (ModelCandidate{}).TableName())
	}
}

func TestModelStatusConstantsAreDistinct(t *testing.T) {
	if ModelStatusEnabled == ModelStatusDisabled {
		t.Fatalf("ModelStatusEnabled and ModelStatusDisabled must differ")
	}
	if ModelCandidateStatusEnabled == ModelCandidateStatusDisabled {
		t.Fatalf("ModelCandidateStatusEnabled and ModelCandidateStatusDisabled must differ")
	}
}
