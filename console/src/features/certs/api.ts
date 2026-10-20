import { queryOptions } from '@tanstack/react-query'

import { admin, unwrap } from '@/api/admin'
import { ApiError } from '@/api/errors'
import { gcpFetch } from '@/api/fetch'

// The Certificate Manager view's reads and writes, all through
// certificatemanager/v1 REST. Types cover the fields the view reads; the
// raw JSON editor covers the rest. The API never returns a private key.

export interface CnameTroubleshooting {
  name?: string
  expectedData?: string
  resolvedData?: string[]
}

export interface IpTroubleshooting {
  resolved?: string[]
  serving?: string[]
  servingOnAltPorts?: string[]
}

export interface AuthorizationAttemptInfo {
  domain?: string
  attemptTime?: string
  state?: string
  failureReason?: string
  details?: string
  troubleshooting?: {
    cname?: CnameTroubleshooting
    ips?: IpTroubleshooting
    issues?: string[]
  }
}

export interface Managed {
  domains?: string[]
  dnsAuthorizations?: string[]
  issuanceConfig?: string
  state?: string
  provisioningIssue?: { reason?: string; details?: string }
  authorizationAttemptInfo?: AuthorizationAttemptInfo[]
}

export interface Certificate {
  name: string
  description?: string
  labels?: Record<string, string>
  scope?: string
  createTime?: string
  updateTime?: string
  pemCertificate?: string
  sanDnsnames?: string[]
  expireTime?: string
  managed?: Managed
  usedBy?: { name?: string }[]
}

/** DnsAuthorization proves control of a domain with a CNAME record. */
export interface DnsAuthorization {
  name: string
  domain?: string
  type?: string
  description?: string
  labels?: Record<string, string>
  createTime?: string
  updateTime?: string
  dnsResourceRecord?: { name?: string; type?: string; data?: string }
}

/** IssuanceConfig has a CA Service CA pool issue the certificates that name it. */
export interface IssuanceConfig {
  name: string
  description?: string
  labels?: Record<string, string>
  createTime?: string
  updateTime?: string
  certificateAuthorityConfig?: { certificateAuthorityServiceConfig?: { caPool?: string } }
  lifetime?: string
  rotationWindowPercentage?: number
  keyAlgorithm?: string
}

/** TrustConfig is the CAs and certificates that verify the other side of mTLS. */
export interface TrustConfig {
  name: string
  description?: string
  labels?: Record<string, string>
  createTime?: string
  updateTime?: string
  etag?: string
  trustStores?: {
    trustAnchors?: { pemCertificate?: string }[]
    intermediateCas?: { pemCertificate?: string }[]
  }[]
  allowlistedCertificates?: { pemCertificate?: string }[]
}

/** GclbTarget is a target proxy that serves a certificate map. */
export interface GclbTarget {
  targetHttpsProxy?: string
  targetSslProxy?: string
  ipConfigs?: { ipAddress?: string; ports?: number[] }[]
}

export interface CertificateMap {
  name: string
  description?: string
  labels?: Record<string, string>
  createTime?: string
  updateTime?: string
  gclbTargets?: GclbTarget[]
}

export interface CertificateMapEntry {
  name: string
  description?: string
  labels?: Record<string, string>
  hostname?: string
  matcher?: string
  certificates?: string[]
  state?: string
  createTime?: string
  updateTime?: string
}

export interface Location {
  name: string
  locationId: string
  displayName?: string
}

/** Operation is a google.longrunning Operation. */
export interface Operation {
  name: string
  done?: boolean
  error?: { code?: number; status?: string; message?: string }
  response?: unknown
}

/** CertRef names a certificate. */
export interface CertRef {
  project: string
  location: string
  cert: string
}

/** MapRef names a certificate map, which is always global. */
export interface MapRef {
  project: string
  map: string
}

/** EntryRef names a certificate map entry. */
export interface EntryRef extends MapRef {
  entry: string
}

const enc = encodeURIComponent
const API = '/certificatemanager/v1'

/** path encodes each segment of a resource name for a URL path. */
export const path = (name: string) => name.split('/').map(enc).join('/')

export const locationName = (project: string, location: string) =>
  `projects/${project}/locations/${location}`

export const certName = (r: CertRef) =>
  `${locationName(r.project, r.location)}/certificates/${r.cert}`

export const mapName = (r: MapRef) =>
  `${locationName(r.project, 'global')}/certificateMaps/${r.map}`

export const entryName = (r: EntryRef) => `${mapName(r)}/certificateMapEntries/${r.entry}`

/** mapRefOf is the MapRef of a certificate map's name, in any of its forms. */
export function mapRefOf(name: string): MapRef | undefined {
  const m = /projects\/([^/]+)\/locations\/global\/certificateMaps\/([^/]+)$/.exec(name)
  return m ? { project: m[1]!, map: m[2]! } : undefined
}

/** CERT_MAP_PREFIX begins a certificate map's name where a target proxy references it. */
export const CERT_MAP_PREFIX = '//certificatemanager.googleapis.com/'

/** mapLabel names a certificate map by ID, with its Project when it is not project. */
export function mapLabel(name: string, project: string): string {
  const r = mapRefOf(name)
  if (!r) return name
  return r.project === project ? r.map : `${r.project}/${r.map}`
}

/** lastSegment is the ID at the end of a resource name. */
export const lastSegment = (name: string) => name.slice(name.lastIndexOf('/') + 1)

/** locationOf is the location in a resource name. */
export const locationOf = (name: string) => /\/locations\/([^/]+)/.exec(name)?.[1] ?? ''

/** CERT_ID is the API's resource ID rule. */
export const CERT_ID = /^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$/
export const certIdMessage = (id: string, field = 'certificateId') =>
  id
    ? `Invalid ${field} ${JSON.stringify(id)}: must be 1-63 characters long, start with a lowercase letter and contain only lowercase letters, digits and hyphens.`
    : `Field ${field} is required.`

// ---- what a certificate is ----

/** CertKind is how a certificate is issued, as the create form chooses. */
export const CERT_KINDS = ['self', 'dns', 'lb', 'issuance'] as const
export type CertKind = (typeof CERT_KINDS)[number]

export function certKind(c: Pick<Certificate, 'managed'>): CertKind {
  const m = c.managed
  if (!m) return 'self'
  if (m.issuanceConfig) return 'issuance'
  if (m.dnsAuthorizations?.length) return 'dns'
  return 'lb'
}

export const KIND_LABELS: Record<CertKind, string> = {
  self: 'Self-managed',
  dns: 'Google-managed (DNS authorization)',
  lb: 'Google-managed (load balancer authorization)',
  issuance: 'Google-managed (certificate issuance config)',
}

export const SCOPES = [
  ['DEFAULT', 'Default'],
  ['EDGE_CACHE', 'Edge cache'],
  ['ALL_REGIONS', 'All regions'],
  ['CLIENT_AUTH', 'Client authentication'],
] as const

export const scopeLabel = (scope?: string) =>
  SCOPES.find(([s]) => s === (scope || 'DEFAULT'))?.[1] ?? scope ?? 'Default'

/**
 * certState is a managed certificate's state; a self-managed one is
 * ACTIVE until it expires (by the browser's clock).
 */
export function certState(c: Certificate): string {
  if (c.managed) return c.managed.state ?? 'PROVISIONING'
  const exp = c.expireTime ? Date.parse(c.expireTime) : NaN
  return exp < Date.now() ? 'EXPIRED' : 'ACTIVE'
}

/** certDomains are the names a certificate covers, or will. */
export const certDomains = (c: Certificate) =>
  c.sanDnsnames?.length ? c.sanDnsnames : (c.managed?.domains ?? [])

/** caPoolOf is the CA Service CA pool a certificate issuance config issues from. */
export const caPoolOf = (c: IssuanceConfig) =>
  c.certificateAuthorityConfig?.certificateAuthorityServiceConfig?.caPool ?? ''

/**
 * keyAlgorithmLabel is a certificate issuance config's key algorithm. The
 * Service applies no default to a config that names none.
 */
export const keyAlgorithmLabel = (c: IssuanceConfig) => c.keyAlgorithm || 'Unspecified'

const DAY = 86400

/** lifetimeLabel is a duration such as "2592000s" in days, when it is whole days. */
export function lifetimeLabel(d?: string): string | undefined {
  if (!d) return undefined
  const s = Number(d.replace(/s$/, ''))
  if (!Number.isFinite(s) || s <= 0 || s % DAY !== 0) return d
  const days = s / DAY
  return `${days} ${days === 1 ? 'day' : 'days'}`
}

/** trustCount is how many certificates of one role a trust config holds. */
export function trustCount(
  t: TrustConfig,
  role: 'trustAnchors' | 'intermediateCas' | 'allowlistedCertificates',
): number {
  if (role === 'allowlistedCertificates') return t.allowlistedCertificates?.length ?? 0
  return (t.trustStores ?? []).reduce((n, s) => n + (s[role]?.length ?? 0), 0)
}

// ---- reads ----

export async function listAll<T>(url: string, field: string): Promise<T[]> {
  const out: T[] = []
  let pageToken = ''
  do {
    const q = new URLSearchParams()
    if (pageToken) q.set('pageToken', pageToken)
    const res = await gcpFetch<Record<string, unknown>>(`${url}${q.size ? `?${q}` : ''}`)
    out.push(...((res[field] as T[] | undefined) ?? []))
    pageToken = (res.nextPageToken as string | undefined) ?? ''
  } while (pageToken)
  return out
}

export const locationsQuery = (project: string) =>
  queryOptions({
    queryKey: ['certs', 'locations', project],
    queryFn: () => listAll<Location>(`${API}/projects/${enc(project)}/locations`, 'locations'),
    staleTime: Infinity,
  })

/** certificatesQuery lists the certificates of every location ("-"). */
export const certificatesQuery = (project: string) =>
  queryOptions({
    queryKey: ['certs', 'certificates', project],
    queryFn: () =>
      listAll<Certificate>(
        `${API}/${path(locationName(project, '-'))}/certificates`,
        'certificates',
      ),
  })

/** PROVISIONING_POLL_MS is how often a provisioning certificate is read again. */
export const PROVISIONING_POLL_MS = 1000

export const certificateQuery = (r: CertRef) =>
  queryOptions({
    queryKey: ['certs', 'certificate', r],
    queryFn: () => gcpFetch<Certificate>(`${API}/${path(certName(r))}`),
    // The Service provisions in the background: follow it to ACTIVE.
    refetchInterval: (q) =>
      q.state.data?.managed?.state === 'PROVISIONING' ? PROVISIONING_POLL_MS : false,
  })

export const dnsAuthorizationsQuery = (project: string, location: string) =>
  queryOptions({
    queryKey: ['certs', 'dnsAuthorizations', project, location],
    queryFn: () =>
      listAll<DnsAuthorization>(
        `${API}/${path(locationName(project, location))}/dnsAuthorizations`,
        'dnsAuthorizations',
      ),
  })

export const issuanceConfigsQuery = (project: string, location: string) =>
  queryOptions({
    queryKey: ['certs', 'issuanceConfigs', project, location],
    queryFn: () =>
      listAll<IssuanceConfig>(
        `${API}/${path(locationName(project, location))}/certificateIssuanceConfigs`,
        'certificateIssuanceConfigs',
      ),
  })

export const trustConfigsQuery = (project: string, location: string) =>
  queryOptions({
    queryKey: ['certs', 'trustConfigs', project, location],
    queryFn: () =>
      listAll<TrustConfig>(
        `${API}/${path(locationName(project, location))}/trustConfigs`,
        'trustConfigs',
      ),
  })

/** TrustRef names a trust config. */
export interface TrustRef {
  project: string
  location: string
  trust: string
}

export const trustName = (r: TrustRef) =>
  `${locationName(r.project, r.location)}/trustConfigs/${r.trust}`

export const trustConfigQuery = (r: TrustRef) =>
  queryOptions({
    queryKey: ['certs', 'trustConfig', r],
    queryFn: () => gcpFetch<TrustConfig>(`${API}/${path(trustName(r))}`),
  })

/** DnsAuthRef names a DNS authorization. */
export interface DnsAuthRef {
  project: string
  location: string
  auth: string
}

export const dnsAuthName = (r: DnsAuthRef) =>
  `${locationName(r.project, r.location)}/dnsAuthorizations/${r.auth}`

export const dnsAuthorizationQuery = (r: DnsAuthRef) =>
  queryOptions({
    queryKey: ['certs', 'dnsAuthorization', r],
    queryFn: () => gcpFetch<DnsAuthorization>(`${API}/${path(dnsAuthName(r))}`),
  })

/** DNS_AUTH_TYPES name the kinds of record a DNS authorization asks for. */
export const DNS_AUTH_TYPES: Record<string, string> = {
  FIXED_RECORD: 'Fixed record',
  PER_PROJECT_RECORD: 'Per-project record',
}

/** IssuanceRef names a certificate issuance config. */
export interface IssuanceRef {
  project: string
  location: string
  config: string
}

export const issuanceName = (r: IssuanceRef) =>
  `${locationName(r.project, r.location)}/certificateIssuanceConfigs/${r.config}`

export const issuanceConfigQuery = (r: IssuanceRef) =>
  queryOptions({
    queryKey: ['certs', 'issuanceConfig', r],
    queryFn: () => gcpFetch<IssuanceConfig>(`${API}/${path(issuanceName(r))}`),
  })

/** mapsQuery lists the certificate maps, which are all global. */
export const mapsQuery = (project: string) =>
  queryOptions({
    queryKey: ['certs', 'maps', project],
    queryFn: () =>
      listAll<CertificateMap>(
        `${API}/${path(locationName(project, 'global'))}/certificateMaps`,
        'certificateMaps',
      ),
  })

export const mapQuery = (r: MapRef) =>
  queryOptions({
    queryKey: ['certs', 'map', r],
    queryFn: () => gcpFetch<CertificateMap>(`${API}/${path(mapName(r))}`),
  })

export const entriesQuery = (r: MapRef) =>
  queryOptions({
    queryKey: ['certs', 'entries', r],
    queryFn: () =>
      listAll<CertificateMapEntry>(
        `${API}/${path(mapName(r))}/certificateMapEntries`,
        'certificateMapEntries',
      ),
  })

export const entryQuery = (r: EntryRef) =>
  queryOptions({
    queryKey: ['certs', 'entry', r],
    queryFn: () => gcpFetch<CertificateMapEntry>(`${API}/${path(entryName(r))}`),
  })

// ---- operations ----

// GRPC_CODES are the canonical status names by code, for Operation errors.
const GRPC_CODES = [
  'OK',
  'CANCELLED',
  'UNKNOWN',
  'INVALID_ARGUMENT',
  'DEADLINE_EXCEEDED',
  'NOT_FOUND',
  'ALREADY_EXISTS',
  'PERMISSION_DENIED',
  'RESOURCE_EXHAUSTED',
  'FAILED_PRECONDITION',
  'ABORTED',
  'OUT_OF_RANGE',
  'UNIMPLEMENTED',
  'INTERNAL',
  'UNAVAILABLE',
  'DATA_LOSS',
  'UNAUTHENTICATED',
]

/** OP_POLL_MS is how often waitOperation checks an Operation. */
export const OP_POLL_MS = 250

/**
 * waitOperation resolves with op's response once it is done, or rejects
 * with its error. api is the API that started it.
 */
export async function waitOperation<T>(op: Operation, api = API): Promise<T> {
  let cur = op
  while (!cur.done) {
    await new Promise((r) => setTimeout(r, OP_POLL_MS))
    cur = await gcpFetch<Operation>(`${api}/${path(cur.name)}`)
  }
  if (cur.error) {
    const status = GRPC_CODES[cur.error.code ?? 2] ?? 'UNKNOWN'
    throw new ApiError(400, status, cur.error.message ?? 'The operation failed.')
  }
  return cur.response as T
}

// ---- writes ----

const json = (body: unknown) => ({ body: JSON.stringify(body) })

/** create creates a resource in parent's collection coll with the ID param idParam. */
const create = async <T>(
  parent: string,
  coll: string,
  idParam: string,
  id: string,
  body: unknown,
) =>
  waitOperation<T>(
    await gcpFetch<Operation>(
      `${API}/${path(parent)}/${coll}?${new URLSearchParams({ [idParam]: id })}`,
      { method: 'POST', ...json(body) },
    ),
  )

const patch = async <T>(name: string, body: unknown, mask: string[]) =>
  waitOperation<T>(
    await gcpFetch<Operation>(
      `${API}/${path(name)}?${new URLSearchParams({ updateMask: mask.join(',') })}`,
      { method: 'PATCH', ...json(body) },
    ),
  )

const remove = async (name: string) =>
  waitOperation(await gcpFetch<Operation>(`${API}/${path(name)}`, { method: 'DELETE' }))

export const createCertificate = (r: CertRef, body: unknown) =>
  create<Certificate>(
    locationName(r.project, r.location),
    'certificates',
    'certificateId',
    r.cert,
    body,
  )

export const patchCertificate = (r: CertRef, body: unknown, mask: string[]) =>
  patch<Certificate>(certName(r), body, mask)

export const deleteCertificate = (r: CertRef) => remove(certName(r))

export const createMap = (r: MapRef, body: unknown) =>
  create<CertificateMap>(
    locationName(r.project, 'global'),
    'certificateMaps',
    'certificateMapId',
    r.map,
    body,
  )

export const patchMap = (r: MapRef, body: unknown, mask: string[]) =>
  patch<CertificateMap>(mapName(r), body, mask)

export const deleteMap = (r: MapRef) => remove(mapName(r))

export const createEntry = (r: EntryRef, body: unknown) =>
  create<CertificateMapEntry>(
    mapName(r),
    'certificateMapEntries',
    'certificateMapEntryId',
    r.entry,
    body,
  )

export const patchEntry = (r: EntryRef, body: unknown, mask: string[]) =>
  patch<CertificateMapEntry>(entryName(r), body, mask)

export const deleteEntry = (r: EntryRef) => remove(entryName(r))

export const createDnsAuthorization = (r: DnsAuthRef, body: unknown) =>
  create<DnsAuthorization>(
    locationName(r.project, r.location),
    'dnsAuthorizations',
    'dnsAuthorizationId',
    r.auth,
    body,
  )

export const patchDnsAuthorization = (r: DnsAuthRef, body: unknown, mask: string[]) =>
  patch<DnsAuthorization>(dnsAuthName(r), body, mask)

export const deleteDnsAuthorization = (r: DnsAuthRef) => remove(dnsAuthName(r))

export const createTrustConfig = (r: TrustRef, body: unknown) =>
  create<TrustConfig>(
    locationName(r.project, r.location),
    'trustConfigs',
    'trustConfigId',
    r.trust,
    body,
  )

/** patchTrustConfig patches a trust config; an etag in body must be its current one. */
export const patchTrustConfig = (r: TrustRef, body: unknown, mask: string[]) =>
  patch<TrustConfig>(trustName(r), body, mask)

export const deleteTrustConfig = (r: TrustRef) => remove(trustName(r))

export const createIssuanceConfig = (r: IssuanceRef, body: unknown) =>
  create<IssuanceConfig>(
    locationName(r.project, r.location),
    'certificateIssuanceConfigs',
    'certificateIssuanceConfigId',
    r.config,
    body,
  )

export const deleteIssuanceConfig = (r: IssuanceRef) => remove(issuanceName(r))

/**
 * rotateCa replaces the Instance CA and resolves once every Google-managed
 * certificate is re-issued, with the path of the new ca.pem.
 */
export const rotateCa = () => unwrap(admin.POST('/_emu/v1/ca/rotate'))
