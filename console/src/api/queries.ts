import { queryOptions } from '@tanstack/react-query'

import { admin, unwrap, type Operation, type Readiness, type RequestEntry } from './admin'
import { toApiError } from './errors'
import { gcpFetch } from './fetch'

// Server state lives only in TanStack Query: every read the console makes
// is one of these query option factories. Queries over state the
// /_emu/v1/events stream reports (resources, Operations, the Request log)
// are kept fresh by the event bridge (events.ts) and do not poll; the rest
// (readiness, runtime, containers) poll.
//
// Queries of a Service's own API are keyed by Service ID first (["gcs",
// ...]) so that the bridge can invalidate them by the Service its events
// name.
export const POLL_MS = 3000

export const infoQuery = () =>
  queryOptions({
    queryKey: ['admin', 'info'],
    queryFn: () => unwrap(admin.GET('/_emu/v1/info')),
    refetchInterval: POLL_MS,
  })

export const readinessQuery = () =>
  queryOptions({
    queryKey: ['admin', 'ready'],
    // 503 is an answer, not a failure: it carries each service's reason.
    queryFn: async (): Promise<Readiness> => {
      const { data, error, response } = await admin.GET('/_emu/v1/ready')
      if (data) return data
      if (response.status === 503 && error) return error
      throw toApiError(response.status, error)
    },
    refetchInterval: POLL_MS,
  })

export const endpointsQuery = () =>
  queryOptions({
    queryKey: ['admin', 'endpoints'],
    queryFn: () => unwrap(admin.GET('/_emu/v1/endpoints')),
    refetchInterval: POLL_MS,
  })

export const containersQuery = () =>
  queryOptions({
    queryKey: ['admin', 'containers'],
    queryFn: async () => (await unwrap(admin.GET('/_emu/v1/containers'))).containers,
    refetchInterval: POLL_MS,
  })

export const resourceCountsQuery = () =>
  queryOptions({
    queryKey: ['admin', 'resources', 'counts'],
    queryFn: () => unwrap(admin.GET('/_emu/v1/resources/counts')),
  })

/**
 * operationsQuery lists every Service's Operations, newest first, or one
 * Project's. Operation events keep it fresh; while an Operation is
 * running it also polls, since a Cloud DNS change completes without a
 * write to report.
 */
export const operationsQuery = (project = '') =>
  queryOptions({
    queryKey: ['admin', 'operations', project],
    queryFn: async (): Promise<Operation[]> =>
      (
        await unwrap(
          admin.GET('/_emu/v1/operations', { params: { query: project ? { project } : {} } }),
        )
      ).operations,
    refetchInterval: (q) => (q.state.data?.some((op) => !op.done) ? POLL_MS : false),
  })

/** requestsQuery is the Request log, oldest first; the bridge appends to it. */
export const requestsQuery = (service = '') =>
  queryOptions({
    queryKey: ['admin', 'requests', service],
    queryFn: async (): Promise<RequestEntry[]> =>
      (
        await unwrap(
          admin.GET('/_emu/v1/requests', { params: { query: service ? { service } : {} } }),
        )
      ).requests ?? [],
  })

export const envQuery = () =>
  queryOptions({
    queryKey: ['admin', 'env'],
    queryFn: () => unwrap(admin.GET('/_emu/v1/env', { params: { query: {} } })),
    refetchInterval: POLL_MS,
  })

export interface Project {
  projectId: string
  name: string
  displayName?: string
  state?: string
}

/** projectsQuery lists Projects through Resource Manager (the iam Service). */
export const projectsQuery = () =>
  queryOptions({
    queryKey: ['iam', 'projects'],
    queryFn: async () => {
      const out: Project[] = []
      let pageToken = ''
      do {
        const q = pageToken ? `?pageToken=${encodeURIComponent(pageToken)}` : ''
        const page = await gcpFetch<{ projects?: Project[]; nextPageToken?: string }>(
          `/cloudresourcemanager/v3/projects:search${q}`,
        )
        out.push(...(page.projects ?? []))
        pageToken = page.nextPageToken ?? ''
      } while (pageToken)
      return out
    },
  })
