package repository

import (
	"context"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
)

// Cache visibility aggregation — the platform totals behind the dashboard's
// verified cache KPI cards. Per-dimension cache figures live on the report
// aggregations instead (ReportTokenCost carries the same settled sums per
// row), so this file only answers the platform-level question.
//
// Every figure here is read from columns settled at write time
// (cache_*_tokens, cache_read_saved_micros, cache_write_extra_micros) — the
// aggregation never re-prices anything, so the numbers a window showed once
// are the numbers it shows forever, whatever happens to the price list later.
//
// Only cache-capable providers participate in the totals. A provider that
// never reports cache metering would pour its uncached input into the
// hit-rate denominator and dilute the platform figure toward a fake 0%; one
// without a configured cache price bills cache tokens at the input price and
// has no cache economics to show. Both are surfaced as explicitly
// unsupported instead of silently flattening the numbers.

// CacheUnsupportedNoMetering / CacheUnsupportedNoCachePrice are the stable
// reason codes on an unsupported-provider row. Metering wins when both
// apply — without metering the price question never arises.
const (
	CacheUnsupportedNoMetering   = "no_cache_metering"
	CacheUnsupportedNoCachePrice = "no_cache_price"
)

// CacheTotals is the token and money roll-up over the cache-capable
// providers' rows in the window. UncachedInputTokens is SUM(input_tokens),
// which the gateway persists as the NET (cache-excluded) input count across
// every protocol — so hit rate = read ÷ (read + write + uncached) needs no
// per-protocol correction here. Sums, not averaged ratios, so the client's
// division is inherently token-weighted.
//
// CacheReadSavedMicros and CacheWriteExtraMicros are both non-negative per
// row; the net saving (read − write) is derived by the reader and is allowed
// to be negative — a window that wrote much and read little must show it.
type CacheTotals struct {
	CacheReadTokens       int64 `json:"cache_read_tokens" gorm:"column:cache_read_tokens"`
	CacheWriteTokens      int64 `json:"cache_write_tokens" gorm:"column:cache_write_tokens"`
	UncachedInputTokens   int64 `json:"uncached_input_tokens" gorm:"column:uncached_input_tokens"`
	CacheReadSavedMicros  int64 `json:"cache_read_saved_micros" gorm:"column:cache_read_saved_micros"`
	CacheWriteExtraMicros int64 `json:"cache_write_extra_micros" gorm:"column:cache_write_extra_micros"`
}

// CacheUnsupportedProviderRow names one provider with traffic in the window
// whose rows are excluded from the cache totals, and why. ProviderName is
// resolved post-fetch; a since-deleted provider keeps its id with an empty
// name rather than vanishing from the disclosure.
type CacheUnsupportedProviderRow struct {
	ProviderID   uint   `json:"provider_id"`
	ProviderName string `json:"provider_name"`
	Reason       string `json:"reason"`
}

// CacheStats is the AggregateCacheStats result: the platform totals plus the
// per-provider unsupported disclosure.
type CacheStats struct {
	Totals               CacheTotals                   `json:"totals"`
	UnsupportedProviders []CacheUnsupportedProviderRow `json:"unsupported_providers"`
}

// AggregateCacheStats returns the cache visibility roll-up for the filter
// window: totals over the cache-capable providers' rows, and the explicit
// list of providers whose traffic was excluded. Providers with no traffic in
// the window appear nowhere — there is nothing to aggregate and nothing to
// disclaim.
func AggregateCacheStats(ctx context.Context, db *gorm.DB, f *RequestLogFilter) (*CacheStats, error) {
	supported, unsupported, err := classifyCacheProviders(ctx, db, f)
	if err != nil {
		return nil, err
	}
	stats := &CacheStats{UnsupportedProviders: unsupported}
	if len(supported) == 0 {
		return stats, nil
	}
	err = f.applyFilter(db.WithContext(ctx)).Select(`
		COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens,
		COALESCE(SUM(cache_write_tokens), 0) AS cache_write_tokens,
		COALESCE(SUM(input_tokens), 0) AS uncached_input_tokens,
		COALESCE(SUM(cache_read_saved_micros), 0) AS cache_read_saved_micros,
		COALESCE(SUM(cache_write_extra_micros), 0) AS cache_write_extra_micros
	`[1:]).Where("provider_id IN ?", supported).Scan(&stats.Totals).Error
	if err != nil {
		return nil, err
	}
	return stats, nil
}

// classifyCacheProviders splits the providers with traffic in the window into
// cache-capable ids and unsupported rows. Capable = the provider has EVER
// persisted a cache token count (the upstream meters its cache) AND at least
// one of its candidates has a configured cache price (the price list
// distinguishes cache from input).
//
// The metering evidence deliberately ignores the window: a capable provider
// whose selected window happens to contain zero cache activity must stay in
// the totals — dropping it would remove its uncached input from the hit-rate
// denominator, inflating the platform figure and making a true 0%-hit window
// impossible to display. Lifetime evidence is also the strongest signal the
// persisted data can carry at all: rows store token counts, not wire-field
// presence, and on some upstream protocols a reported zero and an absent
// field are the same bare int on the wire — so "has ever reported a nonzero
// count" is the one metering fact that can be judged honestly. It remains
// data-driven: the day an upstream starts reporting cache fields, its first
// cached request flips the provider into the totals without a release.
func classifyCacheProviders(ctx context.Context, db *gorm.DB, f *RequestLogFilter) ([]uint, []CacheUnsupportedProviderRow, error) {
	var trafficIDs []uint
	err := f.applyFilter(db.WithContext(ctx)).
		Where("provider_id IS NOT NULL").
		Distinct().Order("provider_id ASC").Pluck("provider_id", &trafficIDs).Error
	if err != nil {
		return nil, nil, err
	}
	if len(trafficIDs) == 0 {
		return nil, []CacheUnsupportedProviderRow{}, nil
	}

	// Lifetime metering evidence, unfiltered on purpose (see above). Scoped
	// to the providers with window traffic so the scan stays bounded by the
	// ids the caller is actually asking about.
	var meteredIDs []uint
	err = db.WithContext(ctx).Model(&model.RequestLog{}).
		Where("provider_id IN ?", trafficIDs).
		Where("cache_read_tokens > 0 OR cache_write_tokens > 0").
		Distinct().Pluck("provider_id", &meteredIDs).Error
	if err != nil {
		return nil, nil, err
	}
	metered := make(map[uint]bool, len(meteredIDs))
	for _, id := range meteredIDs {
		metered[id] = true
	}

	// Price capability, from two sources OR'd together. The candidate check
	// answers "is a cache price configured NOW" (actionable for a provider
	// that has never settled a saving); the settled-savings check answers
	// "did the price list ever distinguish cache from input", read from the
	// same write-time columns the totals sum. The second source is what
	// keeps history stable: clearing a cache price today must not
	// retroactively flip past windows to "unsupported" and silently change
	// a KPI someone already read.
	var pricedIDs []uint
	err = db.WithContext(ctx).Model(&model.ModelCandidate{}).
		Where("cache_read_price IS NOT NULL OR cache_write_price IS NOT NULL").
		Distinct().Pluck("provider_id", &pricedIDs).Error
	if err != nil {
		return nil, nil, err
	}
	var settledIDs []uint
	err = db.WithContext(ctx).Model(&model.RequestLog{}).
		Where("provider_id IN ?", trafficIDs).
		Where("cache_read_saved_micros != 0 OR cache_write_extra_micros != 0").
		Distinct().Pluck("provider_id", &settledIDs).Error
	if err != nil {
		return nil, nil, err
	}
	priced := make(map[uint]bool, len(pricedIDs)+len(settledIDs))
	for _, id := range pricedIDs {
		priced[id] = true
	}
	for _, id := range settledIDs {
		priced[id] = true
	}

	var supported []uint
	unsupported := []CacheUnsupportedProviderRow{}
	for _, id := range trafficIDs {
		switch {
		case !metered[id]:
			unsupported = append(unsupported, CacheUnsupportedProviderRow{
				ProviderID: id, Reason: CacheUnsupportedNoMetering})
		case !priced[id]:
			unsupported = append(unsupported, CacheUnsupportedProviderRow{
				ProviderID: id, Reason: CacheUnsupportedNoCachePrice})
		default:
			supported = append(supported, id)
		}
	}
	if err := resolveCacheProviderNames(db, unsupported); err != nil {
		return nil, nil, err
	}
	return supported, unsupported, nil
}

// resolveCacheProviderNames fills ProviderName via the shared batched
// lookup. A since-deleted provider simply keeps an empty name.
func resolveCacheProviderNames(db *gorm.DB, rows []CacheUnsupportedProviderRow) error {
	return backfillByPtrID(db, rows,
		func(r *CacheUnsupportedProviderRow) *uint { return &r.ProviderID },
		FindProviderNamesByIDs,
		func(r *CacheUnsupportedProviderRow, name string) { r.ProviderName = name })
}
