import createClient from 'openapi-fetch'

import type { components, paths } from './admin.gen'
import { networkError, toApiError } from './errors'
import { origin } from './fetch'

export type Info = components['schemas']['Info']
export type Readiness = components['schemas']['Readiness']
export type Container = components['schemas']['Container']
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
  // Resolved per call: tests swap the global fetch after this module loads.
  fetch: (req) => globalThis.fetch(req),
  credentials: 'omit',
})

admin.use({
  onError({ error }) {
    return networkError(error)
  },
})

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
