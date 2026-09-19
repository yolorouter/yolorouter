package osswire

import (
	"context"
	"errors"
	"time"

	"github.com/yolorouter/yolorouter/internal/gateway/rows"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"

	"gorm.io/gorm"
)

// Store implements the kernel's Store over this deployment's repository,
// converting every row into the kernel's vocabulary on the way out.
type Store struct {
	db *gorm.DB
}

// NewStore wires the kernel's data port to the repository.
func NewStore(db *gorm.DB) *Store {
	return &Store{db: db}
}

func (s *Store) FindModelByName(ctx context.Context, name string) (*rows.Model, error) {
	m, err := repository.FindModelByName(s.db.WithContext(ctx), name)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, rows.ErrNotFound
		}
		return nil, err
	}
	out := Model(m)
	return &out, nil
}

func (s *Store) ListModels(ctx context.Context) ([]rows.Model, error) {
	all, err := repository.ListModels(s.db.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	out := make([]rows.Model, 0, len(all))
	for i := range all {
		out = append(out, Model(&all[i]))
	}
	return out, nil
}

func (s *Store) FindAPIKeyByID(ctx context.Context, id uint) (*rows.APIKey, error) {
	k, err := repository.FindAPIKeyByID(s.db.WithContext(ctx), id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, rows.ErrNotFound
		}
		return nil, err
	}
	return APIKey(k), nil
}

func (s *Store) FindAPIKeyModelIDs(ctx context.Context, apiKeyID uint) ([]uint, error) {
	return repository.FindAPIKeyModelIDs(s.db.WithContext(ctx), apiKeyID)
}

func (s *Store) HasAPIKeyModelAccess(ctx context.Context, apiKeyID, modelID uint) (bool, error) {
	return repository.HasAPIKeyModelAccess(s.db.WithContext(ctx), apiKeyID, modelID)
}

func (s *Store) ListModelCandidatesByModelID(ctx context.Context, modelID uint) ([]rows.ModelCandidate, error) {
	cs, err := repository.ListModelCandidatesByModelID(s.db.WithContext(ctx), modelID)
	if err != nil {
		return nil, err
	}
	return Candidates(cs), nil
}

func (s *Store) ListProviderKeysByProvider(ctx context.Context, providerID uint) ([]rows.ProviderKey, error) {
	ks, err := repository.ListProviderKeysByProvider(s.db.WithContext(ctx), providerID)
	if err != nil {
		return nil, err
	}
	return ProviderKeys(ks), nil
}

func (s *Store) FindProviderByID(ctx context.Context, id uint) (*rows.Provider, error) {
	var p model.Provider
	if err := s.db.WithContext(ctx).First(&p, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, rows.ErrNotFound
		}
		return nil, err
	}
	return Provider(&p), nil
}

func (s *Store) FindProviderKeyByID(ctx context.Context, id uint) (*rows.ProviderKey, error) {
	var k model.ProviderKey
	if err := s.db.WithContext(ctx).First(&k, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, rows.ErrNotFound
		}
		return nil, err
	}
	out := ProviderKey(&k)
	return &out, nil
}

func (s *Store) MarkProviderKeyVerificationFailedIfCurrent(ctx context.Context, keyID uint, providerDestinationVersion, configVersion, testGeneration int, now time.Time) (bool, error) {
	return repository.MarkProviderKeyVerificationFailedIfCurrent(s.db.WithContext(ctx), keyID, providerDestinationVersion, configVersion, testGeneration, now)
}

func (s *Store) UpsertObservedRateLimit(ctx context.Context, keyID uint, meter string, limit *int64, windowSecs *int64, remaining *int64, resetAt *time.Time, now time.Time) error {
	return repository.UpsertObservedRateLimit(s.db.WithContext(ctx), keyID, meter, limit, windowSecs, remaining, resetAt, now)
}

func (s *Store) IncrementAPIKeyBudgetSpent(ctx context.Context, apiKeyID uint, micros int64) error {
	return repository.IncrementAPIKeyBudgetSpent(s.db, apiKeyID, micros)
}
