import { apiFetch } from './client'

// === Public login flow ====================================================

// A login-page button: nothing but display data.
export interface PublicOAuthProvider {
  slug: string
  name: string
  icon: string
}

export function listPublicOAuthProviders(): Promise<{ providers: PublicOAuthProvider[] }> {
  return apiFetch<{ providers: PublicOAuthProvider[] }>('/api/admin/auth/oauth/providers')
}

// beginOAuthLogin asks the server for a one-time authorization URL (state
// + PKCE are minted server-side); the caller navigates the browser there.
export function beginOAuthLogin(slug: string): Promise<{ authorize_url: string }> {
  return apiFetch<{ authorize_url: string }>('/api/admin/auth/oauth/state', {
    method: 'POST',
    body: JSON.stringify({ slug }),
  })
}

// === Admin provider management ============================================

// The five protocol-style knobs every provider carries; the zero shape
// (form/snake/no header/PKCE on/no extras) is the standard OAuth2 flow.
// One declaration feeds both interfaces below so they cannot drift.
export interface OAuthProtocolStyle {
  token_request_style: 'form' | 'json'
  token_field_style: 'snake' | 'camel'
  userinfo_token_header: string
  pkce_enabled: boolean
  extra_authorize_params: string
}

// Mirrors service.OAuthProviderView. The client secret never round-trips:
// has_client_secret only reports that one is stored.
export interface OAuthProviderView extends OAuthProtocolStyle {
  id: number
  slug: string
  name: string
  icon: string
  enabled: boolean
  client_id: string
  has_client_secret: boolean
  authorization_endpoint: string
  token_endpoint: string
  userinfo_endpoint: string
  scopes: string
  user_id_field: string
  username_field: string
  display_name_field: string
  email_field: string
  auth_style: 'basic' | 'post'
  identity_count: number
  created_at: string
  updated_at: string
}

export interface OAuthProviderInput extends OAuthProtocolStyle {
  slug: string
  name: string
  icon: string
  enabled: boolean
  client_id: string
  client_secret: string
  authorization_endpoint: string
  token_endpoint: string
  userinfo_endpoint: string
  scopes: string
  user_id_field: string
  username_field: string
  display_name_field: string
  email_field: string
  auth_style: 'basic' | 'post'
}

// callback_base is this deployment's callback address prefix
// (".../oauth/callback/"), built server-side from external_url when
// configured — the admin form appends the slug to it instead of deriving
// from its own page origin, which can differ from the real redirect_uri.
export function listOAuthProviders(): Promise<{ providers: OAuthProviderView[]; callback_base: string }> {
  return apiFetch<{ providers: OAuthProviderView[]; callback_base: string }>('/api/admin/oauth-providers')
}

export function createOAuthProvider(input: OAuthProviderInput): Promise<OAuthProviderView> {
  return apiFetch<OAuthProviderView>('/api/admin/oauth-providers', {
    method: 'POST',
    body: JSON.stringify(input),
  })
}

// Sparse PATCH: omit client_secret entirely to keep the stored one.
export function updateOAuthProvider(
  id: number,
  input: Partial<OAuthProviderInput>,
): Promise<OAuthProviderView> {
  return apiFetch<OAuthProviderView>(`/api/admin/oauth-providers/${id}`, {
    method: 'PATCH',
    body: JSON.stringify(input),
  })
}

export function deleteOAuthProvider(id: number): Promise<void> {
  return apiFetch<void>(`/api/admin/oauth-providers/${id}`, { method: 'DELETE' })
}

export interface OIDCDiscoveryResult {
  authorization_endpoint: string
  token_endpoint: string
  userinfo_endpoint: string
}

export function discoverOIDC(url: string): Promise<OIDCDiscoveryResult> {
  return apiFetch<OIDCDiscoveryResult>('/api/admin/oauth-providers/discover', {
    method: 'POST',
    body: JSON.stringify({ url }),
  })
}
