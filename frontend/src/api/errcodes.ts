// Numeric error codes this module's UI logic branches on (mirrors
// pkg/errcode's Go constants).
// Only codes an actual call site branches on are listed here — add more as
// they gain a real caller.
export const ACCOUNT_SESSION_INVALID = 10003
export const ACCOUNT_LOGIN_LOCKED = 10005
export const ACCOUNT_SETUP_ALREADY_DONE = 10007
export const PROVIDER_NOT_FOUND = 12001
export const PROVIDER_NAME_TAKEN = 12002
export const PROVIDER_KEY_NOT_FOUND = 12009
export const PROVIDER_KEY_LABEL_TAKEN = 12010
export const PROVIDER_KEY_NOT_VERIFIED = 12011
export const PROVIDER_KEY_NEEDS_REENTRY = 12012
