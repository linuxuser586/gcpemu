import { queryOptions } from '@tanstack/react-query'

import { admin, unwrap, type Readiness } from './admin'
import { toApiError } from './errors'
import { gcpFetch } from './fetch'

// Server state lives only in TanStack Query: every read the console makes
// is one of these query option factories. The dashboard polls until the
// /_emu/v1/events stream replaces polling.
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
    refetchInterval: POLL_MS,
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
    queryKey: ['cloudresourcemanager', 'v3', 'projects:search'],
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
