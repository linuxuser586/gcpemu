import { networkError, toApiError } from './errors'

// Requests carry no credentials, so the gateway attributes them to the
// Instance's Default principal (FR-UI-004).

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
export async function gcpFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers)
  if (init?.body !== undefined) headers.set('Content-Type', 'application/json')
  const res = await send(
    new Request(new URL(path, origin()), { ...init, headers, credentials: 'omit' }),
  )
  const body = await readBody(res)
  if (!res.ok) throw toApiError(res.status, body)
  return body as T
}

/**
 * gcpText calls a public API that answers with plain text, such as a pod's
 * log through the Connect gateway.
 */
export async function gcpText(path: string): Promise<string> {
  const res = await send(new Request(new URL(path, origin()), { credentials: 'omit' }))
  if (!res.ok) throw toApiError(res.status, await readBody(res))
  return res.text()
}
