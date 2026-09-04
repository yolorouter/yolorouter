package gateway

// The speech door's budget pre-gate, in the shape the video door established:
// before anything is dialled, the cheapest estimate across the model's
// enabled audio candidates is held against the caller key's ceiling, because
// a synthesis the caller could never pay for still renders at the operator's
// cost. There is no in-flight reservation term — speech settles
// synchronously in the request's own settlement, so there is no window
// between "rendered" and "billed" for a second request to double-spend.

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
)

// audioBudgetExceededError is the certain refusal the pre-gate answers with.
type audioBudgetExceededError struct {
	Limit int64
	Spent int64
	Ask   int64
}

func (e *audioBudgetExceededError) Error() string {
	return fmt.Sprintf("audio budget exceeded: the cheapest estimate for this request (%d micros) would pass the key's limit (spent %d of %d)",
		e.Ask, e.Spent, e.Limit)
}

// audioBudgetPrecheck is the door-level seam, wired by NewService and
// overridable in tests. Nil in a bare assembly — the door stays silent, the
// same leniency the video precheck takes when it cannot price certainly.
var audioBudgetPrecheck func(ctx context.Context, apiKeyID uint, modelName, input string) error

// precheckAudioBudget holds the cheapest enabled audio candidate's estimate
// against the key's limit. A refusal is certain — every priced candidate's
// estimate breaks the ceiling — so the caller is turned away before any
// synthesis exists that nobody would be billed for. Anything else is
// advisory: the authoritative accounting runs at settle, where the routed
// candidate's own price and meter decide.
func (s *Service) precheckAudioBudget(ctx context.Context, apiKeyID uint, modelName, input string) error {
	m, err := repository.FindModelByName(s.db.WithContext(ctx), modelName)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// Routing will answer a name it cannot resolve; a precheck
			// cannot price a model that does not exist, and refusing here
			// would shadow that answer with a budget-shaped one.
			return nil
		}
		return err
	}
	cands, err := repository.ListModelCandidatesByModelID(s.db.WithContext(ctx), m.ID)
	if err != nil {
		return err
	}
	minAsk := int64(-1)
	// An enabled audio candidate without a price, if routed, bills as
	// unknown rather than free — so while one exists the refusal is not
	// certain, and the door stays silent rather than refusing a call that
	// might cost nothing.
	unpricedEnabled := false
	for i := range cands {
		cand := &cands[i]
		if cand.ManagementStatus != model.ModelCandidateStatusEnabled {
			continue
		}
		if model.NormalizeBillingMode(cand.BillingMode) != model.BillingModeAudio {
			continue
		}
		if cand.AudioUnitPrice == nil {
			unpricedEnabled = true
			continue
		}
		ask := audioMicros(*cand.AudioUnitPrice, speechDialectFor(cand.Provider.BaseURL).Meter(input))
		if ask < 0 {
			// An absurd price overflowing the micros scale cannot be held
			// against a ceiling; the settle path prices it unknown, and the
			// door's refusal must not fire on a number nobody can bill.
			continue
		}
		if minAsk < 0 || ask < minAsk {
			minAsk = ask
		}
	}
	if minAsk < 0 || unpricedEnabled {
		return nil
	}
	key, err := repository.FindAPIKeyByID(s.db.WithContext(ctx), apiKeyID)
	if err != nil {
		return err
	}
	if key.BudgetLimitMicros == nil || *key.BudgetLimitMicros <= 0 {
		return nil
	}
	// The boundary matches the kernel's own admission gate: reaching the
	// limit exactly is allowed and the NEXT ask is refused.
	if key.BudgetSpentMicros+minAsk > *key.BudgetLimitMicros {
		return &audioBudgetExceededError{Limit: *key.BudgetLimitMicros, Spent: key.BudgetSpentMicros, Ask: minAsk}
	}
	return nil
}
