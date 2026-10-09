import { queryOptions } from '@tanstack/react-query'

import { admin, unwrap } from '@/api/admin'
import type { components } from '@/api/admin.gen'
import { ApiError } from '@/api/errors'
import { gcpFetch } from '@/api/fetch'

// The Load Balancer view's reads and writes: the load-balancing
// collections of compute v1 (FR-LB-001), global and regional, and two
// admin API reports GCP has no API for: each forwarding rule's local
// listener and which route of a URL map serves a request. Types cover the
// fields the view reads; the raw JSON editor covers the rest.

export type LbListener = components['schemas']['LbListener']
export type LbRoute = components['schemas']['LbRoute']
export type LbRouteRequest = components['schemas']['LbRouteRequest']

/** Resource is any compute resource, as the API returns it. */
export interface Resource {
  name: string
  selfLink?: string
  region?: string
  description?: string
  creationTimestamp?: string
  fingerprint?: string
  [field: string]: unknown
}

/** Coll is a load-balancing collection, as its URL segment names it. */
export type Coll =
  | 'forwardingRules'
  | 'targetHttpProxies'
  | 'targetHttpsProxies'
  | 'urlMaps'
  | 'backendServices'
  | 'backendBuckets'
  | 'healthChecks'
  | 'sslCertificates'
  | 'sslPolicies'

export interface KindInfo {
  coll: Coll
  /** title is the plural heading, e.g. "URL maps". */
  title: string
  /** singular names one resource in sentences, e.g. "URL map". */
  singular: string
  /** api is the global API resource of the method names, e.g. "urlMaps". */
  api: string
  /** regional: the kind also has regional resources (an aggregated list). */
  regional: boolean
  /** editable: the API has patch (SSL certificates are immutable). */
  editable: boolean
}

/** KINDS are the Service's M resource types (FR-LB-001), in tab order. */
export const KINDS: readonly KindInfo[] = [
  {
    coll: 'forwardingRules',
    title: 'Forwarding rules',
    singular: 'forwarding rule',
    api: 'globalForwardingRules',
    regional: true,
    editable: true,
  },
  {
    coll: 'targetHttpProxies',
    title: 'HTTP proxies',
    singular: 'target HTTP proxy',
    api: 'targetHttpProxies',
    regional: true,
    editable: true,
  },
  {
    coll: 'targetHttpsProxies',
    title: 'HTTPS proxies',
    singular: 'target HTTPS proxy',
    api: 'targetHttpsProxies',
    regional: true,
    editable: true,
  },
  {
    coll: 'urlMaps',
    title: 'URL maps',
    singular: 'URL map',
    api: 'urlMaps',
    regional: true,
    editable: true,
  },
  {
    coll: 'backendServices',
    title: 'Backend services',
    singular: 'backend service',
    api: 'backendServices',
    regional: true,
    editable: true,
  },
  {
    coll: 'backendBuckets',
    title: 'Backend buckets',
    singular: 'backend bucket',
    api: 'backendBuckets',
    regional: false,
    editable: true,
  },
  {
    coll: 'healthChecks',
    title: 'Health checks',
    singular: 'health check',
    api: 'healthChecks',
    regional: true,
    editable: true,
  },
  {
    coll: 'sslCertificates',
    title: 'Certificates',
    singular: 'SSL certificate',
    api: 'sslCertificates',
    regional: true,
    editable: false,
  },
  {
    coll: 'sslPolicies',
    title: 'SSL policies',
    singular: 'SSL policy',
    api: 'sslPolicies',
    regional: true,
    editable: true,
  },
]

const byColl = new Map(KINDS.map((k) => [k.coll, k]))

export const kindInfo = (coll: string) => byColl.get(coll as Coll)

/** Ref names one resource: a Project, "" (global) or a region, and a name. */
export interface Ref {
  project: string
  region: string
  coll: Coll
  name: string
}

const API = '/compute/v1/'

/** scopePath is "projects/P/global" or "projects/P/regions/R". */
export const scopePath = (project: string, region: string) =>
  `projects/${project}/${region ? `regions/${region}` : 'global'}`

/** resourcePath is a resource's relative path. */
export const resourcePath = (r: Ref) =>
  `${scopePath(r.project, r.region)}/${r.coll}/${encodeURIComponent(r.name)}`

/** relPath turns a selfLink or partial URL into "projects/...". */
export function relPath(link: string | undefined): string {
  if (!link) return ''
  const i = link.indexOf('projects/')
  return (i >= 0 ? link.slice(i) : link).replace(/\/+$/, '')
}

/** parseRef reads a resource path or selfLink; undefined for other links. */
export function parseRef(link: string | undefined): Ref | undefined {
  const m = /^projects\/([^/]+)\/(?:global|regions\/([^/]+))\/([^/]+)\/([^/]+)$/.exec(relPath(link))
  if (!m || !kindInfo(m[3]!)) return undefined
  return {
    project: m[1]!,
    region: m[2] ?? '',
    coll: m[3] as Coll,
    name: decodeURIComponent(m[4]!),
  }
}

/** refOf is the Ref of a listed resource. */
export const refOf = (r: Resource, coll: Coll, project: string): Ref =>
  parseRef(r.selfLink) ?? { project, region: '', coll, name: r.name }

/** text is a scalar API field as text; "" for anything else. */
export const text = (v: unknown): string =>
  typeof v === 'string' ? v : typeof v === 'number' || typeof v === 'boolean' ? String(v) : ''

/** port is a forwarding rule's port: its portRange, one port as "80". */
export function port(fr: Resource): string {
  const [lo = '', hi] = text(fr.portRange).split('-')
  return hi && hi !== lo ? `${lo}-${hi}` : lo
}

/** lastSeg is a link's last path segment (a resource's name). */
export const lastSeg = (s?: string) => (s ? decodeURIComponent(s.split('/').pop() ?? '') : '')

/** scopeOf is a resource's scope: "global", or its region. */
export const scopeOf = (r: Resource) => lastSeg(r.region) || 'global'

/** Operation is a compute Operation. */
export interface Operation {
  name: string
  status?: 'PENDING' | 'RUNNING' | 'DONE'
  selfLink?: string
  operationType?: string
  targetLink?: string
  error?: { errors?: { code?: string; message?: string }[] }
}

const send = <T>(path: string, method: string, body?: unknown) =>
  gcpFetch<T>(API + path, {
    method,
    body: body === undefined ? undefined : JSON.stringify(body),
  })

/**
 * waitOperation resolves once op is DONE, or rejects with its error.
 * Writes wait, so that the next page shows what they did.
 */
export async function waitOperation(op: Operation): Promise<Operation> {
  let cur = op
  while (cur.status !== 'DONE') {
    cur = await send<Operation>(`${relPath(cur.selfLink)}/wait`, 'POST')
  }
  const message = cur.error?.errors?.find((e) => e.message)?.message
  if (message)
    throw new ApiError(400, cur.error?.errors?.[0]?.code ?? 'FAILED_PRECONDITION', message)
  return cur
}

const write = async (path: string, method: string, body?: unknown) =>
  waitOperation(await send<Operation>(path, method, body))

type Aggregated = { items?: Record<string, Record<string, Resource[] | undefined>> }

/** listKind lists a Project's resources of a kind in every scope. */
export async function listKind(project: string, coll: Coll): Promise<Resource[]> {
  const k = kindInfo(coll)
  if (k?.regional) {
    const res = await gcpFetch<Aggregated>(`${API}projects/${project}/aggregated/${coll}`)
    return Object.values(res.items ?? {}).flatMap((s) => s[coll] ?? [])
  }
  return (
    (await gcpFetch<{ items?: Resource[] }>(`${API}projects/${project}/global/${coll}`)).items ?? []
  )
}

export const getResource = (r: Ref) => gcpFetch<Resource>(API + resourcePath(r))

export const insertResource = (project: string, region: string, coll: Coll, body: unknown) =>
  write(`${scopePath(project, region)}/${coll}`, 'POST', body)

/** patchResource sends a JSON merge patch (patch() of every kind). */
export const patchResource = (r: Ref, body: unknown) => write(resourcePath(r), 'PATCH', body)

export const deleteResource = (r: Ref) => write(resourcePath(r), 'DELETE')

export interface HealthStatus {
  ipAddress?: string
  port?: number
  healthState?: string
  instance?: string
  forwardingRule?: string
  forwardingRuleIp?: string
}

/** getHealth is backendServices.getHealth of one backend group. */
export const getHealth = async (r: Ref, group: string) =>
  (
    await gcpFetch<{ healthStatus?: HealthStatus[] }>(`${API}${resourcePath(r)}/getHealth`, {
      method: 'POST',
      body: JSON.stringify({ group }),
    })
  ).healthStatus ?? []

/** route asks which route and backend of a stored URL map serve a request. */
export const route = (body: LbRouteRequest) => unwrap(admin.POST('/_emu/v1/lb/route', { body }))

// ---- merge patch ----

type Obj = Record<string, unknown>
export const isObj = (v: unknown): v is Obj =>
  typeof v === 'object' && v !== null && !Array.isArray(v)

const same = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b)

/**
 * mergeDiff is the JSON merge patch (RFC 7386) that turns cur into next:
 * changed fields, objects recursively and null for removed fields. It is
 * empty when nothing changed.
 */
export function mergeDiff(cur: Obj, next: Obj): Obj {
  const out: Obj = {}
  for (const k of Object.keys(cur)) if (!(k in next)) out[k] = null
  for (const [k, v] of Object.entries(next)) {
    const was = cur[k]
    if (isObj(was) && isObj(v)) {
      const d = mergeDiff(was, v)
      if (Object.keys(d).length) out[k] = d
    } else if (!same(was, v)) out[k] = v
  }
  return out
}

/** applyMerge applies a JSON merge patch to cur. */
export function applyMerge(cur: Obj, patch: Obj): Obj {
  const out: Obj = { ...cur }
  for (const [k, v] of Object.entries(patch)) {
    if (v === null) delete out[k]
    else if (isObj(v) && isObj(out[k])) out[k] = applyMerge(out[k], v)
    else out[k] = v
  }
  return out
}

// ---- queries ----

export const kindQuery = (project: string, coll: Coll, enabled = true) =>
  queryOptions({
    queryKey: ['lb', 'list', project, coll],
    queryFn: () => listKind(project, coll),
    enabled: enabled && !!project,
  })

export const resourceQuery = (r: Ref) =>
  queryOptions({
    queryKey: ['lb', 'resource', r],
    queryFn: () => getResource(r),
  })

/** HEALTH_POLL_MS is how often backend health refreshes: probes are not events. */
export const HEALTH_POLL_MS = 2000

export const healthQuery = (r: Ref, group: string) =>
  queryOptions({
    queryKey: ['lb', 'health', r, group],
    queryFn: () => getHealth(r, group),
    refetchInterval: HEALTH_POLL_MS,
  })

/** listenersQuery is the local listener of each forwarding rule (FR-LB-002). */
export const listenersQuery = (project: string) =>
  queryOptions({
    queryKey: ['lb', 'listeners', project],
    queryFn: async () => {
      const res = await unwrap(
        admin.GET('/_emu/v1/lb/listeners', { params: { query: { project } } }),
      )
      return new Map(res.listeners.map((l) => [l.forwardingRule, l]))
    },
    enabled: !!project,
  })

/**
 * negsQuery lists the Project's network endpoint groups, which backend
 * services balance across. They are compute resources.
 */
export const negsQuery = (project: string, enabled = true) =>
  queryOptions({
    queryKey: ['compute', 'lbNegs', project],
    queryFn: async () => {
      const coll = 'networkEndpointGroups'
      const res = await gcpFetch<Aggregated>(`${API}projects/${project}/aggregated/${coll}`)
      return Object.values(res.items ?? {}).flatMap((s) => s[coll] ?? [])
    },
    enabled: enabled && !!project,
  })
