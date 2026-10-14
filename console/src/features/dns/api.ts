import { queryOptions } from '@tanstack/react-query'

import { admin, unwrap } from '@/api/admin'
import type { components } from '@/api/admin.gen'
import { gcpFetch } from '@/api/fetch'

// The Cloud DNS view's reads and writes, through dns/v1 REST: managed
// zones, their record sets and the changes that edit them (FR-DNS-001/002).
// Record sets are written only through changes.create, as the API's
// change history then records every edit. The query box asks the
// Instance's DNS server through the admin API, as a browser cannot speak
// DNS. Types cover the fields the view reads; the raw JSON editor covers
// the rest.

export type DnsQueryResult = components['schemas']['DnsQueryResult']
export type DnsRecord = components['schemas']['DnsRecord']

export interface NetworkBinding {
  networkUrl: string
}

export interface ManagedZone {
  name: string
  dnsName: string
  description?: string
  id?: string
  nameServers?: string[]
  creationTime?: string
  visibility?: 'public' | 'private'
  privateVisibilityConfig?: {
    networks?: NetworkBinding[]
    gkeClusters?: { gkeClusterName: string }[]
  }
  labels?: Record<string, string>
  dnssecConfig?: {
    state?: string
    nonExistence?: string
    defaultKeySpecs?: { keyType?: string; algorithm?: string; keyLength?: number }[]
  }
  forwardingConfig?: {
    targetNameServers?: { ipv4Address?: string; ipv6Address?: string; forwardingPath?: string }[]
  }
  peeringConfig?: { targetNetwork?: { networkUrl?: string } }
  cloudLoggingConfig?: { enableLogging?: boolean }
}

export interface ResourceRecordSet {
  name: string
  type: string
  ttl?: number
  rrdatas?: string[]
  routingPolicy?: {
    wrr?: { items?: { weight?: number; rrdatas?: string[] }[] }
    geo?: { items?: { location?: string; rrdatas?: string[] }[] }
  }
}

export interface Change {
  id: string
  status?: 'pending' | 'done'
  startTime?: string
  additions?: ResourceRecordSet[]
  deletions?: ResourceRecordSet[]
}

/** ZoneRef names a managed zone. */
export interface ZoneRef {
  project: string
  zone: string
}

const enc = encodeURIComponent
const API = '/dns/v1'

const projectPath = (project: string) => `${API}/projects/${enc(project)}`
const zonePath = (r: ZoneRef) => `${projectPath(r.project)}/managedZones/${enc(r.zone)}`
const json = (body: unknown) => ({ body: JSON.stringify(body) })

/** TYPES are the record types the API takes (FR-DNS-002), most used first. */
export const TYPES = [
  'A',
  'AAAA',
  'CNAME',
  'MX',
  'TXT',
  'SRV',
  'NS',
  'SOA',
  'CAA',
  'PTR',
  'SPF',
  'DS',
  'HTTPS',
  'SVCB',
  'TLSA',
  'NAPTR',
  'SSHFP',
] as const

/** ZONE_NAME is the API's managed zone name rule. */
export const ZONE_NAME = /^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$/

/** isDnsName reports whether s is an absolute domain name, as the API checks it. */
export function isDnsName(s: string): boolean {
  if (s === '.' || !s.endsWith('.') || /[\s;"()]/.test(s) || s.length > 254) return false
  return s
    .slice(0, -1)
    .split('.')
    .every((l) => l.length > 0 && l.length <= 63)
}

/** inZone reports whether name is the apex of or below origin. */
export function inZone(name: string, origin: string): boolean {
  const n = name.toLowerCase()
  const o = origin.toLowerCase()
  return n === o || n.endsWith(`.${o}`)
}

// The API's validation messages (services/dns/validate.go, as Cloud DNS).
export const invalid = (field: string, value: unknown) =>
  `Invalid value for '${field}': '${String(value)}'`
export const required = (field: string) => `Required field '${field}' not specified`

/** networkPath reduces a network URL to projects/P/global/networks/N. */
export function networkPath(url: string): string {
  const i = url.indexOf('projects/')
  return i >= 0 ? url.slice(i) : url
}

/** networkLabel names a network for people: its name, with a foreign Project. */
export function networkLabel(url: string, project: string): string {
  const m = /^projects\/([^/]+)\/global\/networks\/([^/]+)$/.exec(networkPath(url))
  if (!m) return url
  return m[1] === project ? m[2]! : `${m[2]} (${m[1]})`
}

/** zoneNetworks are the networks a private zone is bound to. */
export const zoneNetworks = (z: ManagedZone) =>
  (z.privateVisibilityConfig?.networks ?? []).map((n) => networkPath(n.networkUrl))

/** rrsetKey identifies a record set within its zone. */
export const rrsetKey = (rs: Pick<ResourceRecordSet, 'name' | 'type'>) =>
  `${rs.name.toLowerCase()} ${rs.type}`

/** isApexRecord: the zone's own SOA and NS, which it cannot lose. */
export const isApexRecord = (rs: ResourceRecordSet, z: ManagedZone) =>
  rs.name.toLowerCase() === z.dnsName.toLowerCase() && (rs.type === 'SOA' || rs.type === 'NS')

// ---- zones ----

async function listAll<T>(path: string, key: string): Promise<T[]> {
  const out: T[] = []
  let pageToken = ''
  do {
    const q = new URLSearchParams()
    if (pageToken) q.set('pageToken', pageToken)
    const res = await gcpFetch<Record<string, unknown>>(
      `${path}${path.includes('?') ? '&' : '?'}${q}`,
    )
    out.push(...((res[key] as T[] | undefined) ?? []))
    pageToken = (res.nextPageToken as string | undefined) ?? ''
  } while (pageToken)
  return out
}

export const zonesQuery = (project: string) =>
  queryOptions({
    queryKey: ['dns', 'zones', project],
    queryFn: () => listAll<ManagedZone>(`${projectPath(project)}/managedZones`, 'managedZones'),
  })

export const zoneQuery = (r: ZoneRef) =>
  queryOptions({
    queryKey: ['dns', 'zone', r],
    queryFn: () => gcpFetch<ManagedZone>(zonePath(r)),
  })

export const createZone = (project: string, body: unknown) =>
  gcpFetch<ManagedZone>(`${projectPath(project)}/managedZones`, { method: 'POST', ...json(body) })

/** patchZone sends managedZones.patch: the fields in body are replaced. */
export const patchZone = (r: ZoneRef, body: unknown) =>
  gcpFetch<unknown>(zonePath(r), { method: 'PATCH', ...json(body) })

export const deleteZone = (r: ZoneRef) => gcpFetch<void>(zonePath(r), { method: 'DELETE' })

// ---- record sets ----

export const rrsetsQuery = (r: ZoneRef) =>
  queryOptions({
    queryKey: ['dns', 'rrsets', r],
    queryFn: () => listAll<ResourceRecordSet>(`${zonePath(r)}/rrsets`, 'rrsets'),
  })

// ---- changes ----

export const changesQuery = (r: ZoneRef) =>
  queryOptions({
    queryKey: ['dns', 'changes', r],
    queryFn: () =>
      listAll<Change>(
        `${zonePath(r)}/changes?sortBy=changeSequence&sortOrder=descending`,
        'changes',
      ),
  })

export const getChange = (r: ZoneRef, id: string) =>
  gcpFetch<Change>(`${zonePath(r)}/changes/${enc(id)}`)

/** CHANGE_POLL_MS is how often a pending change is asked about. */
export const CHANGE_POLL_MS = 500

/**
 * applyChange sends changes.create and waits until the change is done
 * (FR-CORE-023: changes stay pending for the configured LRO latency).
 */
export async function applyChange(
  r: ZoneRef,
  change: { additions?: ResourceRecordSet[]; deletions?: ResourceRecordSet[] } & Record<
    string,
    unknown
  >,
): Promise<Change> {
  let c = await gcpFetch<Change>(`${zonePath(r)}/changes`, { method: 'POST', ...json(change) })
  while (c.status === 'pending') {
    await new Promise((res) => setTimeout(res, CHANGE_POLL_MS))
    c = await getChange(r, c.id)
  }
  return c
}

// ---- the query box ----

export interface QueryParams {
  name: string
  type: string
  network: string
}

export const dnsQuery = (p: QueryParams) =>
  queryOptions({
    queryKey: ['dns', 'query', p],
    queryFn: () =>
      unwrap(
        admin.GET('/_emu/v1/dns/query', {
          params: {
            query: { name: p.name, type: p.type, ...(p.network ? { network: p.network } : {}) },
          },
        }),
      ),
    staleTime: 0,
  })

// ---- networks (compute v1, for network bindings) ----

export const networksQuery = (project: string) =>
  queryOptions({
    queryKey: ['compute', 'networks', project],
    queryFn: async () =>
      (
        await gcpFetch<{ items?: { name: string; selfLink?: string }[] }>(
          `/compute/v1/projects/${enc(project)}/global/networks`,
        )
      ).items ?? [],
  })

// ---- errors ----

/**
 * apiField finds the form field an API error message names: fields maps
 * the API's field paths (e.g. "entity.managedZone.dnsName") to the
 * form's. Cloud DNS names the field in the message, not in details.
 */
export function apiField<F extends string>(
  message: string,
  fields: readonly (readonly [api: string | RegExp, form: F])[],
): F | undefined {
  const m = /'(entity\.[^']+|parameters\.[^']+)'/.exec(message)
  if (!m) return undefined
  const path = m[1]!
  for (const [api, form] of fields) {
    if (
      typeof api === 'string'
        ? path === api || path.startsWith(`${api}.`) || path.startsWith(`${api}[`)
        : api.test(path)
    )
      return form
  }
  return undefined
}
