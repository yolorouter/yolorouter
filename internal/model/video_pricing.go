package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The video billing declaration: per-second pricing keyed by resolution
// tier. The stored shape is the one the shared admin frontend's editor
// produces — {tiers:[{resolution,purchase_price,sell_price}]} — so the
// backend accepts it verbatim rather than inventing a second vocabulary.
// SellPrice is what a caller is charged per delivered second;
// PurchasePrice is the operator's own cost reference and prices nothing.

// VideoPricingTier is one row of a per-second price table. An empty
// Resolution is the generic tier: what a request prices at when its
// resolution matches no named tier.
type VideoPricingTier struct {
	Resolution    string  `json:"resolution"`
	PurchasePrice float64 `json:"purchase_price"`
	SellPrice     float64 `json:"sell_price"`
}

// VideoPricingTiers is a candidate's per-second video price table.
type VideoPricingTiers struct {
	Tiers []VideoPricingTier `json:"tiers"`
}

// ParseVideoPricingTiers reads the stored declaration. Lenient by design:
// an empty or malformed value reads as "no table", which prices nothing
// rather than failing a request — validation happened once, at save time.
func ParseVideoPricingTiers(raw string) *VideoPricingTiers {
	if raw == "" {
		return nil
	}
	var tiers VideoPricingTiers
	if err := json.Unmarshal([]byte(raw), &tiers); err != nil {
		return nil
	}
	return &tiers
}

// ValidateVideoPricingTiers is the write-path check: at least one tier
// (a video-mode candidate without a table has nothing to price a second
// with), no negative price, and no blank resolution string other than the
// generic tier's own — a tier keyed " " can never match and is a typo.
func ValidateVideoPricingTiers(t *VideoPricingTiers) error {
	if t == nil {
		return errors.New("video pricing tiers must not be empty when submitted")
	}
	if len(t.Tiers) == 0 {
		return errors.New("video pricing tiers need at least one tier")
	}
	for _, tier := range t.Tiers {
		if tier.Resolution != "" && strings.TrimSpace(tier.Resolution) == "" {
			return errors.New("tier resolution must be a name or empty for the generic tier, not whitespace")
		}
		if tier.SellPrice < 0 {
			return fmt.Errorf("tier sell price must not be negative (resolution=%q)", tier.Resolution)
		}
		if tier.PurchasePrice < 0 {
			return fmt.Errorf("tier purchase price must not be negative (resolution=%q)", tier.Resolution)
		}
	}
	return nil
}

// MarshalVideoPricingTiers validates and serializes a declaration for
// storage, so a row can only ever hold a table that prices.
func MarshalVideoPricingTiers(t *VideoPricingTiers) (string, error) {
	if err := ValidateVideoPricingTiers(t); err != nil {
		return "", err
	}
	out, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ResolveSellPrice returns the per-second sell price for one resolution,
// and whether any tier answered. A named tier matches its resolution
// exactly; the generic tier (empty resolution) answers everything else.
// First match wins, so a table that lists the generic tier before a named
// one has decided what it wants. No match is reported as ok=false rather
// than a zero — unpriced and free are different facts, and settlement
// must not mistake one for the other.
func (t *VideoPricingTiers) ResolveSellPrice(resolution string) (price float64, ok bool) {
	if t == nil {
		return 0, false
	}
	for _, tier := range t.Tiers {
		if tier.Resolution == "" || tier.Resolution == resolution {
			return tier.SellPrice, true
		}
	}
	return 0, false
}
