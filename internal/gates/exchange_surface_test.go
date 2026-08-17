package gates

import (
	"go/ast"
	"sort"
	"strings"
	"testing"
)

// TestExchangeExportsReadersOnly pins the exported surface of the kernel's
// Exchange. Every capability bind hands an Exchange across the seam, and
// whatever that hand-over reaches is the kernel's real boundary — so the
// exported method set must be exactly the read-only vocabulary the binds
// rely on, nothing more. The four body mutators that once stayed exported
// for the protocol layer's buffer interface are gone (the relay helpers take
// a small adapter instead); this check is what turns "gone" into "stays
// gone".
//
// Growing the set is a deliberate act, not a side effect: add the method AND
// this list in the same change, and let the failure message be the prompt to
// think about whether the thing being handed across the seam should really
// be able to do that.
func TestExchangeExportsReadersOnly(t *testing.T) {
	files := parseTree(t, "internal")

	// Production files only. A test file may declare a helper method on
	// *Exchange that nothing outside the package — and nothing on the
	// kernel's boundary — can ever see, so the pinned surface is the
	// non-test one.
	got := map[string]bool{}
	inspected := 0
	for _, f := range files {
		if !strings.HasPrefix(f.rel, "internal/gateway/") || strings.HasSuffix(f.rel, "_test.go") {
			continue
		}
		for _, decl := range f.ast.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || methodReceiverName(fn) != "Exchange" {
				continue
			}
			inspected++
			if fn.Name.IsExported() {
				got[fn.Name.Name] = true
			}
		}
	}
	if inspected == 0 {
		t.Fatal("found no methods declared on *Exchange; the check would pass by finding nothing")
	}

	// want is the read-only vocabulary, alphabetical. The four names that
	// must never reappear on their own — AppendResponse, AppendUpstream,
	// SetBody, SetResponseBody — are absent on purpose; a message below
	// names where to think, not just what broke.
	want := map[string]bool{
		"APIKeyID":                  true,
		"AuthCredential":            true,
		"CallSource":                true,
		"CandidateMaxOutput":        true,
		"CompressedRequestBody":     true,
		"CompressEnabled":           true,
		"ConcurrencyLimit":          true,
		"CustomSystemPrompt":        true,
		"CustomSystemPromptEnabled": true,
		"IngressPath":               true,
		"IngressProtocol":           true,
		"IsChatEndpoint":            true,
		"IsStream":                  true,
		"IsVisionFallbackSubCall":   true,
		"OriginalModel":             true,
		"ParentRequestID":           true,
		"ProviderID":                true,
		"RequestBody":               true,
		"RequestHeaders":            true,
		"RequestID":                 true,
		"ResponseBody":              true,
		"RPMLimit":                  true,
		"StreamBodyPath":            true,
		"StreamBodyTruncated":       true,
		"TPMLimit":                  true,
		"UpstreamRequestBody":       true,
		"UpstreamResponseBody":      true,
		"UpstreamURL":               true,
		"UserID":                    true,
		"VisionFallbackModel":       true,
		"VisionFallbackPrompt":      true,
	}

	for name := range got {
		if !want[name] {
			t.Errorf("*Exchange exports %q, which is not in the pinned read-only list. If the method genuinely belongs on the kernel's boundary, add it to the list in this test in the same change; if it writes anything, it belongs behind a narrow adapter instead.", name)
		}
	}
	var missing []string
	for name := range want {
		if !got[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("*Exchange no longer exports %v — update the pinned list in this test together with the removal, or the check misreports the surface it claims to pin.", missing)
	}
}
