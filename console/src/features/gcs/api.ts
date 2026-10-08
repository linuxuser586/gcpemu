import { infiniteQueryOptions, queryOptions } from '@tanstack/react-query'

import { toApiError } from '@/api/errors'
import { gcpFetch, origin, send } from '@/api/fetch'

// The Cloud Storage view's reads and writes, all through the JSON API
// (storage/v1), plus IAM Credentials signBlob for V4 signed URLs
// (signedUrl.ts). Types cover the fields the view reads; the raw JSON
// editor covers the rest.

export interface Bucket {
  name: string
  id?: string
  location?: string
  locationType?: string
  storageClass?: string
  projectNumber?: string
  timeCreated?: string
  updated?: string
  metageneration?: string
  labels?: Record<string, string>
  versioning?: { enabled?: boolean }
  iamConfiguration?: {
    uniformBucketLevelAccess?: { enabled?: boolean; lockedTime?: string }
    publicAccessPrevention?: string
  }
  retentionPolicy?: { retentionPeriod?: string; effectiveTime?: string; isLocked?: boolean }
  defaultEventBasedHold?: boolean
  softDeletePolicy?: { retentionDurationSeconds?: string; effectiveTime?: string }
  website?: { mainPageSuffix?: string; notFoundPage?: string }
  cors?: unknown[]
  lifecycle?: { rule?: unknown[] }
  rpo?: string
  etag?: string
  selfLink?: string
}

export interface StorageObject {
  name: string
  bucket: string
  generation?: string
  metageneration?: string
  contentType?: string
  contentEncoding?: string
  contentDisposition?: string
  contentLanguage?: string
  cacheControl?: string
  size?: string
  md5Hash?: string
  crc32c?: string
  etag?: string
  storageClass?: string
  timeCreated?: string
  updated?: string
  timeDeleted?: string
  timeStorageClassUpdated?: string
  temporaryHold?: boolean
  eventBasedHold?: boolean
  retentionExpirationTime?: string
  metadata?: Record<string, string>
  componentCount?: number
  selfLink?: string
  mediaLink?: string
}

/** Notification is a notification config; the API spells its fields in snake_case. */
export interface Notification {
  id: string
  topic: string
  event_types?: string[]
  payload_format?: string
  object_name_prefix?: string
  custom_attributes?: Record<string, string>
  etag?: string
  selfLink?: string
}

export const STORAGE_CLASSES = ['STANDARD', 'NEARLINE', 'COLDLINE', 'ARCHIVE'] as const
export const EVENT_TYPES = [
  'OBJECT_FINALIZE',
  'OBJECT_METADATA_UPDATE',
  'OBJECT_DELETE',
  'OBJECT_ARCHIVE',
] as const
export const PAYLOAD_FORMATS = ['JSON_API_V1', 'NONE'] as const

/**
 * validBucketName is the API's bucket naming rule (services/gcs/names.go):
 * lowercase letters, digits, '-', '_' and '.', starting and ending with a
 * letter or digit; 3–63 characters, up to 222 with dots, each part ≤ 63;
 * not an IP address; no "goog" prefix and no "google".
 */
export function validBucketName(name: string): boolean {
  const n = name.length
  if (n < 3 || n > 222 || (!name.includes('.') && n > 63)) return false
  if (!/^[a-z0-9][a-z0-9._-]*[a-z0-9]$/.test(name)) return false
  if (name.split('.').some((c) => c === '' || c.length > 63)) return false
  if (/^\d{1,3}(\.\d{1,3}){3}$/.test(name)) return false
  if (name.startsWith('goog')) return false
  return !['google', 'g00gle', 'go0gle', 'g0ogle'].some((b) => name.includes(b))
}

export const bucketNameMessage = (name: string) => `Invalid bucket name: '${name}'`

const API = '/storage/v1/b'

/** enc escapes one path segment; object names keep their slashes escaped. */
const enc = encodeURIComponent

export const bucketPath = (bucket: string) => `${API}/${enc(bucket)}`
export const objectPath = (bucket: string, name: string) => `${bucketPath(bucket)}/o/${enc(name)}`

const json = (body: unknown) => ({ body: JSON.stringify(body) })

// ---- buckets ----

export const listBuckets = async (project: string) => {
  const out: Bucket[] = []
  let pageToken = ''
  do {
    const q = new URLSearchParams({ project })
    if (pageToken) q.set('pageToken', pageToken)
    const res = await gcpFetch<{ items?: Bucket[]; nextPageToken?: string }>(`${API}?${q}`)
    out.push(...(res.items ?? []))
    pageToken = res.nextPageToken ?? ''
  } while (pageToken)
  return out
}

export const getBucket = (bucket: string) => gcpFetch<Bucket>(bucketPath(bucket))

export const createBucket = (project: string, body: unknown) =>
  gcpFetch<Bucket>(`${API}?${new URLSearchParams({ project })}`, { method: 'POST', ...json(body) })

export const patchBucket = (bucket: string, body: unknown) =>
  gcpFetch<Bucket>(bucketPath(bucket), { method: 'PATCH', ...json(body) })

export const deleteBucket = (bucket: string) =>
  gcpFetch<void>(bucketPath(bucket), { method: 'DELETE' })

export const bucketsQuery = (project: string) =>
  queryOptions({
    queryKey: ['gcs', 'buckets', project],
    queryFn: () => listBuckets(project),
  })

export const bucketQuery = (bucket: string) =>
  queryOptions({
    queryKey: ['gcs', 'bucket', bucket],
    queryFn: () => getBucket(bucket),
  })

// ---- objects ----

/** PAGE_SIZE is the objects.list page; the browser loads pages as it scrolls. */
export const PAGE_SIZE = 1000

export interface ObjectPage {
  items?: StorageObject[]
  prefixes?: string[]
  nextPageToken?: string
}

/**
 * objectsQuery lists one level of a bucket under prefix, one page at a
 * time: objects.list with delimiter "/" groups deeper names into prefixes.
 */
export const objectsQuery = (bucket: string, prefix: string) =>
  infiniteQueryOptions({
    queryKey: ['gcs', 'objects', bucket, prefix],
    queryFn: ({ pageParam }) => {
      const q = new URLSearchParams({
        delimiter: '/',
        prefix,
        maxResults: String(PAGE_SIZE),
      })
      if (pageParam) q.set('pageToken', pageParam)
      return gcpFetch<ObjectPage>(`${bucketPath(bucket)}/o?${q}`)
    },
    initialPageParam: '',
    getNextPageParam: (last) => last.nextPageToken || undefined,
  })

export const getObject = (bucket: string, name: string) =>
  gcpFetch<StorageObject>(objectPath(bucket, name))

export const objectQuery = (bucket: string, name: string) =>
  queryOptions({
    queryKey: ['gcs', 'object', bucket, name],
    queryFn: () => getObject(bucket, name),
  })

/** listGenerations returns every generation of one object, newest first. */
export async function listGenerations(bucket: string, name: string) {
  const out: StorageObject[] = []
  let pageToken = ''
  do {
    const q = new URLSearchParams({ prefix: name, versions: 'true', maxResults: '1000' })
    if (pageToken) q.set('pageToken', pageToken)
    const res = await gcpFetch<ObjectPage>(`${bucketPath(bucket)}/o?${q}`)
    out.push(...(res.items ?? []).filter((o) => o.name === name))
    pageToken = res.nextPageToken ?? ''
  } while (pageToken)
  return out.sort((a, b) => Number(BigInt(b.generation ?? 0) - BigInt(a.generation ?? 0)))
}

export const generationsQuery = (bucket: string, name: string) =>
  queryOptions({
    queryKey: ['gcs', 'generations', bucket, name],
    queryFn: () => listGenerations(bucket, name),
  })

export const patchObject = (bucket: string, name: string, body: unknown) =>
  gcpFetch<StorageObject>(objectPath(bucket, name), { method: 'PATCH', ...json(body) })

/** deleteObject deletes the live object, or one generation of it. */
export const deleteObject = (bucket: string, name: string, generation?: string) =>
  gcpFetch<void>(
    objectPath(bucket, name) + (generation ? `?${new URLSearchParams({ generation })}` : ''),
    { method: 'DELETE' },
  )

/**
 * restoreGeneration makes a noncurrent generation live again by copying
 * it over the object, as the Cloud console and gcloud storage cp do.
 */
export const restoreGeneration = (bucket: string, name: string, generation: string) =>
  gcpFetch<StorageObject>(
    `${objectPath(bucket, name)}/copyTo/b/${enc(bucket)}/o/${enc(name)}?${new URLSearchParams({ sourceGeneration: generation })}`,
    { method: 'POST', ...json({}) },
  )

/** downloadUrl is an object's (or a generation's) content, for a link. */
export function downloadUrl(bucket: string, name: string, generation?: string) {
  const q = new URLSearchParams({ alt: 'media' })
  if (generation) q.set('generation', generation)
  return `/download${objectPath(bucket, name)}?${q}`
}

/**
 * uploadObject creates an object from a file with a media upload. It
 * sends no credentials, like every console request (FR-UI-004).
 */
export async function uploadObject(bucket: string, name: string, file: Blob) {
  const q = new URLSearchParams({ uploadType: 'media', name })
  const res = await send(
    new Request(new URL(`/upload${bucketPath(bucket)}/o?${q}`, origin()), {
      method: 'POST',
      body: file,
      headers: { 'Content-Type': file.type || 'application/octet-stream' },
      credentials: 'omit',
    }),
  )
  const text = await res.text()
  let body: unknown = text
  try {
    body = text ? JSON.parse(text) : undefined
  } catch {
    // Not JSON; toApiError shows the text.
  }
  if (!res.ok) throw toApiError(res.status, body)
  return body as StorageObject
}

// ---- notifications ----

export const notificationsQuery = (bucket: string) =>
  queryOptions({
    queryKey: ['gcs', 'notifications', bucket],
    queryFn: async () =>
      (await gcpFetch<{ items?: Notification[] }>(`${bucketPath(bucket)}/notificationConfigs`))
        .items ?? [],
  })

export const createNotification = (bucket: string, body: unknown) =>
  gcpFetch<Notification>(`${bucketPath(bucket)}/notificationConfigs`, {
    method: 'POST',
    ...json(body),
  })

export const deleteNotification = (bucket: string, id: string) =>
  gcpFetch<void>(`${bucketPath(bucket)}/notificationConfigs/${enc(id)}`, { method: 'DELETE' })

/** shortTopic drops the //pubsub.googleapis.com/ the API adds to a topic. */
export const shortTopic = (t: string) => t.replace(/^\/\/pubsub\.googleapis\.com\//, '')
