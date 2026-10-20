import { queryOptions } from '@tanstack/react-query'

import { gcpFetch } from '@/api/fetch'

import { listAll, locationName, path, waitOperation, type Operation } from './api'

// The TLS policies of the Certificate Manager view (FR-LB-006): the
// Network Security resources the certs Service also serves, all through
// networksecurity/v1 REST. Server TLS policies verify a load balancer's
// clients, backend authentication configs its backends, and client TLS
// policies are the client side of service-to-service TLS. Types cover
// the fields the view reads; the raw JSON editor covers the rest.

/** CertificateProvider is where a TLS policy's own certificate comes from. */
export interface CertificateProvider {
  grpcEndpoint?: { targetUri?: string }
  certificateProviderInstance?: { pluginInstance?: string }
}

/** ValidationCa is a CA a TLS policy verifies the other side with. */
export type ValidationCa = CertificateProvider

export interface MtlsPolicy {
  clientValidationMode?: string
  clientValidationTrustConfig?: string
  clientValidationCa?: ValidationCa[]
}

/** TlsResource is any of the three kinds; each sets only its own fields. */
export interface TlsResource {
  name: string
  description?: string
  labels?: Record<string, string>
  createTime?: string
  updateTime?: string
  // serverTlsPolicies
  allowOpen?: boolean
  serverCertificate?: CertificateProvider
  mtlsPolicy?: MtlsPolicy
  // clientTlsPolicies
  sni?: string
  serverValidationCa?: ValidationCa[]
  // clientTlsPolicies (a CertificateProvider) and backendAuthenticationConfigs (a certificate)
  clientCertificate?: CertificateProvider | string
  // backendAuthenticationConfigs
  trustConfig?: string
  wellKnownRoots?: string
}

/** TLS_KINDS are the view's resource types, in the order of its tabs. */
export const TLS_KINDS = {
  serverTlsPolicies: {
    idParam: 'serverTlsPolicyId',
    singular: 'server TLS policy',
    label: 'Server TLS policy',
    title: 'Server TLS policies',
    gcloud: 'server-tls-policies',
    tofu: 'google_network_security_server_tls_policy',
  },
  backendAuthenticationConfigs: {
    idParam: 'backendAuthenticationConfigId',
    singular: 'backend authentication config',
    label: 'Backend authentication config',
    title: 'Backend authentication configs',
    gcloud: 'backend-authentication-configs',
    tofu: 'google_network_security_backend_authentication_config',
  },
  clientTlsPolicies: {
    idParam: 'clientTlsPolicyId',
    singular: 'client TLS policy',
    label: 'Client TLS policy',
    title: 'Client TLS policies',
    gcloud: 'client-tls-policies',
    tofu: 'google_network_security_client_tls_policy',
  },
} as const

export type TlsColl = keyof typeof TLS_KINDS
export const TLS_COLLS = Object.keys(TLS_KINDS) as TlsColl[]

export const isTlsColl = (s: string | undefined): s is TlsColl => !!s && Object.hasOwn(TLS_KINDS, s)

/** collOf is the kind a route's :coll names, server TLS policies by default. */
export const collOf = (s: string | undefined): TlsColl => (isTlsColl(s) ? s : 'serverTlsPolicies')

/** CLIENT_VALIDATION_LABELS name a server TLS policy's mTLS modes. */
export const CLIENT_VALIDATION_LABELS: Record<string, string> = {
  '': 'No client certificates (TLS only)',
  ALLOW_INVALID_OR_MISSING_CLIENT_CERT: 'Allow invalid or missing client certificates',
  REJECT_INVALID: 'Reject invalid client certificates',
}

/** TlsRef names a resource of one of the kinds. */
export interface TlsRef {
  project: string
  location: string
  coll: TlsColl
  id: string
}

export const NS_API = '/networksecurity/v1'

export const tlsName = (r: TlsRef) => `${locationName(r.project, r.location)}/${r.coll}/${r.id}`

/** tlsRefOf is the TlsRef of a resource's name, in any of its forms. */
export function tlsRefOf(name: string): TlsRef | undefined {
  const m = /projects\/([^/]+)\/locations\/([^/]+)\/([^/]+)\/([^/]+)$/.exec(name)
  if (!m || !isTlsColl(m[3])) return undefined
  return { project: m[1]!, location: m[2]!, coll: m[3], id: m[4]! }
}

/** tlsLabel names a resource by ID, with its Project when it is not project. */
export function tlsLabel(name: string, project: string): string {
  const r = tlsRefOf(name)
  if (!r) return name
  return r.project === project ? r.id : `${r.project}/${r.id}`
}

/** clientValidation is how a server TLS policy checks client certificates, if it does. */
export const clientValidation = (r: TlsResource) => r.mtlsPolicy?.clientValidationMode ?? ''

/** referencedTrustConfig is the trust config a resource verifies with, if any. */
export const referencedTrustConfig = (r: TlsResource) =>
  r.trustConfig ?? r.mtlsPolicy?.clientValidationTrustConfig ?? ''

// ---- reads ----

/** tlsListQuery lists one kind's resources of every location ("-"). */
export const tlsListQuery = (project: string, coll: TlsColl) =>
  queryOptions({
    queryKey: ['certs', 'tls', project, coll],
    queryFn: () =>
      listAll<TlsResource>(`${NS_API}/${path(locationName(project, '-'))}/${coll}`, coll),
  })

export const tlsQuery = (r: TlsRef) =>
  queryOptions({
    queryKey: ['certs', 'tls', r],
    queryFn: () => gcpFetch<TlsResource>(`${NS_API}/${path(tlsName(r))}`),
  })

// ---- writes ----

export const createTls = async (r: TlsRef, body: unknown) =>
  waitOperation<TlsResource>(
    await gcpFetch<Operation>(
      `${NS_API}/${path(locationName(r.project, r.location))}/${r.coll}?${new URLSearchParams({ [TLS_KINDS[r.coll].idParam]: r.id })}`,
      { method: 'POST', body: JSON.stringify(body) },
    ),
    NS_API,
  )

export const patchTls = async (r: TlsRef, body: unknown, mask: string[]) =>
  waitOperation<TlsResource>(
    await gcpFetch<Operation>(
      `${NS_API}/${path(tlsName(r))}?${new URLSearchParams({ updateMask: mask.join(',') })}`,
      { method: 'PATCH', body: JSON.stringify(body) },
    ),
    NS_API,
  )

export const deleteTls = async (r: TlsRef) =>
  waitOperation(
    await gcpFetch<Operation>(`${NS_API}/${path(tlsName(r))}`, { method: 'DELETE' }),
    NS_API,
  )
