package gateway

// Per-character settlement tests. The dispatch's unit guard is the answered
// form of the gap fake_modality_audio_test.go used to pin: a character count
// reaches token math only over a misdeclared candidate, and the honest bill
// for that wiring bug is unknown, not a per-million-TOKEN figure.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/model"
)

func audioPrice(v float64) *float64 { return &v }

// TestAudioSettlementPricesCharacterCountsByBillingMode is the completion of
// the old pinned gap: the SAME character report settles as a billed character
// count on an audio-mode candidate and as unknown on a token-mode one — the
// unit now decides, not the shape of the count.
func TestAudioSettlementPricesCharacterCountsByBillingMode(t *testing.T) {
	report := &fact.UsageReported{Unit: fact.UnitCharacter, Source: fact.UsageFromUpstream, Count: 120}

	audioCand := &model.ModelCandidate{BillingMode: model.BillingModeAudio, AudioUnitPrice: audioPrice(350)}
	settled := computeSettlementCost(audioCand, report, nil, 0)
	if !settled.Known {
		t.Fatal("an audio-mode candidate with a price left a character report unpriced")
	}
	// 120 characters at CNY 350 per million: the per-million divisor and the
	// micros multiplier cancel, so the product already is micros.
	if want := int64(350 * 120); settled.CostMicros != want {
		t.Errorf("micros = %d, want %d", settled.CostMicros, want)
	}

	tokenCand := &model.ModelCandidate{BillingMode: model.BillingModeToken, InputPrice: 4, OutputPrice: 4}
	if settled := computeSettlementCost(tokenCand, report, nil, 0); settled.Known {
		t.Errorf("a character count settled as known on a token-mode candidate (micros=%d): "+
			"pricing characters at a per-million-token rate is wrong money in a shape nobody would notice",
			settled.CostMicros)
	}
	if settled := computeSettlementCost(nil, report, nil, 0); settled.Known {
		t.Error("a character count settled as known with no candidate at all")
	}
}

func TestComputeAudioCost(t *testing.T) {
	base := model.ModelCandidate{BillingMode: model.BillingModeAudio, AudioUnitPrice: audioPrice(2)}
	cases := []struct {
		name   string
		cand   *model.ModelCandidate
		rpt    *fact.UsageReported
		want   int64
		priced bool
	}{
		{"nil report", &base, nil, 0, false},
		{"zero count", &base, &fact.UsageReported{Unit: fact.UnitCharacter, Count: 0}, 0, false},
		{"token-unit report on audio mode", &base, &fact.UsageReported{Unit: fact.UnitToken, Count: 5}, 0, false},
		{"unpriced candidate", &model.ModelCandidate{BillingMode: model.BillingModeAudio}, &fact.UsageReported{Unit: fact.UnitCharacter, Count: 5}, 0, false},
		{"free is a price, not a gap", &model.ModelCandidate{BillingMode: model.BillingModeAudio, AudioUnitPrice: audioPrice(0)}, &fact.UsageReported{Unit: fact.UnitCharacter, Count: 5}, 0, true},
		{"negative price is not billable", &model.ModelCandidate{BillingMode: model.BillingModeAudio, AudioUnitPrice: audioPrice(-1)}, &fact.UsageReported{Unit: fact.UnitCharacter, Count: 5}, 0, false},
		{"vendor meter, not runes", &model.ModelCandidate{BillingMode: model.BillingModeAudio, AudioUnitPrice: audioPrice(200)}, &fact.UsageReported{Unit: fact.UnitCharacter, Count: 240}, 200 * 240, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeAudioCost(tc.cand, tc.rpt)
			if got.Known != tc.priced {
				t.Fatalf("known = %v, want %v", got.Known, tc.priced)
			}
			if got.CostMicros != tc.want {
				t.Errorf("micros = %d, want %d", got.CostMicros, tc.want)
			}
		})
	}
}

func TestComputeAudioCostRoundsHalfUp(t *testing.T) {
	// A price whose product lands on a half micro must round up, matching the
	// token math's convention: 1 character at CNY 1.5 per million is 1.5
	// micros, and half-up bills it as 2.
	got := computeAudioCost(
		&model.ModelCandidate{BillingMode: model.BillingModeAudio, AudioUnitPrice: audioPrice(1.5)},
		&fact.UsageReported{Unit: fact.UnitCharacter, Count: 1},
	)
	if !got.Known || got.CostMicros != 2 {
		t.Errorf("micros = %d known=%v, want 2 known=true (1.5 rounds up)", got.CostMicros, got.Known)
	}
}

// The snapshot is the bill's own explanation and is built from the same numbers
// the micros were — the test pins both halves agreeing.
func TestComputeAudioCostSnapshotCarriesMeterAndPrice(t *testing.T) {
	got := computeAudioCost(
		&model.ModelCandidate{BillingMode: model.BillingModeAudio, AudioUnitPrice: audioPrice(350)},
		&fact.UsageReported{Unit: fact.UnitCharacter, Source: fact.UsageFromUpstream, Count: 240},
	)
	if !got.Known || got.AudioSnapshot == "" {
		t.Fatalf("known=%v snapshot=%q, want a priced settlement with a snapshot", got.Known, got.AudioSnapshot)
	}
	var snap map[string]any
	if err := json.Unmarshal([]byte(got.AudioSnapshot), &snap); err != nil {
		t.Fatalf("snapshot did not parse: %v (%s)", err, got.AudioSnapshot)
	}
	for key, want := range map[string]string{
		"billing_mode": model.BillingModeAudio,
		"unit":         fact.UnitCharacter.String(),
		"source":       fact.UsageFromUpstream.String(),
	} {
		if snap[key] != want {
			t.Errorf("snapshot[%q] = %v, want %q", key, snap[key], want)
		}
	}
	if snap["characters"].(float64) != 240 {
		t.Errorf("snapshot characters = %v, want 240 (the vendor meter, not the rune count)", snap["characters"])
	}
	if snap["unit_price"].(float64) != 350 {
		t.Errorf("snapshot unit_price = %v, want 350", snap["unit_price"])
	}
	if strings.Contains(got.AudioSnapshot, "prompt_tokens") {
		t.Error("audio snapshot carries token vocabulary; it should speak only in characters")
	}
}

// The overflow guard: a coherent count against an absurd unit price must
// settle as unknown rather than as whatever the platform's float→int64
// conversion produces (the token math guards the same way).
func TestComputeAudioCostSettlesUnknownWhenProductOverflows(t *testing.T) {
	got := computeAudioCost(
		&model.ModelCandidate{BillingMode: model.BillingModeAudio, AudioUnitPrice: audioPrice(1e308)},
		&fact.UsageReported{Unit: fact.UnitCharacter, Count: 240},
	)
	if got.Known {
		t.Fatalf("overflowing product settled as known with micros=%d", got.CostMicros)
	}
}

// One text, three meters: the same input bills a different Count per vendor
// counting rule, and settlement prices whatever the meter said — it never
// re-counts the text. The three first-batch vendors, one table.
func TestComputeAudioCostPricesTheVendorMeterNotTheText(t *testing.T) {
	// "你好世界hello": 4 CJK glyphs + 5 ASCII chars.
	cases := []struct {
		vendor string
		meter  int // the vendor's own count of that text
		price  float64
	}{
		{"minimax counts a CJK glyph as two characters", 4*2 + 5, 350},
		{"siliconflow counts UTF-8 bytes", 4*3 + 5, 50},
		{"zhipu counts characters", 4 + 5, 200},
	}
	for _, tc := range cases {
		t.Run(tc.vendor, func(t *testing.T) {
			got := computeAudioCost(
				&model.ModelCandidate{BillingMode: model.BillingModeAudio, AudioUnitPrice: audioPrice(tc.price)},
				&fact.UsageReported{Unit: fact.UnitCharacter, Source: fact.UsageFromUpstream, Count: tc.meter},
			)
			if !got.Known || got.CostMicros != int64(tc.price*float64(tc.meter)+0.5) {
				t.Fatalf("micros = %d known=%v; settlement must price the meter count (%d), not re-count the text",
					got.CostMicros, got.Known, tc.meter)
			}
		})
	}
}

// The counted-unit guard is symmetric: a character report must not price as
// images any more than it prices as tokens — a default tier would otherwise
// happily bill it per "image".
func TestCharacterCountDoesNotPriceAsImages(t *testing.T) {
	def := 0.5
	tiers, err := model.MarshalImagePricingTiers(&model.ImagePricingTiers{DefaultPrice: &def})
	if err != nil {
		t.Fatalf("marshal tiers: %v", err)
	}
	imageCand := &model.ModelCandidate{BillingMode: model.BillingModeImage, ImagePricingTiers: tiers}
	report := &fact.UsageReported{Unit: fact.UnitCharacter, Source: fact.UsageFromUpstream, Count: 3}
	if settled := computeSettlementCost(imageCand, report, nil, 0); settled.Known {
		t.Fatalf("a character count settled as known on an image-mode candidate (micros=%d)", settled.CostMicros)
	}
}
