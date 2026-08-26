import { apiFetch } from './client'

/**
 * Account status codes as stored by the backend (users.status). Reads
 * expose the numeric form; writes use the string enum ('enabled' |
 * 'disabled') — see updateUserStatus.
 */
export const USER_STATUS_ENABLED = 1

// Mirrors service.UserSummaryView. Backend wraps the array as
// { users: [...] }.
export interface UserSummary {
  id: number
  username: string
  display_name: string
  email: string
  role: string
  status: number
  is_local: boolean
  /** First-run setup account — the escape hatch with no row actions. */
  is_bootstrap: boolean
  last_login_at: string | null
  created_at: string
  /** Login providers the account arrived through; empty for local password accounts. */
  providers: string[]
  key_count: number
  spend_micros: number
}

export function listUsers(): Promise<{ users: UserSummary[] }> {
  return apiFetch<{ users: UserSummary[] }>('/api/admin/users')
}

export interface CreateUserInput {
  username: string
  display_name?: string
  /** Informational only — this build sends no mail; recorded for the directory. */
  email?: string
  password: string
}

// Provisions a local password member. The password travels once in this
// request; the backend stores only its bcrypt hash and never echoes it.
export function createUser(input: CreateUserInput): Promise<{ user: UserSummary }> {
  return apiFetch<{ user: UserSummary }>('/api/admin/users', {
    method: 'POST',
    body: JSON.stringify(input),
  })
}

// Replaces another local account's password. Reserved to the bootstrap
// administrator (the backend refuses everyone else with 10021); the
// target's live sessions die with the reset.
export function resetUserPassword(id: number, password: string): Promise<null> {
  return apiFetch<null>(`/api/admin/users/${id}/password`, {
    method: 'POST',
    body: JSON.stringify({ password }),
  })
}

// Rewrites another account's display name and email — directory
// information only. Reserved to the bootstrap administrator (the backend
// refuses everyone else with 10022); the target's sessions are untouched.
export function updateUserProfile(
  id: number,
  input: { display_name?: string; email?: string },
): Promise<null> {
  return apiFetch<null>(`/api/admin/users/${id}/profile`, {
    method: 'PATCH',
    body: JSON.stringify(input),
  })
}

// toUserOptions maps accounts to naive-ui <select> options. Kept here —
// next to the UserSummary type — so every user <select> (analytics filter,
// cost page scope) labels accounts the same way and can't drift.
export function toUserOptions(users: UserSummary[]): Array<{ label: string; value: number }> {
  return users.map((u) => ({
    label: u.display_name ? `${u.username} (${u.display_name})` : u.username,
    value: u.id,
  }))
}

export function updateUserStatus(id: number, status: 'enabled' | 'disabled'): Promise<null> {
  return apiFetch<null>(`/api/admin/users/${id}/status`, {
    method: 'PATCH',
    body: JSON.stringify({ status }),
  })
}

export function updateUserRole(id: number, role: 'admin' | 'member'): Promise<null> {
  return apiFetch<null>(`/api/admin/users/${id}/role`, {
    method: 'PATCH',
    body: JSON.stringify({ role }),
  })
}
