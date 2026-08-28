// Package errcode defines system error codes.
package errcode

import "errors"

const (
	Success = 0

	// === Account/session errors (10xxx) — "auth and account security model" ===
	AccountInvalidCredentials = 10001
	AccountDisabled           = 10002
	AccountSessionInvalid     = 10003
	AccountCSRFInvalid        = 10004
	AccountLoginLocked        = 10005 // too many failed login attempts, temporarily locked
	AccountLastAdminProtected = 10006 // operation would leave zero active administrators, rejected outright
	AccountSetupAlreadyDone   = 10007 // first-run setup wizard already completed, cannot create the first admin again
	AccountSetupTokenInvalid  = 10008 // first-run setup wizard token missing or incorrect
	AccountPageForbidden      = 10009 // page-level RBAC: the user's group has no access to this admin page

	// External-login (OAuth) errors, same 10xxx family — they are auth
	// failures from the account model's point of view.
	OAuthProviderNotFound      = 10010 // provider slug unknown or provider disabled
	OAuthStateInvalid          = 10011 // state missing, expired, already consumed, or issued for another provider
	OAuthExchangeFailed        = 10012 // authorization-code -> token exchange with the identity provider failed
	OAuthUserinfoFailed        = 10013 // userinfo fetch failed or the mapped user id field was empty
	OAuthProviderSlugTaken     = 10014 // another provider already uses this slug
	OAuthProviderConfigInvalid = 10015 // provider configuration rejected: required field blank, or an endpoint is not an absolute http(s) URL
	OAuthDiscoveryFailed       = 10016 // OIDC well-known discovery document fetch/parse failed

	AccountSelfOperation       = 10017 // admins cannot change their own status or role — another admin must do it
	AccountUserNotFound        = 10018 // target user id does not exist
	AccountBootstrapProtected  = 10019 // the bootstrap account is the OAuth-failure escape hatch — it cannot be disabled or demoted
	AccountUsernameTaken       = 10020 // another account (local or externally provisioned) already uses that username
	AccountPasswordResetDenied = 10021 // password resets are reserved to the bootstrap administrator, for other local accounts only
	AccountProfileEditDenied   = 10022 // profile edits (display name, email) are reserved to the bootstrap administrator, for other accounts only

	// === API Key errors (11xxx) — "API Key security model" ===
	APIKeyNotFound             = 11001
	APIKeyInvalid              = 11002
	APIKeyExpired              = 11003
	APIKeyRevoked              = 11004
	APIKeyRateLimitedRPM       = 11005
	APIKeyRateLimitedTPM       = 11006
	APIKeyRateLimitedConc      = 11007
	APIKeyBudgetExceeded       = 11008
	APIKeyEmptyAllowlist       = 11009
	CustomSystemPromptTooLong  = 11010 // custom system prompt text exceeds the max rune length
	CustomSystemPromptEmpty    = 11011 // enabled is true but the prompt text is empty
	CustomSystemPromptConflict = 11012 // optimistic-lock CAS miss on system_settings PUT (another writer committed first)
	APIKeyConflict             = 11013 // optimistic-lock CAS miss on api_keys PATCH (another writer committed first)
	InputCompressionConflict   = 11014 // optimistic-lock CAS miss on input_compression_enabled PUT (another writer committed first)
	CompressEnabledRequired    = 11015 // compress_enabled_override is true but compress_enabled is not supplied
	APIKeyPlaintextUnavailable = 11016 // the key predates the encrypted_key column (migration 00021), so its plaintext was never stored and cannot be revealed
	VisionFallbackConflict     = 11017 // optimistic-lock CAS miss on the vision_fallback settings pair PUT (another writer committed first)
	VisionFallbackModelUnknown = 11018 // vision_fallback_model names a model this gateway has no record of

	// === Provider errors (12xxx) ===
	ProviderNotFound   = 12001
	ProviderNameTaken  = 12002
	ProviderDisabled   = 12003
	ProviderTestFailed = 12004
	// 12005 and 12008 are retired: they guarded a "cannot delete a provider
	// in use" rule that no longer exists — deletion now cascades to the
	// provider's keys and model mappings and leaves request history behind.
	// Do not reuse the numbers.
	ProviderNoTestableModel  = 12006 // openai/anthropic type connection test requires at least one enabled model
	ProviderMasterKeyMissing = 12007 // AES-256-GCM master key not configured, cannot encrypt/decrypt the upstream API Key
	ProviderKeyNotFound      = 12009 // the given Key ID does not exist under this provider
	ProviderKeyLabelTaken    = 12010 // label is already taken by another Key within the same provider
	ProviderKeyNotVerified   = 12011 // attempt to enable a Key whose verification_status is not "passed"
	ProviderKeyNeedsReentry  = 12012 // authorized_destination_version differs from the current destination_version, plaintext must be resubmitted
	ProviderKeyTooShort      = 12013 // Key plaintext is shorter than minKeyPlaintextLength (normally already blocked by Gin binding; this is a defensive fallback)
	ProviderKeyTestNotSaved  = 12014 // the test network call finished but the Key was modified concurrently while writing back the result (config_version changed), so the CAS missed and the result was not persisted; retry needed
	ProviderProtocolInvalid  = 12015 // provider_type or protocol_endpoints failed validation (unsupported protocol name, malformed JSON, or a non-absolute-http(s) endpoint URL)

	ModelNotFound               = 12101 // the public model name does not exist
	ModelNameTaken              = 12102 // the public model name is already taken (globally unique)
	ModelCandidateNotFound      = 12103 // the given candidate ID does not exist under this model
	ModelCandidateProviderTaken = 12104 // this provider is already a candidate for this model (one candidate per provider per model)
	ModelCandidateNotVerified   = 12105 // attempt to enable a candidate whose verification_status is not "passed"
	// 12106 (formerly ModelCandidatePriceMissing) was never returned by any
	// service-layer code — input_price/output_price are NOT NULL DEFAULT 0 in
	// the schema (a price of 0 is allowed), so at the data-model level the
	// "not filled in" state cannot be distinguished from "explicitly set to 0"
	// and is fundamentally unreachable; this dead branch was removed rather
	// than kept as an error code that can never fire.

	ModelSchedulingModeInvalid = 12107 // scheduling_mode is not one of failover/balanced

	// === User group errors (13xxx) — user group serving three roles at once ===
	UserGroupNotFound       = 13001
	UserGroupNameTaken      = 13002
	UserGroupHasMembers     = 13003 // group still has members, cannot delete
	UserGroupInvalidPage    = 13004 // page_permissions contains a page key that does not exist
	UserGroupHasRequestLogs = 13005 // already has request logs (cost snapshots), cannot delete
	UserNotFound            = 13101
	UserUsernameTaken       = 13102
	UserDisabled            = 13103 // target user is disabled; this operation cannot be performed for them (e.g. issuing a new API Key)

	// === Relay/gateway errors (14xxx) ===
	// Gateway responses use the upstream's native wire format and do not go
	// through the pkg/response / pkg/errcode envelope, so this segment
	// currently has only one code that is actually used: RequestLogNotFound.
	// The earlier placeholders RelayModelNotAllowed/RelayUnsupportedField/
	// RelayUpstreamError/RelayNoAvailableProvider were never referenced by
	// internal/relay (the relay package hardcodes OpenAI error strings
	// directly), were dead code, and have been removed.
	RequestLogNotFound = 14005 // request log detail query, id does not exist

	// === Self-update errors (15xxx) ===
	SystemUpdateUnsupported = 15001 // in-place update is not available in this runtime (container / windows / disabled / non-release build / capability-bearing binary); the version endpoint's update_mode says which
	SystemUpdateFailed      = 15002 // the update run itself failed (download, checksum, or binary replacement); the server log carries the specific cause
	SystemUpdateInProgress  = 15003 // an update is already running, or one was applied and the process is restarting; a second run before the restart would overwrite the rollback backup with the new binary

	// === System internal errors (50001-50099) ===
	InternalError      = 50001
	DatabaseError      = 50002
	InvalidParam       = 50003
	ConfigError        = 50004
	ServiceUnavailable = 50005
)

// Route-level generic errors (infrastructure, not tied to any specific
// domain). InternalError already exists above (50001) for route-level 500
// responses too, so only these three are actually new.
const (
	RouteNotFound         = 90001
	MethodNotAllowed      = 90002
	RequestEntityTooLarge = 90003
)

// ErrorMessages maps error codes to human-readable messages.
var ErrorMessages = map[int]string{
	Success: "success",

	AccountInvalidCredentials:  "invalid username or password",
	AccountDisabled:            "account is disabled",
	AccountSessionInvalid:      "session invalid or expired",
	AccountCSRFInvalid:         "csrf check failed",
	AccountLoginLocked:         "account temporarily locked due to repeated login failures",
	AccountLastAdminProtected:  "operation refused: would leave no active administrator",
	AccountSetupAlreadyDone:    "first-run setup already completed",
	AccountSetupTokenInvalid:   "setup token invalid or missing",
	AccountPageForbidden:       "your user group does not have access to this page",
	OAuthProviderNotFound:      "login provider not found or disabled",
	OAuthStateInvalid:          "login state invalid or expired, please retry",
	OAuthExchangeFailed:        "identity provider token exchange failed",
	OAuthUserinfoFailed:        "identity provider did not return a usable user identity",
	OAuthProviderSlugTaken:     "another login provider already uses this slug",
	OAuthProviderConfigInvalid: "provider configuration invalid: required field blank or endpoint not an absolute http(s) URL",
	OAuthDiscoveryFailed:       "OIDC discovery document fetch failed",
	AccountSelfOperation:       "operation refused: you cannot change your own status or role",
	AccountUserNotFound:        "user not found",
	AccountBootstrapProtected:  "operation refused: the bootstrap account cannot be disabled or demoted",
	AccountUsernameTaken:       "username already taken",
	AccountPasswordResetDenied: "operation refused: only the setup administrator may reset other local accounts' passwords",
	AccountProfileEditDenied:   "operation refused: only the setup administrator may edit other accounts' profiles",

	APIKeyNotFound:             "api key not found",
	APIKeyInvalid:              "api key invalid",
	APIKeyExpired:              "api key expired",
	APIKeyRevoked:              "api key revoked",
	APIKeyRateLimitedRPM:       "rate limit exceeded (requests per minute)",
	APIKeyRateLimitedTPM:       "rate limit exceeded (tokens per minute)",
	APIKeyRateLimitedConc:      "rate limit exceeded (concurrent requests)",
	APIKeyBudgetExceeded:       "budget limit exceeded",
	APIKeyEmptyAllowlist:       "model_ids must contain at least one model unless allow_all_models is true",
	CustomSystemPromptTooLong:  "custom system prompt is too long",
	CustomSystemPromptEmpty:    "custom system prompt text must not be empty when enabled",
	CustomSystemPromptConflict: "custom system prompt was modified concurrently, please refresh and retry",
	APIKeyConflict:             "api key was modified concurrently, please refresh and retry",
	InputCompressionConflict:   "input compression setting was modified concurrently, please refresh and retry",
	CompressEnabledRequired:    "compress_enabled must be set when compress_enabled_override is true",
	APIKeyPlaintextUnavailable: "this key was created before the reveal feature and its full value cannot be recovered, please create a new one",
	VisionFallbackConflict:     "vision fallback settings were modified concurrently, please refresh and retry",
	VisionFallbackModelUnknown: "vision fallback model is not a model configured on this gateway",

	ProviderNotFound:         "provider not found",
	ProviderNameTaken:        "provider name already taken",
	ProviderDisabled:         "provider is disabled",
	ProviderTestFailed:       "provider connection test failed",
	ProviderNoTestableModel:  "provider has no enabled model to test with",
	ProviderMasterKeyMissing: "provider master key not configured",
	ProviderKeyNotFound:      "provider key not found",
	ProviderKeyLabelTaken:    "provider key label already taken",
	ProviderKeyNotVerified:   "cannot enable a key that has not passed verification",
	ProviderKeyNeedsReentry:  "provider address changed, please resubmit the key plaintext",
	ProviderKeyTooShort:      "key plaintext is too short",
	ProviderKeyTestNotSaved:  "test result not saved because the key was modified concurrently, please retry",
	ProviderProtocolInvalid:  "invalid provider_type or protocol_endpoints",

	ModelNotFound:               "model not found",
	ModelNameTaken:              "model name already taken",
	ModelCandidateNotFound:      "model candidate not found",
	ModelCandidateProviderTaken: "this provider is already a candidate for this model",
	ModelCandidateNotVerified:   "cannot enable a candidate that has not passed the basic test",
	ModelSchedulingModeInvalid:  "scheduling mode must be failover or balanced",

	UserGroupNotFound:       "user group not found",
	UserGroupNameTaken:      "user group name already taken",
	UserGroupHasMembers:     "user group still has members, reassign them first",
	UserGroupInvalidPage:    "page_permissions contains an unrecognized page key",
	UserGroupHasRequestLogs: "user group has existing request logs, cannot be deleted",
	UserNotFound:            "user not found",
	UserUsernameTaken:       "username already taken",
	UserDisabled:            "target user is disabled",

	RequestLogNotFound: "request log not found",

	SystemUpdateUnsupported: "in-place update is not available in this runtime",
	SystemUpdateFailed:      "update failed; check the server log for details",
	SystemUpdateInProgress:  "an update is already in progress or applied; the service is about to restart",

	InternalError:      "internal error",
	DatabaseError:      "database error",
	InvalidParam:       "invalid parameter",
	ConfigError:        "configuration error",
	ServiceUnavailable: "service unavailable",

	RouteNotFound:         "route not found",
	MethodNotAllowed:      "method not allowed",
	RequestEntityTooLarge: "request entity too large",
}

// Sentinel errors for service layer comparisons. Text is derived from
// ErrorMessages so each message string has exactly one source of truth.
var (
	ErrAccountInvalidCredentials  = errors.New(ErrorMessages[AccountInvalidCredentials])
	ErrAccountDisabled            = errors.New(ErrorMessages[AccountDisabled])
	ErrAccountSessionInvalid      = errors.New(ErrorMessages[AccountSessionInvalid])
	ErrAccountLoginLocked         = errors.New(ErrorMessages[AccountLoginLocked])
	ErrAccountLastAdminProtected  = errors.New(ErrorMessages[AccountLastAdminProtected])
	ErrAccountSelfOperation       = errors.New(ErrorMessages[AccountSelfOperation])
	ErrAccountUserNotFound        = errors.New(ErrorMessages[AccountUserNotFound])
	ErrAccountBootstrapProtected  = errors.New(ErrorMessages[AccountBootstrapProtected])
	ErrAccountUsernameTaken       = errors.New(ErrorMessages[AccountUsernameTaken])
	ErrAccountPasswordResetDenied = errors.New(ErrorMessages[AccountPasswordResetDenied])
	ErrAccountProfileEditDenied   = errors.New(ErrorMessages[AccountProfileEditDenied])
	ErrAccountSetupAlreadyDone    = errors.New(ErrorMessages[AccountSetupAlreadyDone])
	ErrOAuthProviderNotFound      = errors.New(ErrorMessages[OAuthProviderNotFound])
	ErrOAuthStateInvalid          = errors.New(ErrorMessages[OAuthStateInvalid])
	ErrOAuthExchangeFailed        = errors.New(ErrorMessages[OAuthExchangeFailed])
	ErrOAuthUserinfoFailed        = errors.New(ErrorMessages[OAuthUserinfoFailed])
	ErrOAuthProviderSlugTaken     = errors.New(ErrorMessages[OAuthProviderSlugTaken])
	ErrOAuthProviderConfigInvalid = errors.New(ErrorMessages[OAuthProviderConfigInvalid])
	ErrOAuthDiscoveryFailed       = errors.New(ErrorMessages[OAuthDiscoveryFailed])

	ErrAPIKeyNotFound             = errors.New(ErrorMessages[APIKeyNotFound])
	ErrAPIKeyInvalid              = errors.New(ErrorMessages[APIKeyInvalid])
	ErrAPIKeyExpired              = errors.New(ErrorMessages[APIKeyExpired])
	ErrAPIKeyRevoked              = errors.New(ErrorMessages[APIKeyRevoked])
	ErrAPIKeyRateLimitedRPM       = errors.New(ErrorMessages[APIKeyRateLimitedRPM])
	ErrAPIKeyRateLimitedTPM       = errors.New(ErrorMessages[APIKeyRateLimitedTPM])
	ErrAPIKeyRateLimitedConc      = errors.New(ErrorMessages[APIKeyRateLimitedConc])
	ErrAPIKeyBudgetExceeded       = errors.New(ErrorMessages[APIKeyBudgetExceeded])
	ErrAPIKeyEmptyAllowlist       = errors.New(ErrorMessages[APIKeyEmptyAllowlist])
	ErrCustomSystemPromptTooLong  = errors.New(ErrorMessages[CustomSystemPromptTooLong])
	ErrCustomSystemPromptEmpty    = errors.New(ErrorMessages[CustomSystemPromptEmpty])
	ErrCustomSystemPromptConflict = errors.New(ErrorMessages[CustomSystemPromptConflict])
	ErrAPIKeyConflict             = errors.New(ErrorMessages[APIKeyConflict])
	ErrInputCompressionConflict   = errors.New(ErrorMessages[InputCompressionConflict])
	ErrCompressEnabledRequired    = errors.New(ErrorMessages[CompressEnabledRequired])
	ErrAPIKeyPlaintextUnavailable = errors.New(ErrorMessages[APIKeyPlaintextUnavailable])
	ErrVisionFallbackConflict     = errors.New(ErrorMessages[VisionFallbackConflict])
	ErrVisionFallbackModelUnknown = errors.New(ErrorMessages[VisionFallbackModelUnknown])

	ErrProviderNotFound         = errors.New(ErrorMessages[ProviderNotFound])
	ErrProviderNameTaken        = errors.New(ErrorMessages[ProviderNameTaken])
	ErrProviderDisabled         = errors.New(ErrorMessages[ProviderDisabled])
	ErrProviderTestFailed       = errors.New(ErrorMessages[ProviderTestFailed])
	ErrProviderNoTestableModel  = errors.New(ErrorMessages[ProviderNoTestableModel])
	ErrProviderMasterKeyMissing = errors.New(ErrorMessages[ProviderMasterKeyMissing])
	ErrProviderKeyNotFound      = errors.New(ErrorMessages[ProviderKeyNotFound])
	ErrProviderKeyLabelTaken    = errors.New(ErrorMessages[ProviderKeyLabelTaken])
	ErrProviderKeyNotVerified   = errors.New(ErrorMessages[ProviderKeyNotVerified])
	ErrProviderKeyNeedsReentry  = errors.New(ErrorMessages[ProviderKeyNeedsReentry])
	ErrProviderKeyTooShort      = errors.New(ErrorMessages[ProviderKeyTooShort])
	ErrProviderKeyTestNotSaved  = errors.New(ErrorMessages[ProviderKeyTestNotSaved])
	ErrProviderProtocolInvalid  = errors.New(ErrorMessages[ProviderProtocolInvalid])

	ErrModelNotFound               = errors.New(ErrorMessages[ModelNotFound])
	ErrModelNameTaken              = errors.New(ErrorMessages[ModelNameTaken])
	ErrModelCandidateNotFound      = errors.New(ErrorMessages[ModelCandidateNotFound])
	ErrModelCandidateProviderTaken = errors.New(ErrorMessages[ModelCandidateProviderTaken])
	ErrModelCandidateNotVerified   = errors.New(ErrorMessages[ModelCandidateNotVerified])
	ErrModelSchedulingModeInvalid  = errors.New(ErrorMessages[ModelSchedulingModeInvalid])

	ErrUserGroupNotFound       = errors.New(ErrorMessages[UserGroupNotFound])
	ErrUserGroupNameTaken      = errors.New(ErrorMessages[UserGroupNameTaken])
	ErrUserGroupHasMembers     = errors.New(ErrorMessages[UserGroupHasMembers])
	ErrUserGroupInvalidPage    = errors.New(ErrorMessages[UserGroupInvalidPage])
	ErrUserGroupHasRequestLogs = errors.New(ErrorMessages[UserGroupHasRequestLogs])
	ErrUserNotFound            = errors.New(ErrorMessages[UserNotFound])
	ErrUserUsernameTaken       = errors.New(ErrorMessages[UserUsernameTaken])
	ErrUserDisabled            = errors.New(ErrorMessages[UserDisabled])

	ErrRequestLogNotFound = errors.New(ErrorMessages[RequestLogNotFound])
)

// GetMessage returns the message for the given error code.
func GetMessage(code int) string {
	if msg, ok := ErrorMessages[code]; ok {
		return msg
	}
	return "unknown error"
}
