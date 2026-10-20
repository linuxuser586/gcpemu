import { queryOptions } from '@tanstack/react-query'

import { admin, unwrap } from '@/api/admin'
import { ApiError, shouldRetry } from '@/api/errors'
import { gcpFetch, gcpText } from '@/api/fetch'
import { POLL_MS } from '@/api/queries'

// The Cloud Build view's reads and actions: Builds and Build triggers
// through cloudbuild/v1 REST, as a client would call them, and each
// Build's log and the Build daemon's cache through the build Service's
// Admin API. Types cover the fields the view reads; the JSON view covers
// the rest.

export type BuildStatus =
  | 'STATUS_UNKNOWN'
  | 'PENDING'
  | 'QUEUED'
  | 'WORKING'
  | 'SUCCESS'
  | 'FAILURE'
  | 'INTERNAL_ERROR'
  | 'TIMEOUT'
  | 'CANCELLED'
  | 'EXPIRED'

export interface TimeSpan {
  startTime?: string
  endTime?: string
}

export interface BuildStep {
  name: string
  id?: string
  status?: BuildStatus
  timing?: TimeSpan
}

export interface Source {
  storageSource?: { bucket?: string; object?: string; generation?: string }
  gitSource?: { url?: string; revision?: string; dir?: string }
  repoSource?: {
    repoName?: string
    branchName?: string
    tagName?: string
    commitSha?: string
  }
  storageSourceManifest?: { bucket?: string; object?: string }
  connectedRepository?: { repository?: string; revision?: string }
}

export interface Build {
  name: string
  id: string
  projectId?: string
  status?: BuildStatus
  statusDetail?: string
  failureInfo?: { type?: string; detail?: string }
  source?: Source
  steps?: BuildStep[]
  createTime?: string
  startTime?: string
  finishTime?: string
  buildTriggerId?: string
  serviceAccount?: string
  logsBucket?: string
  images?: string[]
}

export interface BuildTrigger {
  id: string
  name?: string
  description?: string
  disabled?: boolean
  createTime?: string
  pubsubConfig?: { topic?: string }
  webhookConfig?: object
  github?: { owner?: string; name?: string }
  triggerTemplate?: { repoName?: string }
  repositoryEventConfig?: { repository?: string }
  sourceToBuild?: { uri?: string; ref?: string }
}

/** Operation is the long-running Operation retry and run answer with. */
export interface Operation {
  name: string
  metadata?: { build?: Build }
}

/** BuildRef names a Build, as its resource name and logUrl do. */
export interface BuildRef {
  project: string
  location: string
  id: string
}

const enc = encodeURIComponent
const API = '/cloudbuild/v1'

/** GLOBAL is where Builds made through projects/*\/builds run. */
export const GLOBAL = 'global'

export const parent = (project: string, location: string) =>
  `projects/${enc(project)}/locations/${enc(location)}`

export const buildName = (r: BuildRef) => `${parent(r.project, r.location)}/builds/${enc(r.id)}`

/** buildRef reads a Build's resource name. */
export function buildRef(b: Build): BuildRef {
  const m = /^projects\/([^/]+)\/locations\/([^/]+)\/builds\/([^/]+)$/.exec(b.name)
  return m
    ? { project: m[1]!, location: m[2]!, id: m[3]! }
    : { project: b.projectId ?? '', location: GLOBAL, id: b.id }
}

// ---- status, timing and source ----

const ACTIVE: ReadonlySet<BuildStatus | undefined> = new Set(['PENDING', 'QUEUED', 'WORKING'])

/** isActive reports whether a Build (or step) may still change status. */
export const isActive = (s?: BuildStatus) => ACTIVE.has(s)

/** STATUS_BADGE is the badge variant of each Build or step status. */
export const STATUS_BADGE: Record<BuildStatus, 'default' | 'success' | 'warning' | 'destructive'> =
  {
    STATUS_UNKNOWN: 'default',
    PENDING: 'default',
    QUEUED: 'default',
    WORKING: 'warning',
    SUCCESS: 'success',
    FAILURE: 'destructive',
    INTERNAL_ERROR: 'destructive',
    TIMEOUT: 'destructive',
    CANCELLED: 'default',
    EXPIRED: 'default',
  }

/**
 * duration renders the time between start and end, or between start and
 * now while it runs ("1m 05s", "12s"); empty before it starts.
 */
export function duration(start?: string, end?: string, now = Date.now()): string {
  if (!start) return ''
  const ms = (end ? Date.parse(end) : now) - Date.parse(start)
  if (!Number.isFinite(ms) || ms < 0) return ''
  const s = Math.round(ms / 1000)
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ${String(s % 60).padStart(2, '0')}s`
  return `${Math.floor(m / 60)}h ${String(m % 60).padStart(2, '0')}m`
}

/** sourceLabel summarizes where a Build's source came from. */
export function sourceLabel(src?: Source): string {
  if (src?.storageSource) return `gs://${src.storageSource.bucket}/${src.storageSource.object}`
  if (src?.storageSourceManifest)
    return `gs://${src.storageSourceManifest.bucket}/${src.storageSourceManifest.object} (manifest)`
  if (src?.gitSource)
    return `${src.gitSource.url}${src.gitSource.revision ? `@${src.gitSource.revision}` : ''}`
  if (src?.repoSource) {
    const r = src.repoSource
    const rev = r.branchName ?? r.tagName ?? r.commitSha
    return `${r.repoName ?? ''}${rev ? `@${rev}` : ''}`
  }
  if (src?.connectedRepository) return src.connectedRepository.repository ?? ''
  return 'None'
}

/** accountLabel is a Build's service account by email. */
export const accountLabel = (sa?: string) => (sa ? sa.slice(sa.lastIndexOf('/') + 1) : '')

/** triggerKind names what starts a Build trigger. */
export function triggerKind(t: BuildTrigger): string {
  if (t.pubsubConfig) return 'Pub/Sub'
  if (t.webhookConfig) return 'Webhook'
  if (t.github) return 'GitHub'
  if (t.repositoryEventConfig) return 'Repository event'
  if (t.triggerTemplate) return 'Cloud Source Repositories'
  return 'Manual'
}

// ---- logs ----

/** stepLabel names step i in the log as Cloud Build does. */
const stepLabel = (i: number, id?: string) =>
  id ? `Step #${i} - ${JSON.stringify(id)}` : `Step #${i}`

/**
 * stepLog is step i's output in a Build log: the lines after its label,
 * without it.
 */
export function stepLog(log: string, i: number, id?: string): string {
  const prefix = `${stepLabel(i, id)}: `
  return log
    .split('\n')
    .filter((l) => l.startsWith(prefix))
    .map((l) => l.slice(prefix.length))
    .join('\n')
}

/** LOG_POLL_MS is how often a running Build's log is read again. */
export const LOG_POLL_MS = 1000

/**
 * logObject is where a Build's log object is: log-ID.txt in its
 * logsBucket ("gs://B" or "gs://B/some/dir").
 */
export function logObject(b: Build): { bucket: string; object: string } | undefined {
  if (!b.logsBucket) return undefined
  const [bucket = '', ...dir] = b.logsBucket.replace(/^gs:\/\//, '').split('/')
  const prefix = dir.join('/').replace(/^\/+|\/+$/g, '')
  return { bucket, object: `${prefix ? `${prefix}/` : ''}log-${b.id}.txt` }
}

/**
 * readLog is a Build's log so far: the copy the build Service keeps in
 * memory, or, for a finished Build whose copy went with a restart or
 * Reset, its log object in Cloud Storage.
 */
async function readLog(b: Build): Promise<string> {
  const r = buildRef(b)
  const log = await unwrap<string>(
    admin.GET('/_emu/v1/build/projects/{project}/locations/{location}/builds/{id}/log', {
      params: { path: r },
      parseAs: 'text',
    }),
  )
  const obj = logObject(b)
  if (log || isActive(b.status) || !obj) return log
  try {
    return await gcpText(
      `/storage/v1/b/${enc(obj.bucket)}/o/${enc(obj.object)}?${new URLSearchParams({ alt: 'media' })}`,
    )
  } catch {
    // No log object (no Cloud Storage, or none written): the log is empty.
    return ''
  }
}

/**
 * logQuery is a Build's log so far. It is read again while the Build
 * runs, since log lines are not resource changes; its final status
 * reads it once more through the event bridge.
 */
export const logQuery = (b: Build) =>
  queryOptions({
    queryKey: ['build', 'log', buildRef(b)],
    queryFn: () => readLog(b),
    refetchInterval: isActive(b.status) ? LOG_POLL_MS : false,
  })

// ---- builds ----

export async function listBuilds(project: string, location: string) {
  const out: Build[] = []
  let pageToken = ''
  do {
    const q = new URLSearchParams()
    if (pageToken) q.set('pageToken', pageToken)
    const res = await gcpFetch<{ builds?: Build[]; nextPageToken?: string }>(
      `${API}/${parent(project, location)}/builds?${q}`,
    )
    out.push(...(res.builds ?? []))
    pageToken = res.nextPageToken ?? ''
  } while (pageToken)
  return out
}

export const buildsQuery = (project: string, location: string) =>
  queryOptions({
    queryKey: ['build', 'builds', project, location],
    queryFn: () => listBuilds(project, location),
    // Build events keep it fresh; durations of running Builds tick.
    refetchInterval: (q) => (q.state.data?.some((b) => isActive(b.status)) ? POLL_MS : false),
  })

export const buildQuery = (r: BuildRef) =>
  queryOptions({
    queryKey: ['build', 'build', r],
    queryFn: () => gcpFetch<Build>(`${API}/${buildName(r)}`),
  })

const json = (body: unknown) => ({ method: 'POST', body: JSON.stringify(body) })

export const cancelBuild = (r: BuildRef) =>
  gcpFetch<Build>(`${API}/${buildName(r)}:cancel`, json({}))

/** retryBuild starts a new Build from the one named; its Operation carries it. */
export const retryBuild = (r: BuildRef) =>
  gcpFetch<Operation>(`${API}/${buildName(r)}:retry`, json({}))

// ---- triggers ----

export async function listTriggers(project: string, location: string) {
  const out: BuildTrigger[] = []
  let pageToken = ''
  do {
    // project_id is required alongside the parent, as the API's
    // field behaviour has it.
    const q = new URLSearchParams({ projectId: project })
    if (pageToken) q.set('pageToken', pageToken)
    const res = await gcpFetch<{ triggers?: BuildTrigger[]; nextPageToken?: string }>(
      `${API}/${parent(project, location)}/triggers?${q}`,
    )
    out.push(...(res.triggers ?? []))
    pageToken = res.nextPageToken ?? ''
  } while (pageToken)
  return out
}

/** isUnimplemented reports an answer from an Instance that lacks the call. */
export const isUnimplemented = (e: unknown) => e instanceof ApiError && e.status === 'UNIMPLEMENTED'

export const triggersQuery = (project: string, location: string) =>
  queryOptions({
    queryKey: ['build', 'triggers', project, location],
    queryFn: () => listTriggers(project, location),
    // An Instance without Build triggers answers the same every time.
    retry: (n, e) => !isUnimplemented(e) && shouldRetry(n, e),
  })

/** runTrigger runs a Build trigger on its own source; its Operation carries the Build. */
export const runTrigger = (project: string, location: string, id: string) =>
  gcpFetch<Operation>(
    `${API}/${parent(project, location)}/triggers/${enc(id)}:run`,
    json({ projectId: project, triggerId: id }),
  )

// ---- the Build daemon ----

export const daemonQuery = () =>
  queryOptions({
    queryKey: ['build', 'daemon'],
    queryFn: () => unwrap(admin.GET('/_emu/v1/build/daemon')),
    refetchInterval: POLL_MS,
  })

export const pruneDaemon = () => unwrap(admin.POST('/_emu/v1/build/daemon/prune'))
