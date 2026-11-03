import createClient from 'openapi-fetch'

import type { components, paths } from './admin.gen'
import { networkError, toApiError } from './errors'
import { origin, resending } from './fetch'

export type Info = components['schemas']['Info']
export type Readiness = components['schemas']['Readiness']
export type Container = components['schemas']['Container']
export type External = components['schemas']['External']
export type ResourceCounts = components['schemas']['ResourceCounts']
export type Endpoints = components['schemas']['Endpoints']
export type RequestEntry = components['schemas']['RequestEntry']
export type ResourceChange = components['schemas']['ResourceChange']
export type OperationChange = components['schemas']['OperationChange']
export type Operation = components['schemas']['Operation']
export type SubscriptionStats = components['schemas']['SubscriptionStats']

/** admin is a typed client for the /_emu/v1 admin API. */
export const admin = createClient<paths>({
  baseUrl: origin(),
  // resending resolves the global fetch per call: tests swap it after this
  // module loads. It re-sends a write that got no response (ADR 0062).
  fetch: (req) => resending(req),
  // Beyond loopback the admin API needs the admin token cookie (FR-UI-009).
  credentials: 'same-origin',
})

admin.use({
  onError({ error }) {
    return networkError(error)
  },
})

/**
 * repeatable is the request option of an admin POST safe to send twice,
 * so one that got no response is re-sent too (ADR 0062).
 */
export const repeatable = { fetch: (req: Request) => resending(req, true) }

interface FetchResult<T> {
  data?: T
  error?: unknown
  response: Response
}

/** unwrap returns a call's data, or throws its error as an ApiError. */
export async function unwrap<T>(call: Promise<FetchResult<T>>): Promise<T> {
  const { data, error, response } = await call
  if (error !== undefined || !response.ok) throw toApiError(response.status, error)
  return data as T
}
