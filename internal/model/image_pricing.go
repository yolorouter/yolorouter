package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The billing modes a candidate may declare. The empty value is the token
// default every candidate had before the column existed, so a row that
// predates it keeps its meaning without a backfill.
const (
	BillingModeToken = "token"
	BillingModeImage = "image"
)

// ValidBillingModes is the write-path vocabulary.
var ValidBillingModes = []string{BillingModeToken, BillingModeImage}

// NormalizeBillingMode maps a stored billing_mode onto the vocabulary: the
// empty pre-migration value reads as the token default, and an unknown value
// reads as token too — the hot path must always have a usable answer, and
// pricing a malformed declaration as tokens undercharges visibly rather than
// guessing at a table that was never parsed.
func NormalizeBillingMode(mode string) string {
	if mode == BillingModeImage {
		return BillingModeImage
	}
	return BillingModeToken
}

// ImagePricingTier is one row of a per-image price table: the price of ONE
// image at this quality and size. An empty Quality or Size is a wildcard —
// it matches any request value — and within the table the first match wins,
// so the order tiers are listed in is the order they are consulted in.
type ImagePricingTier struct {
	Quality string  `json:"quality"`
	Size    string  `json:"size"`
	Price   float64 `json:"price"`
}

// ImagePricingTiers is a candidate's per-image price table.
//
// Mode says what the price is per; only per_image exists today, and the
// field exists so a future mode is a new value rather than a new column
// shape. DefaultPrice is what a request prices at when no tier matched:
// absent, a request that matches nothing is unpriced (cost unknown), which
// is deliberately not the same as free.
type ImagePricingTiers struct {
	Mode         string             `json:"mode"`
	Tiers        []ImagePricingTier `json:"tiers"`
	DefaultPrice *float64           `json:"default_price,omitempty"`
}

// perImageMode is the one pricing mode this build prices.
const perImageMode = "per_image"

// ParseImagePricingTiers reads the stored declaration. Lenient by design: an
// empty or malformed value reads as "no table", which prices nothing rather
// than failing a request — validation happened once, at save time.
func ParseImagePricingTiers(raw string) *ImagePricingTiers {
	if raw == "" {
		return nil
	}
	var tiers ImagePricingTiers
	if err := json.Unmarshal([]byte(raw), &tiers); err != nil {
		return nil
	}
	if tiers.Mode == "" {
		tiers.Mode = perImageMode
	}
	return &tiers
}

// ValidateImagePricingTiers is the write-path check: mode must name a
// pricing this build knows, and every price must be non-negative — zero is a
// free image and is a legal declaration, a negative one is a typo that would
// bill backwards.
func ValidateImagePricingTiers(t *ImagePricingTiers) error {
	if t == nil {
		return errors.New("image pricing tiers must not be empty when submitted")
	}
	if t.Mode == "" {
		t.Mode = perImageMode
	}
	if t.Mode != perImageMode {
		return fmt.Errorf("unknown image pricing mode %q", t.Mode)
	}
	if len(t.Tiers) == 0 && t.DefaultPrice == nil {
		return errors.New("image pricing tiers need at least one tier or a default price")
	}
	for _, tier := range t.Tiers {
		if tier.Price < 0 {
			return fmt.Errorf("tier price must not be negative (quality=%q size=%q)", tier.Quality, tier.Size)
		}
	}
	if t.DefaultPrice != nil && *t.DefaultPrice < 0 {
		return errors.New("default price must not be negative")
	}
	return nil
}

// MarshalImagePricingTiers validates and serializes a declaration for
// storage, so a row can only ever hold a table that prices.
func MarshalImagePricingTiers(t *ImagePricingTiers) (string, error) {
	if err := ValidateImagePricingTiers(t); err != nil {
		return "", err
	}
	out, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// Price source names, as a settlement's snapshot records them.
const (
	PriceSourceTier    = "tier"
	PriceSourceDefault = "default"
)

// normalizeSizeAxis folds the two separator spellings the ecosystem uses —
// OpenAI clients send "1024x1024" while some vendors' native docs spell the
// star form — so a tier configured in either form matches a request phrased
// in the other. Only the axis separators fold; the digits and any unit
// suffix pass through unchanged. The protocol layer keeps a byte-identical
// fold for the same reason on its side of the wire boundary (ConvertSize in
// internal/protocols/images): this package deliberately imports nothing
// from the protocol packages, so the rule is written twice and the two
// comments point at each other — change one, change both.
func normalizeSizeAxis(size string) string {
	return strings.ReplaceAll(strings.ReplaceAll(size, "x", "*"), "X", "*")
}

// ResolvePrice returns the per-image price for one request's quality and
// size, and which entry of the table answered — the snapshot reports the
// source so "which line of my table priced this" has one answer, computed by
// the same walk that priced it. First matching tier wins; an empty tier axis
// is a wildcard; the size axis compares through normalizeSizeAxis; a table
// with no match and no default has no answer, and no answer is reported as
// ok=false rather than a zero — unpriced and free are different facts, and
// settlement must not mistake one for the other.
func (t *ImagePricingTiers) ResolvePrice(quality, size string) (price float64, source string, ok bool) {
	if t == nil {
		return 0, "", false
	}
	for _, tier := range t.Tiers {
		if tier.Quality != "" && tier.Quality != quality {
			continue
		}
		if tier.Size != "" && normalizeSizeAxis(tier.Size) != normalizeSizeAxis(size) {
			continue
		}
		return tier.Price, PriceSourceTier, true
	}
	if t.DefaultPrice != nil {
		return *t.DefaultPrice, PriceSourceDefault, true
	}
	return 0, "", false
}
