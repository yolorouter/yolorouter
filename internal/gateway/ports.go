package gateway

import (
	"context"
	"time"

	"github.com/yolorouter/yolorouter/internal/gateway/rows"
)

// The kernel's vocabulary lives in the rows leaf so the attempt
// subpackage can share it without a cycle; the bare names here keep the
// kernel's own reads unqualified.
type (
	Provider            = rows.Provider
	ProviderKey         = rows.ProviderKey
	ModelCandidate      = rows.ModelCandidate
	Model               = rows.Model
	APIKey              = rows.APIKey
	VideoTask           = rows.VideoTask
	SchedulingMode      = rows.SchedulingMode
	QueryResult         = rows.QueryResult
	BudgetExceededError = rows.BudgetExceededError
	ImagePricingTiers   = rows.ImagePricingTiers
	ImagePricingTier    = rows.ImagePricingTier
	VideoPricingTiers   = rows.VideoPricingTiers
	VideoPricingTier    = rows.VideoPricingTier
)

const (
	ModelStatusEnabled              = rows.ModelStatusEnabled
	ModelCandidateStatusEnabled     = rows.ModelCandidateStatusEnabled
	ModelVerificationStatusPassed   = rows.ModelVerificationStatusPassed
	VerificationStatusPassed        = rows.VerificationStatusPassed
	ProviderStatusEnabled           = rows.ProviderStatusEnabled
	ProviderKeyStatusEnabled        = rows.ProviderKeyStatusEnabled
	APIKeyStatusRevoked             = rows.APIKeyStatusRevoked
	APIKeyStatusActive              = rows.APIKeyStatusActive
	VerificationStatusFailed        = rows.VerificationStatusFailed
	ModelVerificationStatusUntested = rows.ModelVerificationStatusUntested
	ModelVerificationStatusFailed   = rows.ModelVerificationStatusFailed
	ProviderStatusDisabled          = rows.ProviderStatusDisabled
	ProviderKeyStatusDisabled       = rows.ProviderKeyStatusDisabled
	ModelStatusDisabled             = rows.ModelStatusDisabled
	ModelCandidateStatusDisabled    = rows.ModelCandidateStatusDisabled
	BillingModeToken                = rows.BillingModeToken
	BillingModeImage                = rows.BillingModeImage
	BillingModeVideo                = rows.BillingModeVideo
	BillingModeAudio                = rows.BillingModeAudio
	MeterRequests                   = rows.MeterRequests
	MeterTokens                     = rows.MeterTokens
	VideoTaskPending                = rows.VideoTaskPending
	VideoTaskCompleted              = rows.VideoTaskCompleted
	VideoTaskFailed                 = rows.VideoTaskFailed
	VideoTaskExpired                = rows.VideoTaskExpired
	VideoTaskCancelled              = rows.VideoTaskCancelled
	RequestLogSourceVisionFallback  = rows.RequestLogSourceVisionFallback
	OutputModalityText              = rows.OutputModalityText
	OutputModalityImage             = rows.OutputModalityImage
	OutputModalityVideo             = rows.OutputModalityVideo
	OutputModalityAudio             = rows.OutputModalityAudio
	ModelSchedulingModeFailover     = rows.ModelSchedulingModeFailover
	ModelSchedulingModeBalanced     = rows.ModelSchedulingModeBalanced
	perImageMode                    = rows.PerImageMode
)

var (
	ErrVideoTaskNotFound   = rows.ErrVideoTaskNotFound
	ErrNotFound            = rows.ErrNotFound
	ParseImagePricingTiers = rows.ParseImagePricingTiers
	ParseVideoPricingTiers = rows.ParseVideoPricingTiers
	NormalizeBillingMode   = rows.NormalizeBillingMode
)

// Store is every database touch the kernel makes, behind one port. A
// deployment implements it against its own storage, converting rows to
// the vocabulary above; the kernel never sees a query.
type Store interface {
	FindModelByName(ctx context.Context, name string) (*Model, error)
	ListModels(ctx context.Context) ([]Model, error)
	FindAPIKeyByID(ctx context.Context, id uint) (*APIKey, error)
	FindAPIKeyModelIDs(ctx context.Context, apiKeyID uint) ([]uint, error)
	HasAPIKeyModelAccess(ctx context.Context, apiKeyID, modelID uint) (bool, error)
	ListModelCandidatesByModelID(ctx context.Context, modelID uint) ([]ModelCandidate, error)
	ListProviderKeysByProvider(ctx context.Context, providerID uint) ([]ProviderKey, error)
	FindProviderByID(ctx context.Context, id uint) (*Provider, error)
	FindProviderKeyByID(ctx context.Context, id uint) (*ProviderKey, error)
	MarkProviderKeyVerificationFailedIfCurrent(ctx context.Context, keyID uint, providerDestinationVersion, configVersion, testGeneration int, now time.Time) (applied bool, err error)
	UpsertObservedRateLimit(ctx context.Context, keyID uint, meter string, limit *int64, windowSecs *int64, remaining *int64, resetAt *time.Time, now time.Time) error
	IncrementAPIKeyBudgetSpent(ctx context.Context, apiKeyID uint, micros int64) error
}

// VideoTasks is the video job domain behind a kernel port: the store
// the video modality submits through, the budget door its speech
// sibling shares, and the getter the job resource routes read. The
// poller that drives upstream task state is kernel-side (see
// NewVideoTaskPoller); this port is the persistent domain around it.
type VideoTasks interface {
	Create(ctx context.Context, task *VideoTask, now time.Time) error
	PrecheckBudget(ctx context.Context, apiKeyID uint, modelName, size string, seconds int) error
	Get(ctx context.Context, apiKeyID uint, id string, now time.Time) (*VideoTask, error)
}

// UpstreamVideoTaskPoller is the exported name of the kernel-side task
// poller a deployment wraps for its video job domain.
type UpstreamVideoTaskPoller = videoTaskQuerier
