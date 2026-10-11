import { queryOptions } from '@tanstack/react-query'

import { admin, unwrap } from '@/api/admin'
import type { components } from '@/api/admin.gen'
import { toApiError } from '@/api/errors'
import { gcpFetch, origin, send } from '@/api/fetch'

// The IAM view's reads and writes: service accounts, keys and roles
// through iam/v1 REST, policies through each resource's own API, the
// policy simulator through the OAuth2 token endpoint and
// testIamPermissions, and the IAM enforcement level through the admin
// API. Types cover the fields the view reads; the raw JSON editors cover
// the rest.

export type EnforcementLevel = components['schemas']['IamEnforcement']['level']

export interface ServiceAccount {
  name: string
  projectId?: string
  uniqueId?: string
  email: string
  displayName?: string
  description?: string
  oauth2ClientId?: string
  disabled?: boolean
  etag?: string
}

export interface ServiceAccountKey {
  name: string
  keyAlgorithm?: string
  keyOrigin?: string
  keyType?: 'USER_MANAGED' | 'SYSTEM_MANAGED'
  validAfterTime?: string
  validBeforeTime?: string
  disabled?: boolean
  privateKeyType?: string
  privateKeyData?: string
}

export type RoleStage = 'ALPHA' | 'BETA' | 'GA' | 'DEPRECATED' | 'DISABLED' | 'EAP'

export interface Role {
  name: string
  title?: string
  description?: string
  includedPermissions?: string[]
  stage?: RoleStage
  etag?: string
  deleted?: boolean
}

export interface Expr {
  title?: string
  description?: string
  expression: string
}

export interface Binding {
  role: string
  members?: string[]
  condition?: Expr
}

export interface Policy {
  version?: number
  bindings?: Binding[]
  auditConfigs?: unknown[]
  etag?: string
  [k: string]: unknown
}

const enc = encodeURIComponent
const IAM = '/iam/v1'
const json = (body: unknown) => ({ body: JSON.stringify(body) })

/** lastSegment is the last segment of a resource name. */
export const lastSegment = (name: string) => name.slice(name.lastIndexOf('/') + 1)

// ---- validation, as the API words it ----

/** ACCOUNT_ID is the API's service account ID rule. */
export const ACCOUNT_ID = /^[a-z]([-a-z0-9]*[a-z0-9])$/
export const accountIdMessage = (id: string) =>
  `Invalid account ID: ${JSON.stringify(id)}. The account ID must be between 6 and 30 characters, start with a lowercase letter, and contain only lowercase letters, digits and dashes.`
export const validAccountId = (id: string) =>
  id.length >= 6 && id.length <= 30 && ACCOUNT_ID.test(id)

/** ROLE_ID is the API's custom role ID rule. */
export const ROLE_ID = /^[a-zA-Z0-9_.]{3,64}$/
export const roleIdMessage = (id: string) =>
  `The role ID ${JSON.stringify(id)} is invalid. It must be 3 to 64 characters of letters, digits, underscores and periods.`

/** PERMISSION is the API's permission name rule. */
export const PERMISSION = /^[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+$/

/** MEMBER is the API's binding member rule. */
export const MEMBER =
  /^(allUsers|allAuthenticatedUsers|(user|serviceAccount|group|domain|projectOwner|projectEditor|projectViewer|principal|principalSet):.+|deleted:(user|serviceAccount|group):.+)$/

/** PRINCIPAL is what the policy simulator can act as: a user or a service account. */
export const PRINCIPAL = /^(user|serviceAccount):[^\s@]+@[^\s@]+$/

// ---- IAM enforcement level (admin API) ----

export const setEnforcement = (level: EnforcementLevel) =>
  unwrap(admin.PUT('/_emu/v1/iam/enforcement', { body: { level } }))

// ---- service accounts ----

/** saPath is the API path of a Project's service account. */
export const saPath = (project: string, email: string) =>
  `${IAM}/projects/${enc(project)}/serviceAccounts/${enc(email)}`

export async function listServiceAccounts(project: string) {
  const out: ServiceAccount[] = []
  let pageToken = ''
  do {
    const q = new URLSearchParams()
    if (pageToken) q.set('pageToken', pageToken)
    const res = await gcpFetch<{ accounts?: ServiceAccount[]; nextPageToken?: string }>(
      `${IAM}/projects/${enc(project)}/serviceAccounts?${q}`,
    )
    out.push(...(res.accounts ?? []))
    pageToken = res.nextPageToken ?? ''
  } while (pageToken)
  return out
}

/** serviceAccountsQuery lists a Project's service accounts. */
export const serviceAccountsQuery = (project: string) =>
  queryOptions({
    queryKey: ['iam', 'serviceAccounts', project],
    queryFn: () => listServiceAccounts(project),
  })

export const serviceAccountQuery = (project: string, email: string) =>
  queryOptions({
    queryKey: ['iam', 'serviceAccount', project, email],
    queryFn: () => gcpFetch<ServiceAccount>(saPath(project, email)),
  })

/** createServiceAccount sends serviceAccounts.create's request: accountId and serviceAccount. */
export const createServiceAccount = (project: string, body: unknown) =>
  gcpFetch<ServiceAccount>(`${IAM}/projects/${enc(project)}/serviceAccounts`, {
    method: 'POST',
    ...json(body),
  })

/** patchServiceAccount sends serviceAccounts.patch's request: serviceAccount and updateMask. */
export const patchServiceAccount = (project: string, email: string, body: unknown) =>
  gcpFetch<ServiceAccount>(saPath(project, email), { method: 'PATCH', ...json(body) })

export const deleteServiceAccount = (project: string, email: string) =>
  gcpFetch<void>(saPath(project, email), { method: 'DELETE' })

/** setAccountDisabled enables or disables a service account. */
export const setAccountDisabled = (project: string, email: string, disabled: boolean) =>
  gcpFetch<void>(`${saPath(project, email)}:${disabled ? 'disable' : 'enable'}`, {
    method: 'POST',
    ...json({}),
  })

/** undeleteServiceAccount restores a deleted account by its unique ID. */
export const undeleteServiceAccount = (uniqueId: string) =>
  gcpFetch<{ restoredAccount?: ServiceAccount }>(
    `${IAM}/projects/-/serviceAccounts/${enc(uniqueId)}:undelete`,
    { method: 'POST', ...json({}) },
  )

// ---- keys ----

export const keysQuery = (project: string, email: string) =>
  queryOptions({
    queryKey: ['iam', 'keys', project, email],
    queryFn: async () =>
      (await gcpFetch<{ keys?: ServiceAccountKey[] }>(`${saPath(project, email)}/keys`)).keys ?? [],
  })

/** createKey creates a JSON key; its privateKeyData is the key file, base64. */
export const createKey = (project: string, email: string) =>
  gcpFetch<ServiceAccountKey>(`${saPath(project, email)}/keys`, {
    method: 'POST',
    ...json({ privateKeyType: 'TYPE_GOOGLE_CREDENTIALS_FILE', keyAlgorithm: 'KEY_ALG_RSA_2048' }),
  })

/** uploadKey uploads a public key certificate (PEM). */
export const uploadKey = (project: string, email: string, pem: string) =>
  gcpFetch<ServiceAccountKey>(`${saPath(project, email)}/keys:upload`, {
    method: 'POST',
    ...json({ publicKeyData: encodeBase64(pem) }),
  })

export const deleteKey = (project: string, email: string, key: string) =>
  gcpFetch<void>(`${saPath(project, email)}/keys/${enc(key)}`, { method: 'DELETE' })

export const setKeyDisabled = (project: string, email: string, key: string, disabled: boolean) =>
  gcpFetch<void>(`${saPath(project, email)}/keys/${enc(key)}:${disabled ? 'disable' : 'enable'}`, {
    method: 'POST',
    ...json({}),
  })

/** encodeBase64 is UTF-8 text as base64. */
export function encodeBase64(text: string): string {
  let bin = ''
  for (const b of new TextEncoder().encode(text)) bin += String.fromCharCode(b)
  return btoa(bin)
}

/** decodeBase64 is base64 as UTF-8 text. */
export function decodeBase64(data: string): string {
  return new TextDecoder().decode(Uint8Array.from(atob(data), (c) => c.charCodeAt(0)))
}

// ---- roles ----

/** predefinedRolesQuery is the Instance's catalogue of predefined roles, with permissions. */
export const predefinedRolesQuery = () =>
  queryOptions({
    queryKey: ['iam', 'roles', 'predefined'],
    queryFn: async () => {
      const out: Role[] = []
      let pageToken = ''
      do {
        const q = new URLSearchParams({ view: 'FULL', pageSize: '1000' })
        if (pageToken) q.set('pageToken', pageToken)
        const res = await gcpFetch<{ roles?: Role[]; nextPageToken?: string }>(`${IAM}/roles?${q}`)
        out.push(...(res.roles ?? []))
        pageToken = res.nextPageToken ?? ''
      } while (pageToken)
      return out
    },
    staleTime: Infinity,
  })

/** customRolesQuery lists a Project's custom roles, deleted ones too, with permissions. */
export const customRolesQuery = (project: string) =>
  queryOptions({
    queryKey: ['iam', 'roles', 'custom', project],
    queryFn: async () => {
      const out: Role[] = []
      let pageToken = ''
      do {
        const q = new URLSearchParams({ view: 'FULL', showDeleted: 'true', pageSize: '1000' })
        if (pageToken) q.set('pageToken', pageToken)
        const res = await gcpFetch<{ roles?: Role[]; nextPageToken?: string }>(
          `${IAM}/projects/${enc(project)}/roles?${q}`,
        )
        out.push(...(res.roles ?? []))
        pageToken = res.nextPageToken ?? ''
      } while (pageToken)
      return out
    },
  })

/** customRolePath is the API path of a Project's custom role. */
export const customRolePath = (project: string, id: string) =>
  `${IAM}/projects/${enc(project)}/roles/${enc(id)}`

export const predefinedRoleQuery = (id: string) =>
  queryOptions({
    queryKey: ['iam', 'role', 'roles', id],
    queryFn: () => gcpFetch<Role>(`${IAM}/roles/${enc(id)}`),
    staleTime: Infinity,
  })

export const customRoleQuery = (project: string, id: string) =>
  queryOptions({
    queryKey: ['iam', 'role', 'projects', project, id],
    queryFn: () => gcpFetch<Role>(customRolePath(project, id)),
  })

export const createRole = (project: string, roleId: string, role: unknown) =>
  gcpFetch<Role>(`${IAM}/projects/${enc(project)}/roles`, {
    method: 'POST',
    ...json({ roleId, role }),
  })

/** patchRole sends roles.patch with updateMask naming the fields; the etag guards it. */
export const patchRole = (project: string, id: string, role: unknown, mask: string[]) =>
  gcpFetch<Role>(
    `${customRolePath(project, id)}?${new URLSearchParams({ updateMask: mask.join(',') })}`,
    {
      method: 'PATCH',
      ...json(role),
    },
  )

export const deleteRole = (project: string, id: string, etag?: string) =>
  gcpFetch<Role>(
    `${customRolePath(project, id)}${etag ? `?${new URLSearchParams({ etag })}` : ''}`,
    { method: 'DELETE' },
  )

export const undeleteRole = (project: string, id: string, etag?: string) =>
  gcpFetch<Role>(`${customRolePath(project, id)}:undelete`, {
    method: 'POST',
    ...json(etag ? { etag } : {}),
  })

/** permissionsQuery lists every permission the Instance knows. */
export const permissionsQuery = (project: string) =>
  queryOptions({
    queryKey: ['iam', 'permissions', project],
    queryFn: async () =>
      (
        await gcpFetch<{ permissions?: { name: string }[] }>(
          `${IAM}/permissions:queryTestablePermissions`,
          {
            method: 'POST',
            ...json({
              fullResourceName: `//cloudresourcemanager.googleapis.com/projects/${project}`,
              pageSize: 1000,
            }),
          },
        )
      ).permissions?.map((p) => p.name) ?? [],
    staleTime: Infinity,
  })

// ---- policies on any resource ----

/**
 * How a resource's API serves its IAM methods: Resource Manager and IAM
 * take POST :getIamPolicy; Pub/Sub, Artifact Registry, Secret Manager and
 * Network Security GET it; Compute uses /getIamPolicy path segments; and
 * Cloud Storage has its own /iam resource.
 */
type Style = 'post' | 'get' | 'compute' | 'gcs'

export type KindId =
  | 'project'
  | 'serviceAccount'
  | 'bucket'
  | 'topic'
  | 'subscription'
  | 'repository'
  | 'secret'
  | 'subnetwork'
  | 'serverTlsPolicy'
  | 'clientTlsPolicy'

export interface PolicyKind {
  id: KindId
  label: string
  /** The Service that serves the resource. */
  service: string
  /** How the resource is named in the view, relative to the Project; absent for the Project. */
  name?: { label: string; placeholder: string; pattern: RegExp }
  /** Permission prefixes the simulator tests by default. */
  perms: string[]
  style: Style
  /** path is the API path of the resource, without the IAM verb. */
  path: (project: string, parts: string[]) => string
  /** fullName is the resource's full name, as IAM conditions see it. */
  fullName: (project: string, parts: string[]) => string
}

const proj = (p: string) => `projects/${enc(p)}`
const segs = (n: number, label: string, placeholder: string) => ({
  label,
  placeholder,
  pattern: new RegExp(`^${Array.from({ length: n }, () => '[^/\\s]+').join('/')}$`),
})

export const KINDS: readonly PolicyKind[] = [
  {
    id: 'project',
    label: 'Project',
    service: 'iam',
    perms: ['resourcemanager.projects.'],
    style: 'post',
    path: (p) => `/cloudresourcemanager/v1/${proj(p)}`,
    fullName: (p) => `//cloudresourcemanager.googleapis.com/projects/${p}`,
  },
  {
    id: 'serviceAccount',
    label: 'Service account',
    service: 'iam',
    name: {
      label: 'Email',
      placeholder: 'NAME@PROJECT.iam.gserviceaccount.com',
      pattern: /^\S+@\S+$/,
    },
    perms: ['iam.serviceAccounts.', 'iam.serviceAccountKeys.'],
    style: 'post',
    path: (p, [email = '']) => saPath(p, email),
    fullName: (p, [email = '']) => `//iam.googleapis.com/projects/${p}/serviceAccounts/${email}`,
  },
  {
    id: 'bucket',
    label: 'Cloud Storage bucket',
    service: 'gcs',
    name: segs(1, 'Bucket', 'BUCKET'),
    perms: ['storage.buckets.', 'storage.objects.'],
    style: 'gcs',
    path: (_, [b = '']) => `/storage/v1/b/${enc(b)}`,
    fullName: (_, [b = '']) => `//storage.googleapis.com/projects/_/buckets/${b}`,
  },
  {
    id: 'topic',
    label: 'Pub/Sub topic',
    service: 'pubsub',
    name: segs(1, 'Topic', 'TOPIC'),
    perms: ['pubsub.topics.'],
    style: 'get',
    path: (p, [t = '']) => `/pubsub/v1/${proj(p)}/topics/${enc(t)}`,
    fullName: (p, [t = '']) => `//pubsub.googleapis.com/projects/${p}/topics/${t}`,
  },
  {
    id: 'subscription',
    label: 'Pub/Sub subscription',
    service: 'pubsub',
    name: segs(1, 'Subscription', 'SUBSCRIPTION'),
    perms: ['pubsub.subscriptions.'],
    style: 'get',
    path: (p, [s = '']) => `/pubsub/v1/${proj(p)}/subscriptions/${enc(s)}`,
    fullName: (p, [s = '']) => `//pubsub.googleapis.com/projects/${p}/subscriptions/${s}`,
  },
  {
    id: 'repository',
    label: 'Artifact Registry repository',
    service: 'ar',
    name: segs(2, 'Repository', 'LOCATION/REPOSITORY'),
    perms: ['artifactregistry.'],
    style: 'get',
    path: (p, [l = '', r = '']) =>
      `/artifactregistry/v1/${proj(p)}/locations/${enc(l)}/repositories/${enc(r)}`,
    fullName: (p, [l = '', r = '']) =>
      `//artifactregistry.googleapis.com/projects/${p}/locations/${l}/repositories/${r}`,
  },
  {
    id: 'secret',
    label: 'Secret',
    service: 'secrets',
    name: {
      label: 'Secret',
      placeholder: 'SECRET or LOCATION/SECRET',
      pattern: /^([^/\s]+\/)?[^/\s]+$/,
    },
    perms: ['secretmanager.secrets.', 'secretmanager.versions.'],
    style: 'get',
    path: (p, parts) => {
      const [l, s] = parts.length > 1 ? parts : ['', parts[0] ?? '']
      return `/secretmanager/v1/${proj(p)}${l ? `/locations/${enc(l)}` : ''}/secrets/${enc(s ?? '')}`
    },
    fullName: (p, parts) => {
      const [l, s] = parts.length > 1 ? parts : ['', parts[0] ?? '']
      return `//secretmanager.googleapis.com/projects/${p}${l ? `/locations/${l}` : ''}/secrets/${s}`
    },
  },
  {
    id: 'subnetwork',
    label: 'VPC subnetwork',
    service: 'compute',
    name: segs(2, 'Subnetwork', 'REGION/SUBNETWORK'),
    perms: ['compute.subnetworks.'],
    style: 'compute',
    path: (p, [r = '', n = '']) => `/compute/v1/${proj(p)}/regions/${enc(r)}/subnetworks/${enc(n)}`,
    fullName: (p, [r = '', n = '']) =>
      `//compute.googleapis.com/projects/${p}/regions/${r}/subnetworks/${n}`,
  },
  {
    id: 'serverTlsPolicy',
    label: 'Server TLS policy',
    service: 'certs',
    name: segs(2, 'Server TLS policy', 'LOCATION/POLICY'),
    perms: ['networksecurity.serverTlsPolicies.'],
    style: 'get',
    path: (p, [l = '', n = '']) =>
      `/networksecurity/v1/${proj(p)}/locations/${enc(l)}/serverTlsPolicies/${enc(n)}`,
    fullName: (p, [l = '', n = '']) =>
      `//networksecurity.googleapis.com/projects/${p}/locations/${l}/serverTlsPolicies/${n}`,
  },
  {
    id: 'clientTlsPolicy',
    label: 'Client TLS policy',
    service: 'certs',
    name: segs(2, 'Client TLS policy', 'LOCATION/POLICY'),
    perms: ['networksecurity.clientTlsPolicies.'],
    style: 'get',
    path: (p, [l = '', n = '']) =>
      `/networksecurity/v1/${proj(p)}/locations/${enc(l)}/clientTlsPolicies/${enc(n)}`,
    fullName: (p, [l = '', n = '']) =>
      `//networksecurity.googleapis.com/projects/${p}/locations/${l}/clientTlsPolicies/${n}`,
  },
]

const byId = new Map(KINDS.map((k) => [k.id, k]))

/** kindOf is the kind with id, or the Project's. */
export const kindOf = (id: string | null | undefined): PolicyKind =>
  byId.get(id as KindId) ?? KINDS[0]!

/** ResourceRef names a resource whose policy the view reads. */
export interface ResourceRef {
  project: string
  kind: KindId
  /** The name relative to the Project, as KINDS words it; '' for the Project. */
  name: string
}

const partsOf = (r: ResourceRef) => (r.name ? r.name.split('/') : [])

/** resourcePath is the API path of the resource ref names. */
export const resourcePath = (r: ResourceRef) => kindOf(r.kind).path(r.project, partsOf(r))

/** fullResourceName is the full name of the resource ref names. */
export const fullResourceName = (r: ResourceRef) => kindOf(r.kind).fullName(r.project, partsOf(r))

/** nameError explains why name does not name a resource of kind, if it does not. */
export function nameError(kind: PolicyKind, name: string): string | undefined {
  if (!kind.name) return undefined
  if (!name) return `Enter the ${kind.name.label.toLowerCase()}.`
  if (!kind.name.pattern.test(name))
    return `Enter the ${kind.name.label.toLowerCase()} as ${kind.name.placeholder}.`
  return undefined
}

export async function getPolicy(r: ResourceRef): Promise<Policy> {
  const base = resourcePath(r)
  switch (kindOf(r.kind).style) {
    case 'post':
      return gcpFetch<Policy>(`${base}:getIamPolicy`, {
        method: 'POST',
        ...json({ options: { requestedPolicyVersion: 3 } }),
      })
    case 'get':
      return gcpFetch<Policy>(`${base}:getIamPolicy`)
    case 'compute':
      return gcpFetch<Policy>(`${base}/getIamPolicy`)
    case 'gcs':
      return gcpFetch<Policy>(`${base}/iam`)
  }
}

export const policyQuery = (r: ResourceRef) =>
  queryOptions({
    queryKey: ['iam', 'policy', r],
    queryFn: () => getPolicy(r),
  })

/** setPolicy replaces the resource's policy; its etag guards against concurrent changes. */
export async function setPolicy(r: ResourceRef, policy: Policy): Promise<Policy> {
  const base = resourcePath(r)
  switch (kindOf(r.kind).style) {
    case 'post':
    case 'get':
      return gcpFetch<Policy>(`${base}:setIamPolicy`, { method: 'POST', ...json({ policy }) })
    case 'compute':
      return gcpFetch<Policy>(`${base}/setIamPolicy`, { method: 'POST', ...json({ policy }) })
    case 'gcs':
      return gcpFetch<Policy>(`${base}/iam`, { method: 'PUT', ...json(policy) })
  }
}

/**
 * testPermissions sends the resource's testIamPermissions with token as
 * the caller's credentials, and returns the permissions the caller holds.
 */
export async function testPermissions(
  r: ResourceRef,
  permissions: string[],
  token: string,
): Promise<string[]> {
  const base = resourcePath(r)
  const headers = { Authorization: `Bearer ${token}` }
  let res: { permissions?: string[] }
  switch (kindOf(r.kind).style) {
    case 'post':
    case 'get':
      res = await gcpFetch(`${base}:testIamPermissions`, {
        method: 'POST',
        headers,
        ...json({ permissions }),
      })
      break
    case 'compute':
      res = await gcpFetch(`${base}/testIamPermissions`, {
        method: 'POST',
        headers,
        ...json({ permissions }),
      })
      break
    case 'gcs': {
      const q = new URLSearchParams()
      for (const p of permissions) q.append('permissions', p)
      res = await gcpFetch(`${base}/iam/testPermissions?${q}`, { headers })
      break
    }
  }
  return res.permissions ?? []
}

// ---- tokens for the policy simulator ----

/**
 * mintToken gets an access token for principal from the Instance's OAuth2
 * token endpoint, through the refresh token grant that user ADC uses: the
 * emulator takes "user:EMAIL" or "serviceAccount:EMAIL" as the refresh
 * token and issues a token for that Principal.
 */
export async function mintToken(principal: string): Promise<string> {
  const res = await send(
    new Request(new URL('/oauth2/token', origin()), {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ grant_type: 'refresh_token', refresh_token: principal }),
      credentials: 'omit',
    }),
  )
  const body = (await res.json().catch(() => ({}))) as {
    access_token?: string
    error?: string
    error_description?: string
  }
  if (!res.ok || !body.access_token) {
    throw toApiError(res.status, {
      error: { message: body.error_description ?? body.error ?? `HTTP ${res.status}` },
    })
  }
  return body.access_token
}

/** revokeToken revokes a token the simulator no longer needs. */
export async function revokeToken(token: string): Promise<void> {
  await send(
    new Request(new URL('/oauth2/revoke', origin()), {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ token }),
      credentials: 'omit',
    }),
  ).catch(() => undefined)
}
