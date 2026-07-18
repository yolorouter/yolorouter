// Numeric error codes this module's UI logic branches on (mirrors
// pkg/errcode's Go constants) — see .claude/docs/2026-07-17-m1-auth-design.md §5.
// Only codes an actual call site branches on are listed here — add more as
// they gain a real caller.
export const ACCOUNT_SESSION_INVALID = 10003
export const ACCOUNT_LOGIN_LOCKED = 10005
export const ACCOUNT_SETUP_ALREADY_DONE = 10007
