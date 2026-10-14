import { queryOptions } from '@tanstack/react-query'

import { admin, unwrap, type RequestEntry } from '@/api/admin'
import type { components } from '@/api/admin.gen'

// The Request log view's reads (FR-UI-013), on the admin API. The log
// itself is requestsQuery, which the events bridge appends to; an entry's
// bodies are read only when it is opened, as the list and the event
// stream leave them out.

export type RequestDetail = components['schemas']['RequestDetail']
export type RequestBody = components['schemas']['RequestBody']

/** requestQuery is one Request log entry with its bodies. An entry never changes. */
export const requestQuery = (id: number) =>
  queryOptions({
    // Not under ['admin', 'requests']: the bridge appends to every query there.
    queryKey: ['admin', 'request', id],
    queryFn: (): Promise<RequestDetail> =>
      unwrap(admin.GET('/_emu/v1/requests/{id}', { params: { path: { id } } })),
    staleTime: Infinity,
  })

/** Filters narrow the Request log; each is a URL search parameter. */
export interface Filters {
  service: string
  method: string
  principal: string
  status: string
  resource: string
}

export const FILTERS = ['service', 'method', 'principal', 'status', 'resource'] as const

const contains = (v: string | undefined, f: string) =>
  !f || (v ?? '').toLowerCase().includes(f.toLowerCase())

/**
 * statusMatches: f is a status ("404"), a class with x for any digit
 * ("4xx"), or part of a code name ("not_found"), so that gRPC and HTTP
 * entries filter alike.
 */
export function statusMatches(e: RequestEntry, f: string): boolean {
  const t = f.trim().toLowerCase()
  if (!t) return true
  if (/^[0-9x]+$/.test(t)) {
    const s = String(e.status ?? '')
    return s.length === t.length && [...t].every((c, i) => c === 'x' || c === s[i])
  }
  return (e.code ?? '').toLowerCase().includes(t)
}

/** matches: the entry passes every filter; text filters match any part, ignoring case. */
export function matches(e: RequestEntry, f: Filters): boolean {
  return (
    (!f.service || e.service === f.service) &&
    contains(e.method, f.method) &&
    contains(e.principal, f.principal) &&
    contains(e.resource, f.resource) &&
    statusMatches(e, f.status)
  )
}

/** bodyText is a body as shown: JSON indented, anything else as sent. */
export function bodyText(b: RequestBody): string {
  const text = b.text ?? ''
  if (b.truncated || !/^\s*[[{]/.test(text)) return text
  try {
    return JSON.stringify(JSON.parse(text), null, 2)
  } catch {
    return text
  }
}

/** size formats a byte count. */
export function size(n: number): string {
  if (n < 1024) return `${n} ${n === 1 ? 'byte' : 'bytes'}`
  if (n < 1024 * 1024) return `${+(n / 1024).toFixed(1)} KiB`
  return `${+(n / 1024 / 1024).toFixed(1)} MiB`
}
