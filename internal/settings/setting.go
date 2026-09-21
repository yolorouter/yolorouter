// Package settings holds neutral DTO types shared across layers without
// pulling implementation dependencies (gateway imports this for its
// SettingsProvider interface; service imports it to implement that interface).
package settings

// CustomSystemPromptSetting is the typed snapshot of the global custom system
// prompt. Read and published as a whole to avoid torn reads between the
// enabled and text rows. It intentionally has NO json tags — it is an internal
// transfer type; handlers wrap it in their own response DTO with json tags.
type CustomSystemPromptSetting struct {
	Enabled bool
	Text    string
}

// VisionFallbackSetting is the typed snapshot of the global vision-fallback
// configuration, read as a whole for the same torn-read reason. An empty
// Model means the feature is off; an empty Prompt means "use
// VisionFallbackDefaultPrompt".
type VisionFallbackSetting struct {
	Model  string
	Prompt string
}

// KeyAutoRecoverySetting is the typed snapshot of the global key-auto-
// recovery configuration (the background loop that periodically retests
// provider keys the system kicked out of rotation), read as a whole for
// the same torn-read reason as the pairs above. It intentionally has NO
// json tags — it is an internal transfer type; handlers wrap it in their
// own response DTO with json tags.
type KeyAutoRecoverySetting struct {
	Enabled         bool
	IntervalMinutes int
}

// KeyAutoRecoveryDefaultIntervalMinutes is the shipped default interval,
// mirrored by the seeding migration (goose SQL cannot reference Go
// constants). Half an hour recovers a quota-reset key well within a
// monthly reset window while keeping probe traffic negligible.
const KeyAutoRecoveryDefaultIntervalMinutes = 30

// DefaultKeyAutoRecoverySetting returns the shipped default (enabled, 30
// minutes). It is served when the rows are absent — a database that
// predates the seeding migration — and as the fail-open value on a
// cold-cache refresh failure, so both paths behave exactly like a freshly
// seeded deployment.
func DefaultKeyAutoRecoverySetting() KeyAutoRecoverySetting {
	return KeyAutoRecoverySetting{
		Enabled:         true,
		IntervalMinutes: KeyAutoRecoveryDefaultIntervalMinutes,
	}
}

// VisionFallbackDefaultPrompt is the runtime fallback describe instruction,
// used only when the stored prompt is empty (never configured, or cleared on
// purpose). It is NOT what the console displays: the console prefills its
// own localized default texts from the frontend i18n bundle and saves them
// as real content, so this constant and those texts are separate artifacts
// that may evolve independently.
const VisionFallbackDefaultPrompt = "Describe this image in detail. Be specific about text, diagrams, charts, code, or any visual content that would be useful for a language model to understand."
