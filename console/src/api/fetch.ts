import { usePrefs } from '@/stores/prefs'

import { networkError, toApiError } from './errors'
import { forgetToken, tokenFor } from './tokens'

// Public API requests act as the Principal chosen in the Principal
// switcher, carrying an access token for it. With none chosen they carry
// no Authorization, so the gateway attributes them to the Instance's
// Default principal (FR-UI-004). They carry same-origin cookies, for a
// reverse proxy that signs users in to the console (#888); the gateway
// drops them, as no public API reads them.

/** origin is where the console was served from: the API gateway. */
export const origin = () => globalThis.location.origin

/** send fetches through the global fetch, mapping failures to ApiError. */
export async function send(input: Request): Promise<Response> {
  try {
    return await globalThis.fetch(input)
  } catch (e) {
    throw networkError(e)
  }
}

export interface GcpInit extends RequestInit {
  /**
   * principal overrides the switcher's Principal for this request; ''
   * sends no Authorization, so the request runs as the Default principal.
   */
  principal?: string
}

/**
 * sendAs sends a public API request as the chosen Principal. A request
 * that sets its own Authorization header keeps it. When the gateway
 * refuses a cached token (the Emulator clock moved past it, or a Reset
 * dropped it), it mints a fresh one and tries once more.
 */
export async function sendAs(path: string, init: GcpInit = {}): Promise<Response> {
  const { principal = usePrefs.getState().principal, ...rest } = init
  const headers = new Headers(rest.headers)
  const url = new URL(path, origin())
  const go = () => send(new Request(url, { ...rest, headers, credentials: 'same-origin' }))
  if (!principal || headers.has('Authorization')) return go()
  headers.set('Authorization', `Bearer ${await tokenFor(principal)}`)
  const res = await go()
  if (res.status !== 401) return res
  forgetToken(principal)
  headers.set('Authorization', `Bearer ${await tokenFor(principal)}`)
  return go()
}

async function readBody(res: Response): Promise<unknown> {
  const text = await res.text()
  if (!text) return undefined
  try {
    return JSON.parse(text) as unknown
  } catch {
    return text
  }
}

/** gcpFetch calls a public GCP API on the gateway, e.g. "/storage/v1/b". */
export async function gcpFetch<T>(path: string, init?: GcpInit): Promise<T> {
  const headers = new Headers(init?.headers)
  if (init?.body !== undefined) headers.set('Content-Type', 'application/json')
  const res = await sendAs(path, { ...init, headers })
  const body = await readBody(res)
  if (!res.ok) throw toApiError(res.status, body)
  return body as T
}

/**
 * gcpText calls a public API that answers with plain text, such as a pod's
 * log through the Connect gateway.
 */
export async function gcpText(path: string): Promise<string> {
  const res = await sendAs(path)
  if (!res.ok) throw toApiError(res.status, await readBody(res))
  return res.text()
}
