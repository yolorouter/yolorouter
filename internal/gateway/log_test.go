package gateway

import (
	"testing"

	"github.com/yolorouter/yolorouter-ce/internal/model"
)

func TestComputeCost(t *testing.T) {
	// Candidate prices are CNY per million tokens (design doc §3.3).
	// 1M input @ 1.0 + 0.5M output @ 2.0 = 1.0 + 1.0 = 2.0 CNY = 200 cents.
	cand := &model.ModelCandidate{InputPrice: 1.0, OutputPrice: 2.0}
	usage := &Usage{PromptTokens: 1_000_000, CompletionTokens: 500_000}
	cents, known := computeCost(cand, usage)
	if !known {
		t.Fatal("expected cost to be known when usage + candidate present")
	}
	if cents != 200 {
		t.Fatalf("cost = %d cents, want 200", cents)
	}
}

func TestComputeCostRoundsToCent(t *testing.T) {
	// 1 token @ 1.0/M = 0.000001 CNY = 0.0001 cents -> rounds to 0 cents.
	cand := &model.ModelCandidate{InputPrice: 1.0, OutputPrice: 0}
	usage := &Usage{PromptTokens: 1, CompletionTokens: 0}
	cents, known := computeCost(cand, usage)
	if !known || cents != 0 {
		t.Fatalf("expected known 0 cents, got %d (known=%v)", cents, known)
	}
}

func TestComputeCostMissingUsageIsUnknown(t *testing.T) {
	// GATE-21: missing usage must be "unknown", never 0 cost.
	cand := &model.ModelCandidate{InputPrice: 1.0, OutputPrice: 1.0}
	if cents, known := computeCost(cand, nil); known || cents != 0 {
		t.Fatalf("expected unknown/0 for nil usage, got %d (known=%v)", cents, known)
	}
}

func TestComputeCostMissingCandidateIsUnknown(t *testing.T) {
	usage := &Usage{PromptTokens: 100, CompletionTokens: 100}
	if cents, known := computeCost(nil, usage); known || cents != 0 {
		t.Fatalf("expected unknown/0 for nil candidate, got %d (known=%v)", cents, known)
	}
}

func TestGenerateRequestIDUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := generateRequestID()
		if len(id) != 16 {
			t.Fatalf("id length = %d, want 16 (id=%q)", len(id), id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id generated: %q", id)
		}
		seen[id] = struct{}{}
	}
}
