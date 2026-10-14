import { toApiError } from './errors'
import { origin, send } from './fetch'

// Access tokens for acting as a Principal (FR-UI-004): the Principal
// switcher and the IAM policy simulator get them from the Instance's
// public OAuth2 token endpoint, never from a console-private API
// (FR-UI-003).

/** PRINCIPAL is what the console can act as: a user or a service account. */
export const PRINCIPAL = /^(user|serviceAccount):[^\s@]+@[^\s@]+$/

interface Minted {
  token: string
  /** expiresIn is the token's lifetime in seconds, by the Emulator clock. */
  expiresIn: number
}

async function requestToken(principal: string): Promise<Minted> {
  const res = await send(
    new Request(new URL('/oauth2/token', origin()), {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ grant_type: 'refresh_token', refresh_token: principal }),
      credentials: 'omit',
    }),
  )
  const body = (await res.json().catch(() => ({}))) as {
    access_token?: string
    expires_in?: number
    error?: string
    error_description?: string
  }
  if (!res.ok || !body.access_token) {
    throw toApiError(res.status, {
      error: { message: body.error_description ?? body.error ?? `HTTP ${res.status}` },
    })
  }
  return { token: body.access_token, expiresIn: body.expires_in ?? 3600 }
}

/**
 * mintToken gets an access token for principal from the Instance's OAuth2
 * token endpoint, through the refresh token grant that user ADC uses: the
 * emulator takes "user:EMAIL" or "serviceAccount:EMAIL" as the refresh
 * token and issues a token for that Principal.
 */
export async function mintToken(principal: string): Promise<string> {
  return (await requestToken(principal)).token
}

/** revokeToken revokes a token that is no longer needed. */
export async function revokeToken(token: string): Promise<void> {
  await send(
    new Request(new URL('/oauth2/revoke', origin()), {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ token }),
      credentials: 'omit',
    }),
  ).catch(() => undefined)
}

// Tokens for the switcher's Principal are kept in memory only, one per
// Principal, and minted again a minute before they expire. Advancing the
// Emulator clock or a Reset can end one sooner; tokenFor's callers then
// forget it and ask again.
const EARLY_MS = 60_000
const cache = new Map<string, Promise<{ token: string; until: number }>>()

/** tokenFor returns a live access token for principal. */
export async function tokenFor(principal: string): Promise<string> {
  const cached = await cache.get(principal)?.catch(() => undefined)
  if (cached && cached.until > Date.now()) return cached.token
  const entry = requestToken(principal).then(({ token, expiresIn }) => ({
    token,
    until: Date.now() + expiresIn * 1000 - EARLY_MS,
  }))
  cache.set(principal, entry)
  void entry.catch(() => {
    if (cache.get(principal) === entry) cache.delete(principal)
  })
  return (await entry).token
}

/** forgetToken drops principal's cached token, e.g. after a 401. */
export function forgetToken(principal: string) {
  cache.delete(principal)
}

/** clearTokens forgets every cached token (tests). */
export function clearTokens() {
  cache.clear()
}
