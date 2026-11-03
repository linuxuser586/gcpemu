import { usePrefs } from '@/stores/prefs'

import { networkError, toApiError } from './errors'
import { forgetToken, tokenFor } from './tokens'

// Public API requests act as the Principal chosen in the Principal
// switcher, carrying an access token for it. With none chosen they carry
// no credentials, so the gateway attributes them to the Instance's
// Default principal (FR-UI-004). Same-origin cookies go along, for a
// reverse proxy that signs users in to the console (#888), but they are
// no public API's credential and the gateway drops them.

/** origin is where the console was served from: the API gateway. */
export const origin = () => globalThis.location.origin

/**
 * RESEND_MS are the waits before each re-send: the first two of
 * react-query's default retryDelay, which the queries use.
 */
const RESEND_MS = [1000, 2000]

/** RESENT_METHODS are the methods safe to send again without an opt-in. */
const RESENT_METHODS = new Set(['PUT', 'PATCH', 'DELETE'])

/**
 * mayResend reports whether req, if it gets no response, is safe to send
 * again (ADR 0062). repeatable is the opt-in of a POST that its API
 * function declares safe to repeat; any other method ignores it. A GET is
 * left to the queries' own retry, and a method not named here is sent once.
 */
function mayResend(req: Request, repeatable: boolean): boolean {
  return RESENT_METHODS.has(req.method) || (req.method === 'POST' && repeatable)
}

/**
 * resending fetches req through the global fetch and, when it gets no
 * response and mayResend allows, sends it again after 1 s and then 2 s.
 * It throws the last attempt's error as fetch did: a Request's body is
 * read once, so every attempt sends a clone.
 */
export async function resending(req: Request, repeatable = false): Promise<Response> {
  if (!mayResend(req, repeatable)) return globalThis.fetch(req)
  for (const wait of RESEND_MS) {
    try {
      return await globalThis.fetch(req.clone())
    } catch (e) {
      if (req.signal.aborted) throw e
    }
    await new Promise((resolve) => setTimeout(resolve, wait))
  }
  return globalThis.fetch(req)
}

/** send fetches through resending, mapping failures to ApiError. */
export async function send(input: Request, repeatable = false): Promise<Response> {
  try {
    return await resending(input, repeatable)
  } catch (e) {
    throw networkError(e)
  }
}

export interface GcpInit extends RequestInit {
  /**
   * principal overrides the switcher's Principal for this request; ''
   * sends no credentials, so the request runs as the Default principal.
   */
  principal?: string
  /**
   * repeatable declares a POST safe to send twice, so one that got no
   * response is re-sent (ADR 0062): a create whose resource name the
   * client chooses, or a custom method whose second call changes nothing.
   */
  repeatable?: boolean
}

/** JsonWrite is a write a feature's request helper sends with a JSON body. */
export interface JsonWrite extends Pick<GcpInit, 'repeatable'> {
  method: string
  body?: unknown
}

/** jsonInit is gcpFetch's init for w, its body encoded as JSON. */
export const jsonInit = ({ body, ...rest }: JsonWrite): GcpInit => ({
  ...rest,
  body: body === undefined ? undefined : JSON.stringify(body),
})

/**
 * sendAs sends a public API request as the chosen Principal. A request
 * that sets its own Authorization header keeps it. When the gateway
 * refuses a cached token (the Emulator clock moved past it, or a Reset
 * dropped it), it mints a fresh one and tries once more.
 */
export async function sendAs(path: string, init: GcpInit = {}): Promise<Response> {
  const { principal = usePrefs.getState().principal, repeatable, ...rest } = init
  const headers = new Headers(rest.headers)
  const url = new URL(path, origin())
  const go = () =>
    send(new Request(url, { ...rest, headers, credentials: 'same-origin' }), repeatable)
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
