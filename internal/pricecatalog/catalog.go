// Package pricecatalog holds the built-in seed of canonical model prices,
// embedded into the binary via catalog.json. It is the fallback source for
// auto-suggesting candidate prices when no historical price exists for a
// provider+model pair (see modeladmin.SuggestCandidatePrice).
//
// Prices follow the provider, not the vendor: the key is the provider's
// base_url host, so an aggregator (e.g. SiliconFlow) carries its own resale
// prices for models it fronts. All values are CNY per million tokens, matching
// internal/gateway/log.go's computeCost.
package pricecatalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

//go:embed catalog.json
var catalogBytes []byte

// The unit contract the figures are billed under. catalog.json is refreshed by
// hand from vendor pricing pages, which quote USD and per-1K rates just as
// often as the pair used here, so the file states which one it holds and the
// loader refuses anything else. Without this a refresh in the wrong unit would
// flow through Lookup into a candidate's price columns and be billed as if it
// were CNY per million — silently off by orders of magnitude.
const (
	expectedCurrency = "CNY"
	expectedUnit     = "per_million_tokens"
	// expectedAudioUnit is the audio section's own contract: one CNY price
	// per million CHARACTERS, the single column an audio-mode candidate
	// bills by. A separate declaration rather than a per-entry override —
	// the whole section is character-priced by construction.
	expectedAudioUnit = "per_million_characters"
)

var (
	loadOnce   sync.Once
	loadedCat  catalogIndex
	loadedDate string
	loadErr    error
)

// Price is one model's four price slots. cache_write / cache_read are pointers
// so a nil faithfully means "this model has no cache pricing" (mirrors
// model.ModelCandidate's *float64 columns).
type Price struct {
	Input      float64  `json:"input"`
	Output     float64  `json:"output"`
	CacheWrite *float64 `json:"cache_write"`
	CacheRead  *float64 `json:"cache_read"`
}

// Catalog is the parsed seed file: prices keyed by base_url host, then by model
// name, plus the header fields declaring what the figures mean. The audio
// section is optional and carries its own unit declaration — speech prices by
// the character, not by the token, and mixing the two under one header is how
// a per-token refresh ends up billed as characters.
type Catalog struct {
	UpdatedAt string                      `json:"updated_at"`
	Currency  string                      `json:"currency"`
	Unit      string                      `json:"unit"`
	Prices    map[string]map[string]Price `json:"prices"`
	Audio     *AudioCatalog               `json:"audio,omitempty"`
}

// AudioCatalog is the character-priced section: one CNY-per-million-characters
// figure per host+model, for audio-mode candidates.
type AudioCatalog struct {
	Unit   string                        `json:"unit"`
	Prices map[string]map[string]float64 `json:"prices"`
}

// index is Catalog.Prices with both key levels already normalized (host
// stripped to its bare name, model lowercased), so a lookup is two map reads
// instead of a case-insensitive scan of every host and every model.
type index map[string]map[string]Price

// audioIndex is AudioCatalog.Prices under the same normalization.
type audioIndex map[string]map[string]float64

// catalogIndex is both halves of a catalog as one value. Live refresh swaps a
// whole one of these atomically, so a concurrent lookup can never observe the
// new token half against the old audio half.
type catalogIndex struct {
	tokens index
	audio  audioIndex
}

// UpdatedAt reports the date of the prices currently in effect, in YYYY-MM-DD
// form. When the background refresh has warmed a live index fetched from the
// distribution endpoint, this is that snapshot's date — what an admin
// sees next to a suggested price, so it reads "today's cron" rather than "the
// day this binary compiled". With no live index it falls back to the embedded
// seed's sync date, or "" if even that failed to load. The freshness this string
// expresses decides whether "from the official catalog" means "verify this" or
// "trust this".
func UpdatedAt() string {
	if d := liveDate.Load(); d != nil && *d != "" {
		return *d
	}
	if _, err := load(); err != nil {
		return ""
	}
	return loadedDate
}

// load parses and validates the embedded JSON exactly once. Embedding
// guarantees the bytes are present, so any failure here is a bad data file
// rather than a runtime condition; it is surfaced as a non-nil error from every
// Lookup (the caller then simply suggests nothing) and is caught in CI by
// TestEmbeddedCatalogLoads.
func load() (catalogIndex, error) {
	loadOnce.Do(func() {
		var c Catalog
		if err := json.Unmarshal(catalogBytes, &c); err != nil {
			loadErr = fmt.Errorf("parse catalog: %w", err)
			return
		}
		loadedCat, loadErr = buildIndex(&c)
		if loadErr == nil {
			loadedDate = c.UpdatedAt
		}
	})
	return loadedCat, loadErr
}

// buildIndex validates a parsed catalog and normalizes its keys. Split out from
// load so the validation and lookup rules can be exercised against a fixture
// instead of the shipped figures, which are expected to be re-synced.
func buildIndex(c *Catalog) (catalogIndex, error) {
	if c.Currency != expectedCurrency {
		return catalogIndex{}, fmt.Errorf("catalog currency is %q, want %q", c.Currency, expectedCurrency)
	}
	if c.Unit != expectedUnit {
		return catalogIndex{}, fmt.Errorf("catalog unit is %q, want %q", c.Unit, expectedUnit)
	}
	// A date rather than free text: it is the only signal of how stale the
	// figures are, and a typo would make that signal unreadable.
	if _, err := time.Parse(time.DateOnly, c.UpdatedAt); err != nil {
		return catalogIndex{}, fmt.Errorf("catalog updated_at %q is not a %s date", c.UpdatedAt, time.DateOnly)
	}

	idx, err := normalizedSection(c.Prices, "catalog")
	if err != nil {
		return catalogIndex{}, err
	}

	audio, err := buildAudioIndex(c.Audio)
	if err != nil {
		return catalogIndex{}, err
	}
	return catalogIndex{tokens: idx, audio: audio}, nil
}

// normalizedSection applies the shared host/model key rules to one section's
// prices: host stripped to its bare name, model lowercased, and collisions
// rejected because which entry wins must not depend on map iteration order.
// Both halves of the catalog (token and audio) fold through it so the rules
// cannot drift apart; what names the section in rejection messages.
func normalizedSection[V any](prices map[string]map[string]V, what string) (map[string]map[string]V, error) {
	idx := make(map[string]map[string]V, len(prices))
	for host, models := range prices {
		h := hostOf(host)
		if h == "" {
			return nil, fmt.Errorf("%s has an empty host key", what)
		}
		if _, dup := idx[h]; dup {
			return nil, fmt.Errorf("%s lists host %q more than once", what, h)
		}
		normalized := make(map[string]V, len(models))
		for name, p := range models {
			m := strings.ToLower(strings.TrimSpace(name))
			if m == "" {
				return nil, fmt.Errorf("%s host %q has an empty model key", what, h)
			}
			if _, dup := normalized[m]; dup {
				return nil, fmt.Errorf("%s host %q lists model %q more than once", what, h, m)
			}
			normalized[m] = p
		}
		idx[h] = normalized
	}
	return idx, nil
}

// buildAudioIndex normalizes the audio section under the same host/model rules
// as the token half, plus its own unit declaration check. A nil section is an
// empty index, not an error: deployments predate the section.
func buildAudioIndex(a *AudioCatalog) (audioIndex, error) {
	if a == nil {
		return audioIndex{}, nil
	}
	if a.Unit != expectedAudioUnit {
		return nil, fmt.Errorf("catalog audio unit is %q, want %q", a.Unit, expectedAudioUnit)
	}
	idx, err := normalizedSection(a.Prices, "catalog audio")
	if err != nil {
		return nil, err
	}
	return audioIndex(idx), nil
}

// hostOf normalizes a provider base_url down to its bare hostname — no scheme,
// port, or path — so "https://api.deepseek.com:443/v1" matches the seed key
// "api.deepseek.com". An unparseable base_url falls back to the trimmed raw
// string, which still matches in the common no-scheme case.
func hostOf(baseURL string) string {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return ""
	}
	// url.Parse needs a scheme to populate Host; prepend one if absent so a
	// user-supplied "api.deepseek.com/v1" still parses.
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	// Hostname rather than Host: it drops the port and unwraps the brackets
	// around an IPv6 literal, both of which would otherwise never match a key.
	if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
		return strings.ToLower(u.Hostname())
	}
	return strings.ToLower(strings.TrimSpace(baseURL))
}

// Lookup returns the price for a provider (by base_url) and model name,
// case-insensitively, from the warm live index when one exists, and
// otherwise from the embedded seed. ok is false when neither source has a
// matching entry — the caller then falls back to leaving the field empty.
//
// Live and embed UNION, not shadow: a Lookup checks the live index first, and
// on a miss falls back to the embedded seed rather than returning false. This
// lets the daily cron refresh a subset of providers (e.g. while a fetcher is
// being fixed) without "losing" the un-refreshed ones — they keep their
// embedded price. See TestLiveUnionsWithEmbed.
func Lookup(baseURL, modelName string) (Price, bool) {
	if p, ok := lookupLive(baseURL, modelName); ok {
		return p, true
	}
	idx, err := load()
	if err != nil {
		return Price{}, false
	}
	return lookupIn(idx.tokens, baseURL, modelName)
}

// LookupAudio returns the per-million-characters price for a provider and
// model, under the same live-first, union-with-embed rule as Lookup.
func LookupAudio(baseURL, modelName string) (float64, bool) {
	if p, ok := lookupLiveAudio(baseURL, modelName); ok {
		return p, true
	}
	idx, err := load()
	if err != nil {
		return 0, false
	}
	return lookupAudioIn(idx.audio, baseURL, modelName)
}

// lookupLive is the warm-index branch of Lookup. Returning false for an empty
// live index is what makes the embed fallback fire: a freshly started instance,
// or one whose endpoint is unreachable, keeps serving the embedded seed.
func lookupLive(baseURL, modelName string) (Price, bool) {
	p := live.Load()
	if p == nil {
		return Price{}, false
	}
	return lookupIn(p.tokens, baseURL, modelName)
}

func lookupLiveAudio(baseURL, modelName string) (float64, bool) {
	p := live.Load()
	if p == nil {
		return 0, false
	}
	return lookupAudioIn(p.audio, baseURL, modelName)
}

func lookupIn(idx index, baseURL, modelName string) (Price, bool) {
	host := hostOf(baseURL)
	model := strings.ToLower(strings.TrimSpace(modelName))
	if host == "" || model == "" {
		return Price{}, false
	}
	p, ok := idx[host][model]
	if !ok {
		return Price{}, false
	}
	// The struct copy above still shares the two pointers with the index, which
	// sync.Once keeps alive for the life of the process. Handing those out would
	// let one caller writing through them (a unit conversion, a markup) rewrite
	// the embedded catalog for every caller after it.
	return Price{Input: p.Input, Output: p.Output, CacheWrite: clonePtr(p.CacheWrite), CacheRead: clonePtr(p.CacheRead)}, true
}

func lookupAudioIn(idx audioIndex, baseURL, modelName string) (float64, bool) {
	host := hostOf(baseURL)
	model := strings.ToLower(strings.TrimSpace(modelName))
	if host == "" || model == "" {
		return 0, false
	}
	p, ok := idx[host][model]
	return p, ok
}

func clonePtr(v *float64) *float64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}
