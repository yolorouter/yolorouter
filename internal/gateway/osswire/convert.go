// Package osswire is this repository's wiring of the kernel's ports:
// converters from the storage-facing model types to the kernel's
// vocabulary and a Store implementation over the repository. It is
// deployment glue — the kernel itself never imports the model or
// repository packages — so it stays out of any kernel sync.
package osswire

import (
	"github.com/yolorouter/yolorouter/internal/gateway/rows"
	"github.com/yolorouter/yolorouter/internal/model"
)

// APIKey copies the caller-credential face the kernel reads.
func APIKey(k *model.APIKey) *rows.APIKey {
	if k == nil {
		return nil
	}
	return &rows.APIKey{
		ID:                                k.ID,
		KeyHash:                           k.KeyHash,
		KeyPrefix:                         k.KeyPrefix,
		UserID:                            k.UserID,
		Status:                            k.Status,
		ExpiresAt:                         k.ExpiresAt,
		RPMLimit:                          k.RPMLimit,
		TPMLimit:                          k.TPMLimit,
		ConcurrencyLimit:                  k.ConcurrencyLimit,
		AllowAllModels:                    k.AllowAllModels,
		BudgetLimitMicros:                 k.BudgetLimitMicros,
		BudgetSpentMicros:                 k.BudgetSpentMicros,
		CustomSystemPromptEnabledOverride: k.CustomSystemPromptEnabledOverride,
		CustomSystemPromptEnabled:         k.CustomSystemPromptEnabled,
		CustomSystemPrompt:                k.CustomSystemPrompt,
		CompressEnabledOverride:           k.CompressEnabledOverride,
		CompressEnabled:                   k.CompressEnabled,
		CreatedAt:                         k.CreatedAt,
		UpdatedAt:                         k.UpdatedAt,
	}
}

// Provider copies the routing-relevant provider face.
func Provider(p *model.Provider) *rows.Provider {
	if p == nil {
		return nil
	}
	return &rows.Provider{
		ID:                 p.ID,
		Name:               p.Name,
		ProviderType:       p.ProviderType,
		BaseURL:            p.BaseURL,
		ProtocolEndpoints:  p.ProtocolEndpoints,
		ManagementStatus:   p.ManagementStatus,
		DestinationVersion: p.DestinationVersion,
		CreatedAt:          p.CreatedAt,
		UpdatedAt:          p.UpdatedAt,
	}
}

// ProviderKey copies one dispatchable credential row.
func ProviderKey(k *model.ProviderKey) rows.ProviderKey {
	return rows.ProviderKey{
		ID:                           k.ID,
		ProviderID:                   k.ProviderID,
		Label:                        k.Label,
		EncryptedKey:                 k.EncryptedKey,
		KeyPrefix:                    k.KeyPrefix,
		TestModel:                    k.TestModel,
		SortOrder:                    k.SortOrder,
		ManagementStatus:             k.ManagementStatus,
		VerificationStatus:           k.VerificationStatus,
		AuthorizedDestinationVersion: k.AuthorizedDestinationVersion,
		ConfigVersion:                k.ConfigVersion,
		TestGeneration:               k.TestGeneration,
		CreatedAt:                    k.CreatedAt,
		UpdatedAt:                    k.UpdatedAt,
	}
}

// ProviderKeys copies a key listing.
func ProviderKeys(ks []model.ProviderKey) []rows.ProviderKey {
	out := make([]rows.ProviderKey, 0, len(ks))
	for i := range ks {
		out = append(out, ProviderKey(&ks[i]))
	}
	return out
}

// Model copies the model row's routing face.
func Model(m *model.Model) rows.Model {
	return rows.Model{
		ID:                 m.ID,
		Name:               m.Name,
		ManagementStatus:   m.ManagementStatus,
		SchedulingMode:     rows.SchedulingMode(m.SchedulingMode),
		SupportsImageInput: m.SupportsImageInput,
		OutputModalities:   m.OutputModalities,
		CreatedAt:          m.CreatedAt,
		UpdatedAt:          m.UpdatedAt,
	}
}

// Candidate copies one routable mapping with its pricing snapshot.
func Candidate(c *model.ModelCandidate) rows.ModelCandidate {
	return rows.ModelCandidate{
		ID:                      c.ID,
		ModelID:                 c.ModelID,
		ProviderID:              c.ProviderID,
		ProviderModelName:       c.ProviderModelName,
		Provider:                Provider(c.Provider),
		InputPrice:              c.InputPrice,
		OutputPrice:             c.OutputPrice,
		CacheWritePrice:         c.CacheWritePrice,
		CacheReadPrice:          c.CacheReadPrice,
		MaxOutput:               c.MaxOutput,
		BillingMode:             c.BillingMode,
		ImagePricingTiers:       c.ImagePricingTiers,
		VideoPricingTiers:       c.VideoPricingTiers,
		AudioUnitPrice:          c.AudioUnitPrice,
		SupportsStreaming:       c.SupportsStreaming,
		SupportsFunctionCalling: c.SupportsFunctionCalling,
		ManagementStatus:        c.ManagementStatus,
		VerificationStatus:      c.VerificationStatus,
		SortOrder:               c.SortOrder,
		CreatedAt:               c.CreatedAt,
		UpdatedAt:               c.UpdatedAt,
	}
}

// Candidates copies a candidate listing.
func Candidates(cs []model.ModelCandidate) []rows.ModelCandidate {
	out := make([]rows.ModelCandidate, 0, len(cs))
	for i := range cs {
		out = append(out, Candidate(&cs[i]))
	}
	return out
}
