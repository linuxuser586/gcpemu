import { queryOptions } from '@tanstack/react-query'

import { ApiError } from '@/api/errors'
import { gcpFetch } from '@/api/fetch'

// The Artifact Registry view's reads and writes, all through
// artifactregistry/v1 REST. Types cover the fields the view reads; the
// raw JSON editor covers the rest.

export type RepositoryMode = 'STANDARD_REPOSITORY' | 'REMOTE_REPOSITORY' | 'VIRTUAL_REPOSITORY'

export interface UpstreamPolicy {
  id?: string
  repository?: string
  priority?: number
}

export interface Repository {
  name: string
  format?: string
  mode?: RepositoryMode
  description?: string
  labels?: Record<string, string>
  createTime?: string
  updateTime?: string
  sizeBytes?: string
  registryUri?: string
  dockerConfig?: { immutableTags?: boolean }
  remoteRepositoryConfig?: {
    description?: string
    dockerRepository?: {
      publicRepository?: string
      customRepository?: { uri?: string }
    }
    commonRepository?: { uri?: string }
  }
  virtualRepositoryConfig?: { upstreamPolicies?: UpstreamPolicy[] }
  cleanupPolicyDryRun?: boolean
}

export interface Package {
  name: string
  createTime?: string
  updateTime?: string
}

/** ImageManifest is one platform's manifest of a multi-arch image. */
export interface ImageManifest {
  architecture?: string
  os?: string
  digest?: string
  mediaType?: string
  variant?: string
  osVersion?: string
  osFeatures?: string[]
}

export interface DockerImage {
  name: string
  uri: string
  tags?: string[]
  imageSizeBytes?: string
  uploadTime?: string
  mediaType?: string
  buildTime?: string
  updateTime?: string
  artifactType?: string
  imageManifests?: ImageManifest[]
}

export interface Tag {
  name: string
  version: string
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
  error?: { code?: number; message?: string }
  response?: unknown
}

/** RepoRef names a repository. */
export interface RepoRef {
  project: string
  location: string
  repo: string
}

const enc = encodeURIComponent
const API = '/artifactregistry/v1'

/** path encodes each segment of a resource name for a URL path. */
const path = (name: string) => name.split('/').map(enc).join('/')

export const locationName = (project: string, location: string) =>
  `projects/${project}/locations/${location}`

export const repoName = (r: RepoRef) =>
  `${locationName(r.project, r.location)}/repositories/${r.repo}`

/**
 * packageName is an image's package: its path with "/" escaped, as the
 * API names packages and docker images.
 */
export const packageName = (r: RepoRef, image: string) =>
  `${repoName(r)}/packages/${image.replaceAll('/', '%2F')}`

/** lastSegment is the ID at the end of a resource name. */
export const lastSegment = (name: string) => name.slice(name.lastIndexOf('/') + 1)

/** imageOf is the image path a package, tag or version name belongs to. */
export function imageOf(name: string): string {
  const m = /\/packages\/([^/]+)/.exec(name)
  return m ? decodeURIComponent(m[1]!) : ''
}

/** digestOf is the digest a version or docker image name ends with. */
export const digestOf = (name: string) =>
  name.slice(name.lastIndexOf(name.includes('@') ? '@' : '/') + 1)

/** REPO_ID is the API's repository ID rule. */
export const REPO_ID = /^[a-z]([a-z0-9-]*[a-z0-9])?$/
export const repoIdMessage = (id: string) =>
  `Invalid repository ID ${JSON.stringify(id)}: repository IDs must start with a lowercase letter, contain only lowercase letters, numbers and hyphens, and be at most 63 characters.`

/** TAG is the API's tag rule. */
export const TAG = /^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$/
export const tagMessage = (tag: string) => `Invalid tag ${JSON.stringify(tag)}.`

const json = (body: unknown) => ({ body: JSON.stringify(body) })

async function listAll<T>(url: string, field: string): Promise<T[]> {
  const out: T[] = []
  let pageToken = ''
  do {
    const q = new URLSearchParams()
    if (pageToken) q.set('pageToken', pageToken)
    const sep = url.includes('?') ? '&' : '?'
    const res = await gcpFetch<Record<string, unknown>>(`${url}${q.size ? sep + q.toString() : ''}`)
    out.push(...((res[field] as T[] | undefined) ?? []))
    pageToken = (res.nextPageToken as string | undefined) ?? ''
  } while (pageToken)
  return out
}

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

// ---- locations ----

export const locationsQuery = (project: string) =>
  queryOptions({
    queryKey: ['ar', 'locations', project],
    queryFn: () => listAll<Location>(`${API}/projects/${enc(project)}/locations`, 'locations'),
    staleTime: Infinity,
  })

// ---- repositories ----

export const listRepositories = (project: string, location: string) =>
  listAll<Repository>(
    `${API}/${path(locationName(project, location))}/repositories`,
    'repositories',
  )

/**
 * repositoriesQuery lists a Project's repositories in one location, or in
 * every location when location is empty: the API lists one location at
 * a time.
 */
export const repositoriesQuery = (project: string, location: string) =>
  queryOptions({
    queryKey: ['ar', 'repositories', project, location],
    queryFn: async ({ client }) => {
      if (location) return listRepositories(project, location)
      const locs = await client.ensureQueryData(locationsQuery(project))
      const all = await Promise.all(locs.map((l) => listRepositories(project, l.locationId)))
      return all.flat()
    },
  })

export const repositoryQuery = (r: RepoRef) =>
  queryOptions({
    queryKey: ['ar', 'repository', r],
    queryFn: () => gcpFetch<Repository>(`${API}/${path(repoName(r))}`),
  })

export const createRepository = async (
  project: string,
  location: string,
  id: string,
  body: unknown,
) =>
  waitOperation<Repository>(
    await gcpFetch<Operation>(
      `${API}/${path(locationName(project, location))}/repositories?${new URLSearchParams({ repositoryId: id })}`,
      { method: 'POST', ...json(body) },
    ),
  )

/** patchRepository sends repositories.patch with updateMask naming the fields to change. */
export const patchRepository = (r: RepoRef, body: unknown, mask: string[]) =>
  gcpFetch<Repository>(
    `${API}/${path(repoName(r))}?${new URLSearchParams({ updateMask: mask.join(',') })}`,
    { method: 'PATCH', ...json(body) },
  )

export const deleteRepository = async (r: RepoRef) =>
  waitOperation(await gcpFetch<Operation>(`${API}/${path(repoName(r))}`, { method: 'DELETE' }))

// ---- packages (images) ----

export const packagesQuery = (r: RepoRef) =>
  queryOptions({
    queryKey: ['ar', 'packages', r],
    queryFn: () => listAll<Package>(`${API}/${path(repoName(r))}/packages`, 'packages'),
  })

export const deletePackage = async (r: RepoRef, image: string) =>
  waitOperation(
    await gcpFetch<Operation>(`${API}/${path(packageName(r, image))}`, { method: 'DELETE' }),
  )

// ---- docker images (digests) ----

/** dockerImagesQuery lists every manifest of a repository, newest first. */
export const dockerImagesQuery = (r: RepoRef) =>
  queryOptions({
    queryKey: ['ar', 'dockerImages', r],
    queryFn: () =>
      listAll<DockerImage>(
        `${API}/${path(repoName(r))}/dockerImages?${new URLSearchParams({ orderBy: 'upload_time desc' })}`,
        'dockerImages',
      ),
  })

/** deleteVersion deletes a digest; force also deletes the tags on it. */
export const deleteVersion = async (r: RepoRef, image: string, digest: string) =>
  waitOperation(
    await gcpFetch<Operation>(
      `${API}/${path(`${packageName(r, image)}/versions/${digest}`)}?force=true`,
      { method: 'DELETE' },
    ),
  )

// ---- tags ----

export const versionName = (r: RepoRef, image: string, digest: string) =>
  `${packageName(r, image)}/versions/${digest}`

export const createTag = (r: RepoRef, image: string, tag: string, digest: string) =>
  gcpFetch<Tag>(
    `${API}/${path(packageName(r, image))}/tags?${new URLSearchParams({ tagId: tag })}`,
    {
      method: 'POST',
      ...json({ version: versionName(r, image, digest) }),
    },
  )

/** moveTag points an existing tag at another digest. */
export const moveTag = (r: RepoRef, image: string, tag: string, digest: string) => {
  const name = `${packageName(r, image)}/tags/${tag}`
  return gcpFetch<Tag>(`${API}/${path(name)}?updateMask=version`, {
    method: 'PATCH',
    ...json({ name, version: versionName(r, image, digest) }),
  })
}

export const deleteTag = (r: RepoRef, image: string, tag: string) =>
  gcpFetch<void>(`${API}/${path(`${packageName(r, image)}/tags/${tag}`)}`, { method: 'DELETE' })

// ---- registry ----

/**
 * registryHost is where docker reaches a repository's location: the real
 * LOCATION-docker.pkg.dev in host mode, otherwise the registry port with
 * the real host as the first path component (both are what the registry
 * accepts, see services/ar).
 */
export function registryHost(location: string, registry: string | undefined, hostMode: boolean) {
  const host = `${location}-docker.pkg.dev`
  return hostMode || !registry ? host : `${registry}/${host}`
}

/** imageRef is image's reference at a tag or digest ("sha256:..."). */
export function imageRef(host: string, r: RepoRef, image: string, ref: string) {
  return `${host}/${r.project}/${r.repo}/${image}${ref.startsWith('sha256:') ? '@' : ':'}${ref}`
}

/** platform names an image manifest's platform, e.g. linux/arm64/v8. */
export const platform = (m: ImageManifest) =>
  [m.os, m.architecture, m.variant].filter(Boolean).join('/') || 'unknown'

/** shortDigest is a digest's first 12 hex characters. */
export const shortDigest = (d: string) => d.replace(/^sha256:/, '').slice(0, 12)
