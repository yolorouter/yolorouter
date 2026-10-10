package model

import (
	"os"
	"strings"
	"testing"
)

// The input-modalities declaration: write-path vocabulary validation
// (mirroring the output-modalities precedent), the lenient read, and the
// mirror derivation onto the deprecated supports_image_input column.

func TestCanonicalInputModalities(t *testing.T) {
	t.Run("empty list is the legal undeclared form", func(t *testing.T) {
		for _, list := range [][]string{nil, {}} {
			out, err := CanonicalInputModalities(list)
			if err != nil {
				t.Fatalf("empty list must canonicalize to undeclared, got error: %v", err)
			}
			if out != "" {
				t.Fatalf("undeclared canonical form = %q, want empty string", out)
			}
		}
	})

	t.Run("valid ids are stored in submitted order", func(t *testing.T) {
		out, err := CanonicalInputModalities([]string{InputModalityText, InputModalityImage, InputModalityAudio, InputModalityVideo})
		if err != nil {
			t.Fatalf("canonicalize: %v", err)
		}
		if out != `["text","image","audio","video"]` {
			t.Fatalf("canonical form = %q", out)
		}
	})

	t.Run("mixed case and surrounding blanks fold onto the vocabulary", func(t *testing.T) {
		out, err := CanonicalInputModalities([]string{"  Text ", "IMAGE", "audio"})
		if err != nil {
			t.Fatalf("canonicalize: %v", err)
		}
		if out != `["text","image","audio"]` {
			t.Fatalf("canonical form = %q, want folded lowercase ids", out)
		}
	})

	t.Run("unknown id is rejected", func(t *testing.T) {
		if _, err := CanonicalInputModalities([]string{"text", "vibes"}); err == nil {
			t.Fatal("unknown modality id was accepted")
		}
	})

	t.Run("empty string element is rejected", func(t *testing.T) {
		if _, err := CanonicalInputModalities([]string{"text", ""}); err == nil {
			t.Fatal("an empty id inside the list was accepted")
		}
		if _, err := CanonicalInputModalities([]string{"  "}); err == nil {
			t.Fatal("a blank id was accepted")
		}
	})

	t.Run("duplicate after folding is rejected", func(t *testing.T) {
		if _, err := CanonicalInputModalities([]string{"TEXT", "text"}); err == nil {
			t.Fatal("a duplicate that only differs by case was accepted")
		}
	})
}

// The lenient read: a stored value must never fail a reader, and every
// no-declaration spelling (NULL, empty, malformed, empty JSON array) reads as
// the same nil — never as a narrowed declaration.
func TestInputModalityListLenientRead(t *testing.T) {
	for _, raw := range []string{"", "   ", "null", "[]", "{oops", `["text"`} {
		if got := (Model{InputModalities: raw}).InputModalityList(); got != nil {
			t.Errorf("stored %q read as %v, want nil (undeclared)", raw, got)
		}
	}
	got := (Model{InputModalities: `["text","image"]`}).InputModalityList()
	if len(got) != 2 || got[0] != "text" || got[1] != "image" {
		t.Fatalf(`stored ["text","image"] read as %v`, got)
	}
	// An unknown id in a stored row survives the read as-is: the read path is
	// laxer than the write path, and it is not this function's job to judge.
	if got := (Model{InputModalities: `["telepathy"]`}).InputModalityList(); len(got) != 1 || got[0] != "telepathy" {
		t.Fatalf(`stored ["telepathy"] read as %v, want it passed through`, got)
	}
}

// DeriveImageInputSupport is the mirror the save path writes into the
// deprecated column. Every form matters: the gateway's vision-fallback hot
// path still reads that column, so a wrong direction here would silently
// change request behaviour on upgrade.
func TestDeriveImageInputSupport(t *testing.T) {
	assertTri := func(t *testing.T, canonical string, want *bool) {
		t.Helper()
		got := DeriveImageInputSupport(canonical)
		switch {
		case want == nil && got != nil:
			t.Fatalf("derive(%q) = %v, want nil", canonical, *got)
		case want != nil && (got == nil || *got != *want):
			t.Fatalf("derive(%q) = %v, want %v", canonical, got, *want)
		}
	}
	tr, fl := true, false
	assertTri(t, "", nil)                  // no declaration
	assertTri(t, `["text","image"]`, &tr)  // contains image
	assertTri(t, `["image"]`, &tr)         // image alone still counts
	assertTri(t, `["text"]`, &fl)          // non-empty without image
	assertTri(t, `["audio","video"]`, &fl) // exotic inputs are still "no image"
	assertTri(t, "not json", nil)          // unparseable input degrades to undeclared
}

// SupportsImageInput is deprecated in favour of InputModalities; the
// deprecation must stay visible at the field so no new reader picks the old
// column up as the source of truth. This is the static pin: stripping the
// annotation from model.go turns it red.
func TestSupportsImageInputDeprecatedAnnotation(t *testing.T) {
	src, err := os.ReadFile("model.go")
	if err != nil {
		t.Fatalf("read model.go: %v", err)
	}
	lines := strings.Split(string(src), "\n")
	field := -1
	for i, line := range lines {
		if strings.Contains(line, "SupportsImageInput *bool") {
			field = i
			break
		}
	}
	if field < 0 {
		t.Fatal("SupportsImageInput field not found in model.go")
	}
	// The doc comment is the contiguous // block directly above the field.
	var comment []string
	for j := field - 1; j >= 0 && strings.HasPrefix(strings.TrimSpace(lines[j]), "//"); j-- {
		comment = append([]string{lines[j]}, comment...)
	}
	joined := strings.Join(comment, "\n")
	if !strings.Contains(joined, "Deprecated:") {
		t.Fatal("SupportsImageInput lost its Deprecated: annotation")
	}
	if !strings.Contains(joined, "InputModalities") && !strings.Contains(joined, "input_modalities") {
		t.Fatal("the deprecation note must name the replacing field (InputModalities) so new readers stop at the old column")
	}
}
