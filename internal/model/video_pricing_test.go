package model

import (
	"strings"
	"testing"
)

func TestVideoPricingValidation(t *testing.T) {
	if err := ValidateVideoPricingTiers(nil); err == nil {
		t.Fatal("nil table must be refused")
	}
	if err := ValidateVideoPricingTiers(&VideoPricingTiers{}); err == nil {
		t.Fatal("empty table must be refused — a video-mode candidate with no tier prices nothing")
	}
	if err := ValidateVideoPricingTiers(&VideoPricingTiers{Tiers: []VideoPricingTier{
		{Resolution: "720P", SellPrice: -0.1, PurchasePrice: 0},
	}}); err == nil {
		t.Fatal("negative sell price must be refused")
	}
	if err := ValidateVideoPricingTiers(&VideoPricingTiers{Tiers: []VideoPricingTier{
		{Resolution: "720P", SellPrice: 0.6, PurchasePrice: -1},
	}}); err == nil {
		t.Fatal("negative purchase price must be refused")
	}
	if err := ValidateVideoPricingTiers(&VideoPricingTiers{Tiers: []VideoPricingTier{
		{Resolution: " ", SellPrice: 1},
	}}); err == nil {
		t.Fatal("a whitespace resolution is a typo of the generic tier, not a key")
	}
	// Zero sell price is a free tier, a legal declaration.
	if err := ValidateVideoPricingTiers(&VideoPricingTiers{Tiers: []VideoPricingTier{
		{Resolution: "720P", SellPrice: 0},
	}}); err != nil {
		t.Fatalf("a free tier is legal: %v", err)
	}
}

func TestVideoPricingMarshalRoundTrip(t *testing.T) {
	raw, err := MarshalVideoPricingTiers(&VideoPricingTiers{Tiers: []VideoPricingTier{
		{Resolution: "", PurchasePrice: 0.4, SellPrice: 0.5},
		{Resolution: "1080P", PurchasePrice: 0.8, SellPrice: 1.0},
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The stored shape is the frontend editor's own field names.
	for _, want := range []string{`"resolution"`, `"purchase_price"`, `"sell_price"`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("stored form must keep the editor's field names, missing %s in %s", want, raw)
		}
	}
	parsed := ParseVideoPricingTiers(raw)
	if parsed == nil || len(parsed.Tiers) != 2 {
		t.Fatalf("round trip lost tiers: %+v", parsed)
	}
	if ParseVideoPricingTiers("") != nil || ParseVideoPricingTiers("{bad") != nil {
		t.Fatal("empty and malformed stored values read as no table, not as errors")
	}
}

func TestResolveSellPrice(t *testing.T) {
	table := &VideoPricingTiers{Tiers: []VideoPricingTier{
		{Resolution: "720P", SellPrice: 0.5},
		{Resolution: "1080P", SellPrice: 1.0},
		{Resolution: "", SellPrice: 0.3},
	}}
	if price, ok := table.ResolveSellPrice("720P"); !ok || price != 0.5 {
		t.Fatalf("named tier must price its own resolution, got %v %v", price, ok)
	}
	if price, ok := table.ResolveSellPrice("1080P"); !ok || price != 1.0 {
		t.Fatalf("1080P tier must price, got %v %v", price, ok)
	}
	if price, ok := table.ResolveSellPrice("480P"); !ok || price != 0.3 {
		t.Fatalf("unlisted resolution must fall to the generic tier, got %v %v", price, ok)
	}
	strict := &VideoPricingTiers{Tiers: []VideoPricingTier{
		{Resolution: "1080P", SellPrice: 1.0},
	}}
	if _, ok := strict.ResolveSellPrice("720P"); ok {
		t.Fatal("no match and no generic tier is unpriced, not free")
	}
	var nilTable *VideoPricingTiers
	if _, ok := nilTable.ResolveSellPrice("720P"); ok {
		t.Fatal("a nil table prices nothing")
	}
}

func TestNormalizeBillingModeIncludesVideo(t *testing.T) {
	if NormalizeBillingMode("video") != BillingModeVideo {
		t.Fatal("video must survive normalization")
	}
	if NormalizeBillingMode("") != BillingModeToken || NormalizeBillingMode("nonsense") != BillingModeToken {
		t.Fatal("empty and unknown must read as the token default")
	}
}
