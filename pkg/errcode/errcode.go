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

	// === API Key errors (11xxx) — "API Key security model" ===
	APIKeyNotFound        = 11001
	APIKeyInvalid         = 11002
	APIKeyExpired         = 11003
	APIKeyRevoked         = 11004
	APIKeyRateLimitedRPM  = 11005
	APIKeyRateLimitedTPM  = 11006
	APIKeyRateLimitedConc = 11007
	APIKeyBudgetExceeded  = 11008

	// === Provider errors (12xxx) ===
	ProviderNotFound         = 12001
	ProviderNameTaken        = 12002
	ProviderDisabled         = 12003
	ProviderTestFailed       = 12004
	ProviderHasModels        = 12005 // still has models under it, cannot delete
	ProviderNoTestableModel  = 12006 // openai/anthropic type connection test requires at least one enabled model
	ProviderMasterKeyMissing = 12007 // AES-256-GCM master key not configured, cannot encrypt/decrypt the upstream API Key
	ProviderHasRequestLogs   = 12008 // already has request logs, cannot delete (the FK would reject it; surface a clear error early here)
	ProviderKeyNotFound      = 12009 // the given Key ID does not exist under this provider
	ProviderKeyLabelTaken    = 12010 // label is already taken by another Key within the same provider
	ProviderKeyNotVerified   = 12011 // attempt to enable a Key whose verification_status is not "passed"
	ProviderKeyNeedsReentry  = 12012 // authorized_destination_version differs from the current destination_version, plaintext must be resubmitted
	ProviderKeyTooShort      = 12013 // Key plaintext is shorter than minKeyPlaintextLength (normally already blocked by Gin binding; this is a defensive fallback)
	ProviderKeyTestNotSaved  = 12014 // the test network call finished but the Key was modified concurrently while writing back the result (config_version changed), so the CAS missed and the result was not persisted; retry needed

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

	AccountInvalidCredentials: "invalid username or password",
	AccountDisabled:           "account is disabled",
	AccountSessionInvalid:     "session invalid or expired",
	AccountCSRFInvalid:        "csrf check failed",
	AccountLoginLocked:        "account temporarily locked due to repeated login failures",
	AccountLastAdminProtected: "operation refused: would leave no active administrator",
	AccountSetupAlreadyDone:   "first-run setup already completed",
	AccountSetupTokenInvalid:  "setup token invalid or missing",
	AccountPageForbidden:      "your user group does not have access to this page",

	APIKeyNotFound:        "api key not found",
	APIKeyInvalid:         "api key invalid",
	APIKeyExpired:         "api key expired",
	APIKeyRevoked:         "api key revoked",
	APIKeyRateLimitedRPM:  "rate limit exceeded (requests per minute)",
	APIKeyRateLimitedTPM:  "rate limit exceeded (tokens per minute)",
	APIKeyRateLimitedConc: "rate limit exceeded (concurrent requests)",
	APIKeyBudgetExceeded:  "budget limit exceeded",

	ProviderNotFound:         "provider not found",
	ProviderNameTaken:        "provider name already taken",
	ProviderDisabled:         "provider is disabled",
	ProviderTestFailed:       "provider connection test failed",
	ProviderHasModels:        "provider still has models, remove them first",
	ProviderNoTestableModel:  "provider has no enabled model to test with",
	ProviderMasterKeyMissing: "provider master key not configured",
	ProviderHasRequestLogs:   "provider has existing request logs, cannot be deleted",
	ProviderKeyNotFound:      "provider key not found",
	ProviderKeyLabelTaken:    "provider key label already taken",
	ProviderKeyNotVerified:   "cannot enable a key that has not passed verification",
	ProviderKeyNeedsReentry:  "provider address changed, please resubmit the key plaintext",
	ProviderKeyTooShort:      "key plaintext is too short",
	ProviderKeyTestNotSaved:  "test result not saved because the key was modified concurrently, please retry",

	ModelNotFound:               "model not found",
	ModelNameTaken:              "model name already taken",
	ModelCandidateNotFound:      "model candidate not found",
	ModelCandidateProviderTaken: "this provider is already a candidate for this model",
	ModelCandidateNotVerified:   "cannot enable a candidate that has not passed the basic test",

	UserGroupNotFound:       "user group not found",
	UserGroupNameTaken:      "user group name already taken",
	UserGroupHasMembers:     "user group still has members, reassign them first",
	UserGroupInvalidPage:    "page_permissions contains an unrecognized page key",
	UserGroupHasRequestLogs: "user group has existing request logs, cannot be deleted",
	UserNotFound:            "user not found",
	UserUsernameTaken:       "username already taken",
	UserDisabled:            "target user is disabled",

	RequestLogNotFound: "request log not found",

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
	ErrAccountInvalidCredentials = errors.New(ErrorMessages[AccountInvalidCredentials])
	ErrAccountDisabled           = errors.New(ErrorMessages[AccountDisabled])
	ErrAccountSessionInvalid     = errors.New(ErrorMessages[AccountSessionInvalid])
	ErrAccountLoginLocked        = errors.New(ErrorMessages[AccountLoginLocked])
	ErrAccountLastAdminProtected = errors.New(ErrorMessages[AccountLastAdminProtected])
	ErrAccountSetupAlreadyDone   = errors.New(ErrorMessages[AccountSetupAlreadyDone])

	ErrAPIKeyNotFound        = errors.New(ErrorMessages[APIKeyNotFound])
	ErrAPIKeyInvalid         = errors.New(ErrorMessages[APIKeyInvalid])
	ErrAPIKeyExpired         = errors.New(ErrorMessages[APIKeyExpired])
	ErrAPIKeyRevoked         = errors.New(ErrorMessages[APIKeyRevoked])
	ErrAPIKeyRateLimitedRPM  = errors.New(ErrorMessages[APIKeyRateLimitedRPM])
	ErrAPIKeyRateLimitedTPM  = errors.New(ErrorMessages[APIKeyRateLimitedTPM])
	ErrAPIKeyRateLimitedConc = errors.New(ErrorMessages[APIKeyRateLimitedConc])
	ErrAPIKeyBudgetExceeded  = errors.New(ErrorMessages[APIKeyBudgetExceeded])

	ErrProviderNotFound         = errors.New(ErrorMessages[ProviderNotFound])
	ErrProviderNameTaken        = errors.New(ErrorMessages[ProviderNameTaken])
	ErrProviderDisabled         = errors.New(ErrorMessages[ProviderDisabled])
	ErrProviderTestFailed       = errors.New(ErrorMessages[ProviderTestFailed])
	ErrProviderHasModels        = errors.New(ErrorMessages[ProviderHasModels])
	ErrProviderNoTestableModel  = errors.New(ErrorMessages[ProviderNoTestableModel])
	ErrProviderMasterKeyMissing = errors.New(ErrorMessages[ProviderMasterKeyMissing])
	ErrProviderHasRequestLogs   = errors.New(ErrorMessages[ProviderHasRequestLogs])
	ErrProviderKeyNotFound      = errors.New(ErrorMessages[ProviderKeyNotFound])
	ErrProviderKeyLabelTaken    = errors.New(ErrorMessages[ProviderKeyLabelTaken])
	ErrProviderKeyNotVerified   = errors.New(ErrorMessages[ProviderKeyNotVerified])
	ErrProviderKeyNeedsReentry  = errors.New(ErrorMessages[ProviderKeyNeedsReentry])
	ErrProviderKeyTooShort      = errors.New(ErrorMessages[ProviderKeyTooShort])
	ErrProviderKeyTestNotSaved  = errors.New(ErrorMessages[ProviderKeyTestNotSaved])

	ErrModelNotFound               = errors.New(ErrorMessages[ModelNotFound])
	ErrModelNameTaken              = errors.New(ErrorMessages[ModelNameTaken])
	ErrModelCandidateNotFound      = errors.New(ErrorMessages[ModelCandidateNotFound])
	ErrModelCandidateProviderTaken = errors.New(ErrorMessages[ModelCandidateProviderTaken])
	ErrModelCandidateNotVerified   = errors.New(ErrorMessages[ModelCandidateNotVerified])

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
