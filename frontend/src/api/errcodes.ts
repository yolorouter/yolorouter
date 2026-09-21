// Numeric error codes this module's UI logic branches on (mirrors
// pkg/errcode's Go constants).
// Only codes an actual call site branches on are listed here — add more as
// they gain a real caller.
export const ACCOUNT_SESSION_INVALID = 10003
export const ACCOUNT_LOGIN_LOCKED = 10005
export const ACCOUNT_SETUP_ALREADY_DONE = 10007
// Mirrors backend APIKeyNotFound (pkg/errcode).
export const API_KEY_NOT_FOUND = 11001
export const CUSTOM_SYSTEM_PROMPT_CONFLICT = 11012
export const API_KEY_CONFLICT = 11013
export const INPUT_COMPRESSION_CONFLICT = 11014
export const COMPRESS_ENABLED_REQUIRED = 11015
// Mirrors backend KeyAutoRecoveryConflict (pkg/errcode).
export const KEY_AUTO_RECOVERY_CONFLICT = 11019
export const PROVIDER_NOT_FOUND = 12001
export const PROVIDER_NAME_TAKEN = 12002
export const PROVIDER_KEY_NOT_FOUND = 12009
export const PROVIDER_KEY_LABEL_TAKEN = 12010
export const PROVIDER_KEY_NOT_VERIFIED = 12011
export const PROVIDER_KEY_NEEDS_REENTRY = 12012
