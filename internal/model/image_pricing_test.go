package model

import "testing"

// The tier table's resolve rule: first match wins, an empty axis is a
// wildcard, no match falls to the default, and a table with neither answer
// has no price — which is not zero.
func TestImagePricingTiersResolvePrice(t *testing.T) {
	high := 0.04
	low := 0.01
	table := &ImagePricingTiers{
		Mode: "per_image",
		Tiers: []ImagePricingTier{
			{Quality: "high", Size: "1024*1024", Price: 0.19},
			{Quality: "high", Price: 0.11}, // size wildcard
			{Price: 0.05},                  // both wildcard
		},
		DefaultPrice: &low,
	}
	cases := []struct {
		name          string
		quality, size string
		want          float64
		wantSource    string
	}{
		{"x-spelled request against star-spelled tier", "high", "1024x1024", 0.19, PriceSourceTier},
		{"uppercase separator folds too", "high", "1024X1024", 0.19, PriceSourceTier},
		{"size wildcard", "high", "1536x1024", 0.11, PriceSourceTier},
		{"both wildcard", "medium", "512x512", 0.05, PriceSourceTier},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, source, ok := table.ResolvePrice(tc.quality, tc.size)
			if !ok || got != tc.want || source != tc.wantSource {
				t.Fatalf("ResolvePrice(%q,%q) = %v,%v,%v want %v,%v,true", tc.quality, tc.size, got, source, ok, tc.want, tc.wantSource)
			}
		})
	}

	t.Run("default answers when no tier matches", func(t *testing.T) {
		withDefault := &ImagePricingTiers{
			Tiers:        []ImagePricingTier{{Quality: "high", Price: high}},
			DefaultPrice: &low,
		}
		if got, source, ok := withDefault.ResolvePrice("unheardof", "4096x4096"); !ok || got != low || source != PriceSourceDefault {
			t.Fatalf("got %v,%v,%v want the default %v via default", got, source, ok, low)
		}
	})

	t.Run("first match wins over a later wildcard", func(t *testing.T) {
		ordered := &ImagePricingTiers{Tiers: []ImagePricingTier{{Quality: "high", Price: 0.19}, {Price: 0.05}}}
		got, source, ok := ordered.ResolvePrice("high", "")
		if !ok || got != 0.19 || source != PriceSourceTier {
			t.Fatalf("got %v,%v,%v want the first tier 0.19 via tier", got, source, ok)
		}
	})

	t.Run("no match and no default has no price", func(t *testing.T) {
		bare := &ImagePricingTiers{Tiers: []ImagePricingTier{{Quality: "high", Price: high}}}
		if got, _, ok := bare.ResolvePrice("low", ""); ok {
			t.Fatalf("got %v,true want no answer: unpriced is not free", got)
		}
	})

	t.Run("nil table has no price", func(t *testing.T) {
		var nilTable *ImagePricingTiers
		if _, _, ok := nilTable.ResolvePrice("", ""); ok {
			t.Fatal("nil table resolved a price")
		}
	})
}

// The write path's validation: a table that prices is accepted (including a
// free one — zero is a legal price), a table that cannot price is rejected.
func TestImagePricingTiersValidate(t *testing.T) {
	free := 0.0
	cases := []struct {
		name    string
		table   *ImagePricingTiers
		wantErr bool
	}{
		{"tier table", &ImagePricingTiers{Tiers: []ImagePricingTier{{Quality: "high", Price: 0.19}}}, false},
		{"default only", &ImagePricingTiers{DefaultPrice: &free}, false},
		{"free tier is legal", &ImagePricingTiers{Tiers: []ImagePricingTier{{Quality: "free", Price: free}}}, false},
		{"empty is not a table", &ImagePricingTiers{}, true},
		{"nil is not a table", nil, true},
		{"negative price", &ImagePricingTiers{Tiers: []ImagePricingTier{{Price: -0.01}}}, true},
		{"negative default", &ImagePricingTiers{Tiers: []ImagePricingTier{{Price: 0.01}}, DefaultPrice: &free}, false},
		{"unknown mode", &ImagePricingTiers{Mode: "per_pixel", Tiers: []ImagePricingTier{{Price: 1}}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateImagePricingTiers(tc.table)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}

	t.Run("marshal validates and round-trips", func(t *testing.T) {
		in := &ImagePricingTiers{Tiers: []ImagePricingTier{{Quality: "high", Size: "1024x1024", Price: 0.19}}, DefaultPrice: &free}
		raw, err := MarshalImagePricingTiers(in)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		back := ParseImagePricingTiers(raw)
		if back == nil || len(back.Tiers) != 1 || back.Tiers[0].Price != 0.19 {
			t.Fatalf("round trip lost the table: %+v", back)
		}
		if got, source, ok := back.ResolvePrice("high", "1024x1024"); !ok || got != 0.19 || source != PriceSourceTier {
			t.Fatalf("round-tripped table does not resolve: %v,%v,%v", got, source, ok)
		}
	})

	t.Run("parse is lenient on garbage", func(t *testing.T) {
		if ParseImagePricingTiers("") != nil {
			t.Fatal("empty parsed to a table")
		}
		if ParseImagePricingTiers("not-json") != nil {
			t.Fatal("garbage parsed to a table")
		}
	})
}

// The stored billing mode reads through one normalization: anything that is
// not image is token, so a malformed value degrades to the default rather
// than to a third, undefined mode.
func TestNormalizeBillingMode(t *testing.T) {
	for mode, want := range map[string]string{"": BillingModeToken, "token": BillingModeToken, "image": BillingModeImage, "bogus": BillingModeToken} {
		if got := NormalizeBillingMode(mode); got != want {
			t.Errorf("NormalizeBillingMode(%q) = %q, want %q", mode, got, want)
		}
	}
}
