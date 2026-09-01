package modeladmin_test

// Output-modalities declaration tests: the create path's default, the
// vocabulary the write path accepts, and the update path's rule that a
// submitted list is a whole replacement — never a silent reset to the
// default.

import (
	"errors"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

func TestCreateModelOutputModalities(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()

	t.Run("absent means text only", func(t *testing.T) {
		view, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "plain"}, now)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if len(view.OutputModalities) != 1 || view.OutputModalities[0] != model.OutputModalityText {
			t.Fatalf("output modalities = %v, want [text]", view.OutputModalities)
		}
	})

	t.Run("image declaration is stored and served", func(t *testing.T) {
		view, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "painter", OutputModalities: []string{model.OutputModalityImage}}, now)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if len(view.OutputModalities) != 1 || view.OutputModalities[0] != model.OutputModalityImage {
			t.Fatalf("output modalities = %v, want [image]", view.OutputModalities)
		}
		var stored model.Model
		if err := db.Where("id = ?", view.ID).First(&stored).Error; err != nil {
			t.Fatalf("read back: %v", err)
		}
		if !stored.ServesOutputModality(model.OutputModalityImage) {
			t.Fatalf("stored row does not serve image: %q", stored.OutputModalities)
		}
		if stored.ServesOutputModality(model.OutputModalityText) {
			t.Fatalf("stored row serves text despite an image-only declaration: %q", stored.OutputModalities)
		}
	})

	t.Run("unknown id is rejected", func(t *testing.T) {
		_, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "broken", OutputModalities: []string{"video"}}, now)
		if err == nil {
			t.Fatal("create accepted an unknown modality id")
		}
	})

	t.Run("duplicate id is rejected", func(t *testing.T) {
		_, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "dup", OutputModalities: []string{"text", "text"}}, now)
		if err == nil {
			t.Fatal("create accepted a duplicate modality id")
		}
	})
}

func TestUpdateModelOutputModalities(t *testing.T) {
	svc, _, _ := newTestModelService(t)
	now := time.Now().UTC()
	created, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "flip"}, now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	t.Run("replaces the whole declaration", func(t *testing.T) {
		view, err := svc.UpdateModel(created.ID, modeladmin.UpdateModelInput{
			Name: "flip", OutputModalitiesSet: true,
			OutputModalities: []string{model.OutputModalityText, model.OutputModalityImage},
		}, now)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if len(view.OutputModalities) != 2 {
			t.Fatalf("output modalities = %v, want both", view.OutputModalities)
		}
	})

	t.Run("absent leaves the stored declaration alone", func(t *testing.T) {
		view, err := svc.UpdateModel(created.ID, modeladmin.UpdateModelInput{Name: "flip"}, now)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if len(view.OutputModalities) != 2 {
			t.Fatalf("output modalities = %v, want the stored pair untouched", view.OutputModalities)
		}
	})

	t.Run("empty present list is a rejection, not the default", func(t *testing.T) {
		_, err := svc.UpdateModel(created.ID, modeladmin.UpdateModelInput{
			Name: "flip", OutputModalitiesSet: true, OutputModalities: []string{},
		}, now)
		if err == nil {
			t.Fatal("update accepted an empty declaration")
		}
		if !errors.Is(err, errcode.ErrModelOutputModalityInvalid) {
			t.Fatalf("error = %v, want the output-modality sentinel", err)
		}
	})
}

// The batch create carries one declaration for every name it creates, so the
// dialog's preset quick-add can land image models in the image pool directly.
func TestCreateModelsBatchOutputModalities(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	now := time.Now().UTC()

	t.Run("declaration applies to every created row", func(t *testing.T) {
		result, err := svc.CreateModelsBatch(modeladmin.CreateModelsBatchInput{
			Names: []string{"wan2.7-image", "wan2.7-image-pro"}, OutputModalities: []string{model.OutputModalityImage},
		}, now)
		if err != nil {
			t.Fatalf("batch create: %v", err)
		}
		if len(result.Created) != 2 {
			t.Fatalf("created = %d, want 2", len(result.Created))
		}
		for _, view := range result.Created {
			var stored model.Model
			if err := db.Where("id = ?", view.ID).First(&stored).Error; err != nil {
				t.Fatalf("read back: %v", err)
			}
			if !stored.OutputImageExclusive() {
				t.Fatalf("batch-declared row not image-exclusive: %q", stored.OutputModalities)
			}
		}
	})

	t.Run("absent keeps the text-only default", func(t *testing.T) {
		result, err := svc.CreateModelsBatch(modeladmin.CreateModelsBatchInput{Names: []string{"plain-batch-model"}}, now)
		if err != nil {
			t.Fatalf("batch create: %v", err)
		}
		var stored model.Model
		if err := db.Where("id = ?", result.Created[0].ID).First(&stored).Error; err != nil {
			t.Fatalf("read back: %v", err)
		}
		if stored.OutputModalities != `["text"]` {
			t.Fatalf("undeclared batch row = %q, want text-only", stored.OutputModalities)
		}
	})

	t.Run("invalid list rejects the whole request", func(t *testing.T) {
		_, err := svc.CreateModelsBatch(modeladmin.CreateModelsBatchInput{
			Names: []string{"never-created"}, OutputModalities: []string{"video"},
		}, now)
		if err == nil {
			t.Fatal("batch create accepted an unknown modality id")
		}
		var count int64
		if err := db.Model(&model.Model{}).Where("name = ?", "never-created").Count(&count).Error; err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 0 {
			t.Fatal("a rejected declaration must not leave rows behind")
		}
	})
}
