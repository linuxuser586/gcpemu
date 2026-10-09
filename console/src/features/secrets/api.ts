import { queryOptions } from '@tanstack/react-query'

import { gcpFetch } from '@/api/fetch'

// The Secret Manager view's reads and writes, all through
// secretmanager/v1 REST. Types cover the fields the view reads; the raw
// JSON editor covers the rest.

export interface Topic {
  name: string
}

export interface Replication {
  automatic?: { customerManagedEncryption?: { kmsKeyName?: string } }
  userManaged?: { replicas?: { location: string }[] }
}

export interface Secret {
  name: string
  replication?: Replication
  createTime?: string
  labels?: Record<string, string>
  annotations?: Record<string, string>
  topics?: Topic[]
  expireTime?: string
  ttl?: string
  etag?: string
  rotation?: {
    nextRotationTime?: string
    rotationPeriod?: string
    managedRotationStatus?: { state?: string; error?: { message?: string } }
  }
  versionAliases?: Record<string, string>
  versionDestroyTtl?: string
  secretType?: string
}

export type VersionState = 'ENABLED' | 'DISABLED' | 'DESTROYED'

export interface SecretVersion {
  name: string
  createTime?: string
  destroyTime?: string
  scheduledDestroyTime?: string
  state?: VersionState
  etag?: string
  clientSpecifiedPayloadChecksum?: boolean
}

export interface Location {
  name: string
  locationId: string
  displayName?: string
}

/** SecretRef names a secret: location is empty for a global secret. */
export interface SecretRef {
  project: string
  location: string
  secret: string
}

const enc = encodeURIComponent
const API = '/secretmanager/v1'

/** parent is the API's parent of a Project's (or a location's) secrets. */
export const parent = (project: string, location: string) =>
  `projects/${enc(project)}${location ? `/locations/${enc(location)}` : ''}`

export const secretName = (r: SecretRef) =>
  `${parent(r.project, r.location)}/secrets/${enc(r.secret)}`

/** secretId is the last segment of a secret or version name. */
export const lastSegment = (name: string) => name.slice(name.lastIndexOf('/') + 1)

/** versionId is a version's number from its name. */
export const versionId = (name: string) => lastSegment(name)

/** SECRET_ID is the API's secret ID rule. */
export const SECRET_ID = /^[a-zA-Z0-9_-]{1,255}$/
export const secretIdMessage = (id: string) =>
  `Secret ID ${JSON.stringify(id)} is invalid: it must be 1-255 characters of letters, numbers, hyphens and underscores.`

const json = (body: unknown) => ({ body: JSON.stringify(body) })

// ---- payloads ----

/** encodePayload is text as the base64 the API's SecretPayload.data takes. */
export function encodePayload(text: string): string {
  const bytes = new TextEncoder().encode(text)
  let bin = ''
  for (const b of bytes) bin += String.fromCharCode(b)
  return btoa(bin)
}

/**
 * decodePayload reads SecretPayload.data: the text when it is UTF-8,
 * otherwise undefined (show the base64).
 */
export function decodePayload(data: string): string | undefined {
  const bin = atob(data)
  const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0))
  try {
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch {
    return undefined
  }
}

// ---- locations ----

export const locationsQuery = (project: string) =>
  queryOptions({
    queryKey: ['secrets', 'locations', project],
    queryFn: async () =>
      (await gcpFetch<{ locations?: Location[] }>(`${API}/projects/${enc(project)}/locations`))
        .locations ?? [],
    staleTime: Infinity,
  })

// ---- secrets ----

export async function listSecrets(project: string, location: string) {
  const out: Secret[] = []
  let pageToken = ''
  do {
    const q = new URLSearchParams()
    if (pageToken) q.set('pageToken', pageToken)
    const res = await gcpFetch<{ secrets?: Secret[]; nextPageToken?: string }>(
      `${API}/${parent(project, location)}/secrets?${q}`,
    )
    out.push(...(res.secrets ?? []))
    pageToken = res.nextPageToken ?? ''
  } while (pageToken)
  return out
}

export const secretsQuery = (project: string, location: string) =>
  queryOptions({
    queryKey: ['secrets', 'list', project, location],
    queryFn: () => listSecrets(project, location),
  })

export const getSecret = (r: SecretRef) => gcpFetch<Secret>(`${API}/${secretName(r)}`)

export const secretQuery = (r: SecretRef) =>
  queryOptions({
    queryKey: ['secrets', 'secret', r],
    queryFn: () => getSecret(r),
  })

export const createSecret = (project: string, location: string, id: string, body: unknown) =>
  gcpFetch<Secret>(
    `${API}/${parent(project, location)}/secrets?${new URLSearchParams({ secretId: id })}`,
    { method: 'POST', ...json(body) },
  )

/** patchSecret sends secrets.patch with updateMask naming the fields to change. */
export const patchSecret = (r: SecretRef, body: unknown, mask: string[]) =>
  gcpFetch<Secret>(
    `${API}/${secretName(r)}?${new URLSearchParams({ updateMask: mask.join(',') })}`,
    { method: 'PATCH', ...json(body) },
  )

export const deleteSecret = (r: SecretRef) =>
  gcpFetch<void>(`${API}/${secretName(r)}`, { method: 'DELETE' })

export const rotateSecret = (r: SecretRef) =>
  gcpFetch<SecretVersion>(`${API}/${secretName(r)}:rotateSecret`, { method: 'POST', ...json({}) })

// ---- versions ----

export async function listVersions(r: SecretRef) {
  const out: SecretVersion[] = []
  let pageToken = ''
  do {
    const q = new URLSearchParams()
    if (pageToken) q.set('pageToken', pageToken)
    const res = await gcpFetch<{ versions?: SecretVersion[]; nextPageToken?: string }>(
      `${API}/${secretName(r)}/versions?${q}`,
    )
    out.push(...(res.versions ?? []))
    pageToken = res.nextPageToken ?? ''
  } while (pageToken)
  return out
}

export const versionsQuery = (r: SecretRef) =>
  queryOptions({
    queryKey: ['secrets', 'versions', r],
    queryFn: () => listVersions(r),
  })

export const addVersion = (r: SecretRef, text: string) =>
  gcpFetch<SecretVersion>(`${API}/${secretName(r)}:addVersion`, {
    method: 'POST',
    ...json({ payload: { data: encodePayload(text) } }),
  })

export const accessVersion = (r: SecretRef, version: string) =>
  gcpFetch<{ name: string; payload?: { data?: string; dataCrc32c?: string } }>(
    `${API}/${secretName(r)}/versions/${enc(version)}:access`,
  )

/** changeVersion enables, disables or destroys a version. */
export const changeVersion = (
  r: SecretRef,
  version: string,
  action: 'enable' | 'disable' | 'destroy',
) =>
  gcpFetch<SecretVersion>(`${API}/${secretName(r)}/versions/${enc(version)}:${action}`, {
    method: 'POST',
    ...json({}),
  })

// ---- durations ----

/** seconds reads a protobuf Duration ("86400s") as whole seconds. */
export const seconds = (d?: string) => (d ? d.replace(/s$/, '').replace(/\.0+$/, '') : '')

/** duration writes whole seconds as a protobuf Duration. */
export const duration = (s: string) => `${s}s`
