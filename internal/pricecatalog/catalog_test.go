package pricecatalog

import "testing"

func ptr(v float64) *float64 { return &v }

// fixtureCatalog is a stand-in for catalog.json. The lookup rules are asserted
// against it rather than against the shipped figures, which are re-synced from
// vendor pricing pages and must be able to change without breaking tests.
func fixtureCatalog() *Catalog {
	return &Catalog{
		UpdatedAt: "2026-01-02",
		Currency:  expectedCurrency,
		Unit:      expectedUnit,
		Prices: map[string]map[string]Price{
			"api.example.com": {
				"Model-Flash": {Input: 1, Output: 2, CacheRead: ptr(0.02)},
				"model-pro":   {Input: 3, Output: 6, CacheWrite: ptr(4), CacheRead: ptr(0.3)},
			},
		},
	}
}

func fixtureIndex(t *testing.T) index {
	t.Helper()
	idx, err := buildIndex(fixtureCatalog())
	if err != nil {
		t.Fatalf("buildIndex on the fixture failed: %v", err)
	}
	return idx
}

// The embedded seed is data, not code, so nothing else proves it is well
// formed: Lookup swallows a load failure and silently suggests nothing. This is
// the check that turns a broken or wrongly-denominated catalog.json into a
// build failure instead of a feature that quietly stops working.
func TestEmbeddedCatalogLoads(t *testing.T) {
	idx, err := load()
	if err != nil {
		t.Fatalf("embedded catalog failed to load: %v", err)
	}
	if len(idx) == 0 {
		t.Fatal("embedded catalog has no hosts")
	}
	for host, models := range idx {
		if len(models) == 0 {
			t.Errorf("host %q has no models", host)
		}
		for name, p := range models {
			if p.Input < 0 || p.Output < 0 {
				t.Errorf("%s/%s: negative price input=%v output=%v", host, name, p.Input, p.Output)
			}
			if p.CacheWrite != nil && *p.CacheWrite < 0 {
				t.Errorf("%s/%s: negative cache_write %v", host, name, *p.CacheWrite)
			}
			if p.CacheRead != nil && *p.CacheRead < 0 {
				t.Errorf("%s/%s: negative cache_read %v", host, name, *p.CacheRead)
			}
		}
	}
}

func TestLookupReturnsAllFourSlots(t *testing.T) {
	idx := fixtureIndex(t)

	p, ok := lookupIn(idx, "https://api.example.com/v1", "model-flash")
	if !ok {
		t.Fatal("expected a hit for api.example.com/model-flash")
	}
	if p.Input != 1 || p.Output != 2 {
		t.Errorf("want input=1 output=2, got input=%v output=%v", p.Input, p.Output)
	}
	// A model with no cache-write fee keeps a nil, which the caller forwards as
	// a NULL column rather than as a zero price.
	if p.CacheWrite != nil {
		t.Errorf("want nil cache_write, got %v", *p.CacheWrite)
	}
	if p.CacheRead == nil || *p.CacheRead != 0.02 {
		t.Errorf("want cache_read=0.02, got %v", p.CacheRead)
	}
}

func TestLookupNormalizesBaseURL(t *testing.T) {
	idx := fixtureIndex(t)
	// Every form an admin can plausibly paste into base_url must resolve to the
	// same host key, including one carrying an explicit port.
	cases := []string{
		"https://api.example.com/v1",
		"http://api.example.com",
		"api.example.com/v1",
		"API.Example.COM/v1",
		"https://api.example.com:443/v1",
		"  https://api.example.com/v1  ",
	}
	for _, base := range cases {
		if _, ok := lookupIn(idx, base, "model-flash"); !ok {
			t.Errorf("expected lookup to match normalized host for %q", base)
		}
	}
}

func TestLookupIsCaseInsensitiveOnModelName(t *testing.T) {
	idx := fixtureIndex(t)
	// The catalog key itself is mixed case, so this covers both directions:
	// a lowercased query against a mixed-case key and vice versa.
	for _, name := range []string{"model-flash", "Model-Flash", "MODEL-FLASH", "  model-pro  "} {
		if _, ok := lookupIn(idx, "https://api.example.com", name); !ok {
			t.Errorf("expected case-insensitive model match for %q", name)
		}
	}
}

func TestLookupMissesReturnFalse(t *testing.T) {
	idx := fixtureIndex(t)
	cases := []struct{ name, baseURL, model string }{
		{"unknown host", "https://not-a-real-host.example.org", "model-flash"},
		{"known host, unknown model", "https://api.example.com/v1", "does-not-exist"},
		{"empty base_url", "", "model-flash"},
		{"empty model", "https://api.example.com/v1", ""},
		{"blank model", "https://api.example.com/v1", "   "},
	}
	for _, tc := range cases {
		if _, ok := lookupIn(idx, tc.baseURL, tc.model); ok {
			t.Errorf("%s: expected a miss", tc.name)
		}
	}
}

func TestBuildIndexRejectsWrongUnitContract(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Catalog)
		wantErr string
	}{
		{"foreign currency", func(c *Catalog) { c.Currency = "USD" }, "currency"},
		{"per-1K unit", func(c *Catalog) { c.Unit = "per_thousand_tokens" }, "unit"},
		{"missing currency", func(c *Catalog) { c.Currency = "" }, "currency"},
		{"unparseable updated_at", func(c *Catalog) { c.UpdatedAt = "soon" }, "updated_at"},
	}
	for _, tc := range cases {
		c := fixtureCatalog()
		tc.mutate(c)
		if _, err := buildIndex(c); err == nil {
			t.Errorf("%s: expected buildIndex to reject the catalog", tc.name)
		}
	}
}

// Duplicate keys would make the returned price depend on map iteration order,
// which Go randomizes — the same binary would bill differently run to run.
func TestBuildIndexRejectsCollidingKeys(t *testing.T) {
	c := fixtureCatalog()
	c.Prices["API.Example.com"] = map[string]Price{"model-flash": {Input: 99}}
	if _, err := buildIndex(c); err == nil {
		t.Error("expected buildIndex to reject two host keys that normalize alike")
	}

	c = fixtureCatalog()
	c.Prices["api.example.com"]["MODEL-FLASH"] = Price{Input: 99}
	if _, err := buildIndex(c); err == nil {
		t.Error("expected buildIndex to reject two model keys that normalize alike")
	}
}

func TestBuildIndexRejectsEmptyKeys(t *testing.T) {
	c := fixtureCatalog()
	c.Prices[""] = map[string]Price{"model-flash": {}}
	if _, err := buildIndex(c); err == nil {
		t.Error("expected buildIndex to reject an empty host key")
	}

	c = fixtureCatalog()
	c.Prices["api.example.com"][" "] = Price{}
	if _, err := buildIndex(c); err == nil {
		t.Error("expected buildIndex to reject a blank model key")
	}
}

// The pointers in a returned Price must not reach into the process-wide index,
// or one caller adjusting a price in place would rewrite the catalog for every
// caller after it.
func TestLookupDoesNotAliasTheIndex(t *testing.T) {
	idx := fixtureIndex(t)

	first, ok := lookupIn(idx, "https://api.example.com", "model-pro")
	if !ok || first.CacheRead == nil {
		t.Fatal("fixture must have a cache_read price to mutate")
	}
	*first.CacheRead = 999
	*first.CacheWrite = 999

	second, _ := lookupIn(idx, "https://api.example.com", "model-pro")
	if *second.CacheRead == 999 || *second.CacheWrite == 999 {
		t.Fatalf("mutating a returned Price rewrote the catalog: %+v", second)
	}
}

// cache_read/cache_write silently deserialize as nil when the JSON keys use
// the wrong casing (e.g. "cacheRead"), and nil passes every non-negativity
// check above. At least one shipped cache price must come through, or the
// casing regressed and every cache-price prefill quietly went empty.
func TestEmbeddedCatalogDeserializesCachePrices(t *testing.T) {
	idx, err := load()
	if err != nil {
		t.Fatalf("embedded catalog failed to load: %v", err)
	}
	for _, models := range idx {
		for _, p := range models {
			if p.CacheRead != nil || p.CacheWrite != nil {
				return
			}
		}
	}
	t.Fatal("embedded catalog has no non-nil cache price; the JSON keys have probably regressed to camelCase")
}

func TestUpdatedAtReportsTheEmbeddedDate(t *testing.T) {
	// The date is what tells an operator whether a suggested price is a month or
	// a year old, so an empty string here means the catalog failed to load.
	if UpdatedAt() == "" {
		t.Fatal("expected the embedded catalog to report its sync date")
	}
}
