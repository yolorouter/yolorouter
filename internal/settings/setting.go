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

// RequestLogRetentionSetting is the typed snapshot of the global request-log
// rolling retention configuration: how many days of request_logs (and their
// body rows / stream files) the background cleanup keeps. Zero means keep
// forever — the shipped default, so an existing deployment changes nothing
// on upgrade. It intentionally has NO json tags — it is an internal transfer
// type; handlers wrap it in their own response DTO with json tags.
type RequestLogRetentionSetting struct {
	Days int
}

// RequestLogRetentionDefaultDays is the shipped default retention (keep
// forever), mirrored by the seeding migration (goose SQL cannot reference
// Go constants).
const RequestLogRetentionDefaultDays = 0

// DefaultRequestLogRetentionSetting returns the shipped default (keep
// forever). It is served when the row is absent — a database that predates
// the seeding migration — and as the fail-open value on a cold-cache
// refresh failure, so both paths behave exactly like a freshly seeded
// deployment and nothing is ever deleted by accident.
func DefaultRequestLogRetentionSetting() RequestLogRetentionSetting {
	return RequestLogRetentionSetting{Days: RequestLogRetentionDefaultDays}
}

// AdminAuditRetentionSetting is the typed snapshot of the global admin-audit
// retention configuration: how many days of admin_audit_logs rows the audit
// retention loop keeps. Zero means keep forever. The shipped default is 180
// days — unlike the request-log retention's keep-forever default, console
// audit volume is tiny and a bounded default keeps the trail honest without
// anyone opting in. Same no-json-tags convention as the pairs above.
type AdminAuditRetentionSetting struct {
	Days int
}

// AdminAuditRetentionDefaultDays is the shipped default audit retention,
// mirrored by the seeding migration (goose SQL cannot reference Go
// constants).
const AdminAuditRetentionDefaultDays = 180

// DefaultAdminAuditRetentionSetting returns the shipped default. It is
// served when the row is absent — a database that predates the seeding
// migration — and as the fail-open value on a cold-cache refresh failure,
// so both paths behave exactly like a freshly seeded deployment.
func DefaultAdminAuditRetentionSetting() AdminAuditRetentionSetting {
	return AdminAuditRetentionSetting{Days: AdminAuditRetentionDefaultDays}
}

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
