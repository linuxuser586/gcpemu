import { queryOptions } from '@tanstack/react-query'

import { admin, unwrap } from '@/api/admin'
import type { components } from '@/api/admin.gen'
import { gcpFetch } from '@/api/fetch'

import {
  listKind,
  relPath,
  resourcePath,
  waitOperation,
  type Operation,
  type Ref,
  type Resource,
} from '../lb/api'

// The Cloud CDN view's reads and writes. Cloud CDN has no resources of its
// own: an origin is a backend service or bucket with enableCdn, whose
// cdnPolicy holds its cache settings (FR-CDN-001), and invalidation is
// urlMaps.invalidateCache (FR-CDN-005), all compute v1. The cache's
// contents and hit counts, and purging it, are on the admin API: GCP
// reports them only through Cloud Monitoring and has no purge.

export type CdnBackend = components['schemas']['CdnBackend']
export type CdnEntry = components['schemas']['CdnEntry']

/** OriginColl is a collection whose resources can be origins. */
export type OriginColl = 'backendServices' | 'backendBuckets'

export const ORIGIN_COLLS: readonly OriginColl[] = ['backendServices', 'backendBuckets']

export const isOriginColl = (c: string): c is OriginColl =>
  c === 'backendServices' || c === 'backendBuckets'

/** kindName names an origin's kind in sentences. */
export const kindName = (c: OriginColl) =>
  c === 'backendServices' ? 'backend service' : 'backend bucket'

/** Backend is a backend service or bucket, with its kind. */
export interface Backend {
  coll: OriginColl
  r: Resource
}

/** listBackends lists a Project's backend services and buckets. */
export async function listBackends(project: string): Promise<Backend[]> {
  const [services, buckets] = await Promise.all(ORIGIN_COLLS.map((c) => listKind(project, c)))
  return [
    ...services!.map((r) => ({ coll: 'backendServices' as const, r })),
    ...buckets!.map((r) => ({ coll: 'backendBuckets' as const, r })),
  ]
}

/** isOrigin: Cloud CDN is on for the backend. */
export const isOrigin = (r: Resource) => r.enableCdn === true

/** CacheInvalidationRule is the body of urlMaps.invalidateCache. */
export interface CacheInvalidationRule {
  host?: string
  path?: string
  cacheTags?: string[]
}

/**
 * invalidate is urlMaps.invalidateCache of a URL map: it removes the
 * matching entries of every origin the map routes to.
 */
export const invalidate = async (urlMap: Ref, rule: CacheInvalidationRule) =>
  waitOperation(
    await gcpFetch<Operation>(`/compute/v1/${resourcePath(urlMap)}/invalidateCache`, {
      method: 'POST',
      body: JSON.stringify(rule),
    }),
  )

/** purge clears the whole cache (`gcpemu cdn purge`, FR-CDN-008). */
export const purge = () => unwrap(admin.POST('/_emu/v1/cdn/purge'))

/** serviceRefs are the backend services and buckets a URL map routes to. */
export function serviceRefs(map: Resource): string[] {
  const out = new Set<string>()
  const add = (v: unknown) => {
    if (typeof v === 'string' && v) out.add(relPath(v))
  }
  const action = (a: unknown) => {
    const ws = (a as { weightedBackendServices?: { backendService?: string }[] } | undefined)
      ?.weightedBackendServices
    ws?.forEach((w) => add(w.backendService))
  }
  add(map.defaultService)
  action(map.defaultRouteAction)
  for (const pm of (map.pathMatchers as Record<string, unknown>[] | undefined) ?? []) {
    add(pm.defaultService)
    action(pm.defaultRouteAction)
    for (const r of (pm.pathRules as Record<string, unknown>[] | undefined) ?? []) {
      add(r.service)
      action(r.routeAction)
    }
    for (const r of (pm.routeRules as Record<string, unknown>[] | undefined) ?? []) {
      add(r.service)
      action(r.routeAction)
    }
  }
  return [...out]
}

/** hitRatio is the share of requests served from cache, hits and revalidations. */
export function hitRatio(b: Pick<CdnBackend, 'hits' | 'misses' | 'revalidated' | 'uncacheable'>) {
  const total = b.hits + b.misses + b.revalidated + b.uncacheable
  return total ? (b.hits + b.revalidated) / total : undefined
}

export const requests = (b: CdnBackend) => b.hits + b.misses + b.revalidated + b.uncacheable

/** percent formats a ratio, "—" when there were no requests. */
export const percent = (r: number | undefined) =>
  r === undefined ? '—' : `${(r * 100).toFixed(r === 1 || r === 0 ? 0 : 1)}%`

/** bytes formats a size in binary units. */
export function bytes(n: number): string {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let i = 0
  let v = n
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return i === 0 ? `${n} B` : `${v.toFixed(v < 10 ? 1 : 0)} ${units[i]}`
}

/** duration formats seconds as the largest whole unit, e.g. "1 h" or "90 s". */
export function duration(s: number): string {
  for (const [n, u] of [
    [86400, 'd'],
    [3600, 'h'],
    [60, 'min'],
  ] as const) {
    if (s >= n && s % n === 0) return `${s / n} ${u}`
  }
  return `${s} s`
}

// ---- queries ----

/** STATS_POLL_MS is how often cache statistics refresh: hits are not events. */
export const STATS_POLL_MS = 2000

export const backendsQuery = (project: string) =>
  queryOptions({
    queryKey: ['cdn', 'backends', project],
    queryFn: () => listBackends(project),
    enabled: !!project,
  })

/** statsQuery is the cache's usage and each of the Project's backends'. */
export const statsQuery = (project: string) =>
  queryOptions({
    queryKey: ['cdn', 'stats', project],
    queryFn: async () => {
      const res = await unwrap(admin.GET('/_emu/v1/cdn', { params: { query: { project } } }))
      return { ...res, byBackend: new Map(res.backends.map((b) => [b.backend, b])) }
    },
    enabled: !!project,
    refetchInterval: STATS_POLL_MS,
  })

/** entriesQuery lists an origin's cached responses. */
export const entriesQuery = (backend: string) =>
  queryOptions({
    queryKey: ['cdn', 'entries', backend],
    queryFn: () => unwrap(admin.GET('/_emu/v1/cdn/entries', { params: { query: { backend } } })),
    refetchInterval: STATS_POLL_MS,
  })
