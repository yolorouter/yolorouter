package modeladmin_test

// Capability-metadata tests for the model save path: the five new fields
// round-trip with their tri-states intact (nil stays nil), and every write
// of input_modalities derives the deprecated supports_image_input mirror in
// the direction the vision-fallback hot path depends on.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// TestUpdateModelCapabilityMetadataRoundTrip walks each of the five fields
// through set → untouched → cleared, reading both the API view and the stored
// row: a set value must come back byte-identical, an update that omits the
// field must leave it alone, and a cleared field must land back on NULL —
// nil is a state ("undeclared"), not a spelling of zero.
func TestUpdateModelCapabilityMetadataRoundTrip(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	created, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "cap-roundtrip"}, now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// 1. Set every field in one PATCH. The temperature deliberately carries
	// three decimals: it pins the float64-faithful caliber (SQLite REAL and
	// the postgres tree's DOUBLE PRECISION both store an IEEE double, and a
	// declaration must survive the store bit-exactly — a 2-decimal value like
	// 0.7 could not tell a rounding column from a faithful one).
	ctxLen := 200000
	tools := true
	reasoning := false
	temp := 0.705
	view, err := svc.UpdateModel(created.ID, modeladmin.UpdateModelInput{
		Name:             "cap-roundtrip",
		ContextLengthSet: true, ContextLength: &ctxLen,
		SupportsToolsSet: true, SupportsTools: &tools,
		SupportsReasoningSet: true, SupportsReasoning: &reasoning,
		TemperatureSet: true, Temperature: &temp,
		InputModalitiesSet: true, InputModalities: []string{"text", "image", "audio", "video"},
	}, now)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if view.ContextLength == nil || *view.ContextLength != ctxLen {
		t.Fatalf("context_length = %v, want %d", view.ContextLength, ctxLen)
	}
	if view.SupportsTools == nil || !*view.SupportsTools {
		t.Fatalf("supports_tools = %v, want true", view.SupportsTools)
	}
	if view.SupportsReasoning == nil || *view.SupportsReasoning {
		t.Fatalf("supports_reasoning = %v, want false", view.SupportsReasoning)
	}
	if view.Temperature == nil || *view.Temperature != temp {
		t.Fatalf("temperature = %v, want %v", view.Temperature, temp)
	}
	if len(view.InputModalities) != 4 || view.InputModalities[0] != "text" || view.InputModalities[3] != "video" {
		t.Fatalf("input_modalities = %v, want the declared four in order", view.InputModalities)
	}
	// The mirror: a declaration containing image must read true everywhere.
	if view.SupportsImageInput == nil || !*view.SupportsImageInput {
		t.Fatalf("supports_image_input mirror = %v, want true for an image-declaring list", view.SupportsImageInput)
	}

	// 2. An update that omits every capability field keeps the stored values.
	view, err = svc.UpdateModel(created.ID, modeladmin.UpdateModelInput{Name: "cap-roundtrip"}, now)
	if err != nil {
		t.Fatalf("update without fields: %v", err)
	}
	if view.ContextLength == nil || *view.ContextLength != ctxLen ||
		view.SupportsTools == nil || !*view.SupportsTools ||
		view.SupportsReasoning == nil || *view.SupportsReasoning ||
		view.Temperature == nil || *view.Temperature != temp ||
		len(view.InputModalities) != 4 {
		t.Fatalf("an unrelated update dropped stored declarations: %+v", view)
	}

	// 3. Clear every field back to undeclared.
	view, err = svc.UpdateModel(created.ID, modeladmin.UpdateModelInput{
		Name:                 "cap-roundtrip",
		ContextLengthSet:     true,
		SupportsToolsSet:     true,
		SupportsReasoningSet: true,
		TemperatureSet:       true,
		InputModalitiesSet:   true, InputModalities: []string{},
	}, now)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if view.ContextLength != nil || view.SupportsTools != nil || view.SupportsReasoning != nil ||
		view.Temperature != nil || view.InputModalities != nil || view.SupportsImageInput != nil {
		t.Fatalf("cleared declarations must all read undeclared, got %+v", view)
	}

	// And the row itself, not just the view, must hold the tri-states.
	var stored model.Model
	if err := db.Where("id = ?", created.ID).First(&stored).Error; err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored.ContextLength != nil || stored.SupportsTools != nil || stored.SupportsReasoning != nil ||
		stored.Temperature != nil || //nolint:staticcheck // SA1019: asserting the deprecated mirror column is the test's purpose
		stored.SupportsImageInput != nil || stored.InputModalityList() != nil {
		t.Fatalf("cleared row must store NULLs, got %+v", stored)
	}
}

// TestUpdateModelInputModalitiesMirror pins the four derivation forms. The
// direction is the zero-behaviour redline: the gateway's vision-fallback hot
// path still reads supports_image_input, so a save that stores a declaration
// without deriving the old column (or derives it backwards) changes request
// behaviour on upgrade.
func TestUpdateModelInputModalitiesMirror(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()

	cases := []struct {
		name     string
		list     []string // nil means "clear" via a set-but-empty declaration
		wantNil  bool
		wantBool bool
	}{
		{"contains image derives true", []string{"text", "image"}, false, true},
		{"image alone derives true", []string{"image"}, false, true},
		{"non-empty without image derives false", []string{"text"}, false, false},
		{"non-image modalities derive false", []string{"audio", "video"}, false, false},
		{"cleared declaration derives nil", []string{}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slug := "mirror-" + strings.ReplaceAll(tc.name, " ", "-")
			created, err := svc.CreateModel(modeladmin.CreateModelInput{Name: slug}, now)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			view, err := svc.UpdateModel(created.ID, modeladmin.UpdateModelInput{
				Name:               slug,
				InputModalitiesSet: true, InputModalities: tc.list,
			}, now)
			if err != nil {
				t.Fatalf("update: %v", err)
			}
			if tc.wantNil {
				if view.SupportsImageInput != nil {
					t.Fatalf("supports_image_input = %v, want nil (undeclared)", *view.SupportsImageInput)
				}
			} else if view.SupportsImageInput == nil || *view.SupportsImageInput != tc.wantBool {
				t.Fatalf("supports_image_input = %v, want %v", view.SupportsImageInput, tc.wantBool)
			}
			// The stored row, not just the view, carries the derived mirror.
			var stored model.Model
			if err := db.Where("id = ?", created.ID).First(&stored).Error; err != nil {
				t.Fatalf("read back: %v", err)
			}
			if tc.wantNil {
				//nolint:staticcheck // SA1019: asserting the deprecated mirror column is the test's purpose
				if stored.SupportsImageInput != nil {
					t.Fatalf("stored supports_image_input = %v, want nil", *stored.SupportsImageInput)
				}
			} else if stored.SupportsImageInput == nil || *stored.SupportsImageInput != tc.wantBool { //nolint:staticcheck // SA1019: asserting the deprecated mirror column is the test's purpose
				t.Fatalf("stored supports_image_input = %v, want %v", stored.SupportsImageInput, tc.wantBool) //nolint:staticcheck // SA1019: mirror assertion
			}
		})
	}

	// A declaration that stops declaring image must FLIP the old column from
	// true to false — the transition an upgraded deployment actually makes.
	t.Run("re-declaration flips the mirror", func(t *testing.T) {
		created, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "mirror-flip"}, now)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := svc.UpdateModel(created.ID, modeladmin.UpdateModelInput{
			Name: "mirror-flip", InputModalitiesSet: true, InputModalities: []string{"text", "image"},
		}, now); err != nil {
			t.Fatalf("declare image: %v", err)
		}
		view, err := svc.UpdateModel(created.ID, modeladmin.UpdateModelInput{
			Name: "mirror-flip", InputModalitiesSet: true, InputModalities: []string{"text"},
		}, now)
		if err != nil {
			t.Fatalf("drop image: %v", err)
		}
		if view.SupportsImageInput == nil || *view.SupportsImageInput {
			t.Fatalf("supports_image_input = %v, want false after dropping image from the declaration", view.SupportsImageInput)
		}
	})

	// A request carrying both fields: the array is authoritative, the legacy
	// boolean in the same request must not win against its own derivation.
	t.Run("submitted array supersedes a legacy boolean in the same request", func(t *testing.T) {
		created, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "mirror-precedence"}, now)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		legacyTrue := true
		view, err := svc.UpdateModel(created.ID, modeladmin.UpdateModelInput{
			Name:          "mirror-precedence",
			ImageInputSet: true, ImageInput: &legacyTrue,
			InputModalitiesSet: true, InputModalities: []string{"text"},
		}, now)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if view.SupportsImageInput == nil || *view.SupportsImageInput {
			t.Fatalf("supports_image_input = %v, want the array-derived false", view.SupportsImageInput)
		}
	})
}

// The input-modalities write path through the service: mixed case folds onto
// the vocabulary before storing, and an unknown or empty id answers with the
// field's own sentinel instead of a generic failure.
func TestUpdateModelInputModalitiesVocabulary(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()
	created, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "vocab"}, now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	t.Run("mixed case folds to canonical ids on the stored row", func(t *testing.T) {
		_, err := svc.UpdateModel(created.ID, modeladmin.UpdateModelInput{
			Name: "vocab", InputModalitiesSet: true, InputModalities: []string{"  Text ", "IMAGE"},
		}, now)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		var stored model.Model
		if err := db.Where("id = ?", created.ID).First(&stored).Error; err != nil {
			t.Fatalf("read back: %v", err)
		}
		if stored.InputModalities != `["text","image"]` {
			t.Fatalf("stored input_modalities = %q, want folded canonical form", stored.InputModalities)
		}
	})

	t.Run("unknown id answers the input-modality sentinel", func(t *testing.T) {
		_, err := svc.UpdateModel(created.ID, modeladmin.UpdateModelInput{
			Name: "vocab", InputModalitiesSet: true, InputModalities: []string{"telepathy"},
		}, now)
		if !errors.Is(err, errcode.ErrModelInputModalityInvalid) {
			t.Fatalf("error = %v, want ErrModelInputModalityInvalid", err)
		}
		var stored model.Model
		if err := db.Where("id = ?", created.ID).First(&stored).Error; err != nil {
			t.Fatalf("read back: %v", err)
		}
		if stored.InputModalities != `["text","image"]` {
			t.Fatalf("a rejected declaration must not persist, got %q", stored.InputModalities)
		}
	})
}
