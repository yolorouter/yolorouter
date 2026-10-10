package modeladmin

// Save-time consistency rules between a model's capability declaration and
// its candidate set. The invariants span BOTH sides of the pair, so every
// save — the model save and the candidate save alike — re-reads the other
// side and re-asserts them against the effective post-save state; neither
// path alone can drift the pair into self-contradiction. The criteria are
// hard-coded constants rather than admin-configurable policy, bind only the
// admin save paths (the gateway's forwarding behaviour reads none of this),
// and skip non-save transitions: the dedicated status toggle and the probe
// queue's auto-enable are not candidate saves, so neither re-asserts the pair.

import (
	"fmt"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// The closed interval a declared model temperature must lie in — the same
// bounds the playground handler's request binding enforces (gte=0, lte=2), so
// the recorded recommendation can never name a value the playground would
// itself refuse.
const (
	modelTemperatureMin = 0
	modelTemperatureMax = 2
)

// validateTemperature applies the temperature range rule to a model's
// effective post-save declaration. nil passes: undeclared is a tri-state
// value of its own, never a zero to bound-check.
func validateTemperature(declared *float64) error {
	if declared == nil {
		return nil
	}
	if *declared < modelTemperatureMin || *declared > modelTemperatureMax {
		return fmt.Errorf("%w: %v is outside [%v, %v]",
			errcode.ErrModelTemperatureInvalid, *declared, modelTemperatureMin, modelTemperatureMax)
	}
	return nil
}

// candidateDeclaration is one candidate's effective post-save state as the
// cross-entity rules read it: the submitted values where a save changes a
// field, the stored ones where it leaves the field alone.
type candidateDeclaration struct {
	providerID     uint
	enabled        bool
	billingMode    string
	imageTiersJSON string
	maxOutput      int
}

// declarationFromRow maps a stored candidate row onto the rules' view of it.
func declarationFromRow(c model.ModelCandidate) candidateDeclaration {
	return candidateDeclaration{
		providerID:     c.ProviderID,
		enabled:        c.ManagementStatus == model.ModelCandidateStatusEnabled,
		billingMode:    c.BillingMode,
		imageTiersJSON: c.ImagePricingTiers,
		maxOutput:      c.MaxOutput,
	}
}

// validateModelCandidateConsistency applies the two cross-entity invariants
// to one model's effective declaration against its candidate set's effective
// post-save states:
//
//   - image rule: a model whose output contains image prices per delivered
//     image, so every ENABLED candidate must bill per image and carry a
//     stored pricing table — a token-billed enabled candidate delivers images
//     nothing bills. Disabled candidates are exempt: they route nothing, and
//     the rule must not block storing a work-in-progress mapping.
//   - context rule: an enabled candidate's max_output must not exceed the
//     model's declared context_length. Equal passes (a candidate may fill the
//     whole window); an undeclared context pins nothing.
//
// Both rules read the LENIENT parsers (OutputModalityList,
// NormalizeBillingMode, ParseImagePricingTiers) on purpose: a save re-asserts
// the pair exactly as every reader will read it back, so a legacy malformed
// value surfaces as a save-time rejection rather than passing the save and
// breaking at read.
func validateModelCandidateConsistency(m model.Model, candidates []candidateDeclaration) error {
	if m.ServesOutputModality(model.OutputModalityImage) {
		for _, c := range candidates {
			if !c.enabled {
				continue
			}
			if model.NormalizeBillingMode(c.billingMode) != model.BillingModeImage ||
				model.ParseImagePricingTiers(c.imageTiersJSON) == nil {
				return fmt.Errorf("%w: enabled candidate of provider %d bills %q",
					errcode.ErrModelImageBillingMismatch, c.providerID, c.billingMode)
			}
		}
	}
	if m.ContextLength != nil {
		for _, c := range candidates {
			if !c.enabled {
				continue
			}
			if c.maxOutput > *m.ContextLength {
				return fmt.Errorf("%w: enabled candidate of provider %d declares max_output %d over context_length %d",
					errcode.ErrModelMaxOutputExceedsContext, c.providerID, c.maxOutput, *m.ContextLength)
			}
		}
	}
	return nil
}

// validateModelSaveConsistency applies the save-time rules to a model PATCH:
// the model's effective post-save declaration (each submitted field over the
// stored row) is checked in its own right (temperature) and against the
// candidate set, which is re-read inside the same transaction that is about
// to write — the pair cannot change between the check and the write.
// canonicalOutput is the already-validated canonical JSON of a submitted
// output declaration; empty means the PATCH leaves the stored one alone.
func (s *ModelService) validateModelSaveConsistency(tx *gorm.DB, stored model.Model, in UpdateModelInput, canonicalOutput string) error {
	effective := stored
	if in.OutputModalitiesSet {
		effective.OutputModalities = canonicalOutput
	}
	if in.ContextLengthSet {
		effective.ContextLength = in.ContextLength
	}
	if in.TemperatureSet {
		effective.Temperature = in.Temperature
	}
	if err := validateTemperature(effective.Temperature); err != nil {
		return err
	}
	rows, err := repository.ListModelCandidatesByModelID(tx, stored.ID)
	if err != nil {
		return err
	}
	declarations := make([]candidateDeclaration, len(rows))
	for i, row := range rows {
		declarations[i] = declarationFromRow(row)
	}
	return validateModelCandidateConsistency(effective, declarations)
}

// validateCandidateSaveConsistency applies the two cross-entity rules to a
// candidate save: it re-reads the model and the sibling candidates — the
// other side of the pair the rules span — and swaps the saved candidate's
// entry for its effective post-save state. candidateID 0 means a create,
// whose declaration is appended to the siblings' list. The saved candidate's
// own temperature never enters this check: temperature is model-level
// metadata, validated where it is written (the model save path), and nothing
// a candidate save touches can change it.
func (s *ModelService) validateCandidateSaveConsistency(modelID, candidateID uint, effective candidateDeclaration) error {
	m, err := repository.FindModelByID(s.db, modelID)
	if err != nil {
		return err
	}
	rows, err := repository.ListModelCandidatesByModelID(s.db, modelID)
	if err != nil {
		return err
	}
	declarations := make([]candidateDeclaration, 0, len(rows)+1)
	replaced := false
	for _, row := range rows {
		if row.ID == candidateID {
			declarations = append(declarations, effective)
			replaced = true
			continue
		}
		declarations = append(declarations, declarationFromRow(row))
	}
	// A create's declaration joins the siblings at the end; candidateID 0
	// matches no stored row (primary keys start at 1), so replaced stays false
	// and the append below is what carries it in.
	if !replaced {
		declarations = append(declarations, effective)
	}
	return validateModelCandidateConsistency(*m, declarations)
}
