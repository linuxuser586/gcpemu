import { queryOptions } from '@tanstack/react-query'

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

export interface DnsAuthorization {
  name: string
  domain?: string
  type?: string
  description?: string
  dnsResourceRecord?: { name?: string; type?: string; data?: string }
}

export interface IssuanceConfig {
  name: string
  description?: string
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

const enc = encodeURIComponent
const API = '/certificatemanager/v1'

/** path encodes each segment of a resource name for a URL path. */
const path = (name: string) => name.split('/').map(enc).join('/')

export const locationName = (project: string, location: string) =>
  `projects/${project}/locations/${location}`

export const certName = (r: CertRef) =>
  `${locationName(r.project, r.location)}/certificates/${r.cert}`

/** lastSegment is the ID at the end of a resource name. */
export const lastSegment = (name: string) => name.slice(name.lastIndexOf('/') + 1)

/** locationOf is the location in a resource name. */
export const locationOf = (name: string) => /\/locations\/([^/]+)/.exec(name)?.[1] ?? ''

/** CERT_ID is the API's resource ID rule. */
export const CERT_ID = /^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$/
export const certIdMessage = (id: string) =>
  id
    ? `Invalid certificateId ${JSON.stringify(id)}: must be 1-63 characters long, start with a lowercase letter and contain only lowercase letters, digits and hyphens.`
    : 'Field certificateId is required.'

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

// ---- reads ----

async function listAll<T>(url: string, field: string): Promise<T[]> {
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

/** waitOperation resolves with op's response once it is done, or rejects with its error. */
export async function waitOperation<T>(op: Operation): Promise<T> {
  let cur = op
  while (!cur.done) {
    await new Promise((r) => setTimeout(r, OP_POLL_MS))
    cur = await gcpFetch<Operation>(`${API}/${path(cur.name)}`)
  }
  if (cur.error) {
    const status = GRPC_CODES[cur.error.code ?? 2] ?? 'UNKNOWN'
    throw new ApiError(400, status, cur.error.message ?? 'The operation failed.')
  }
  return cur.response as T
}

// ---- writes ----

const json = (body: unknown) => ({ body: JSON.stringify(body) })

export const createCertificate = async (r: CertRef, body: unknown) =>
  waitOperation<Certificate>(
    await gcpFetch<Operation>(
      `${API}/${path(locationName(r.project, r.location))}/certificates?${new URLSearchParams({ certificateId: r.cert })}`,
      { method: 'POST', ...json(body) },
    ),
  )

export const patchCertificate = async (r: CertRef, body: unknown, mask: string[]) =>
  waitOperation<Certificate>(
    await gcpFetch<Operation>(
      `${API}/${path(certName(r))}?${new URLSearchParams({ updateMask: mask.join(',') })}`,
      { method: 'PATCH', ...json(body) },
    ),
  )

export const deleteCertificate = async (r: CertRef) =>
  waitOperation(await gcpFetch<Operation>(`${API}/${path(certName(r))}`, { method: 'DELETE' }))
