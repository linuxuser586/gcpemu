import { queryOptions, useQuery } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { Outlet, useParams } from 'react-router'

import { admin, unwrap } from '@/api/admin'
import { endpointsQuery, infoQuery } from '@/api/queries'
import { Card } from '@/components/ui/card'
import { useViewState } from '@/lib/viewState'

import { NotEnabled } from '../services/ServicePage'
import { registryHost, type RepoRef } from './api'

/**
 * ArLayout gates the Artifact Registry view (SRS 4.8.3) on the Service
 * being enabled.
 */
export function ArLayout() {
  const { data: info } = useQuery(infoQuery())
  if (info && !info.services.includes('ar')) {
    return <NotEnabled id="ar" enabled={info.services} />
  }
  return <Outlet />
}

/** ChooseProject asks for a Project before anything else. */
export function ChooseProject({ what }: { what: ReactNode }) {
  return (
    <div className="flex flex-col gap-6">
      <h1 className="text-2xl font-semibold">Artifact Registry</h1>
      <Card role="status">
        <p className="text-sm text-muted-foreground">
          Choose a Project with the Project switcher to {what}.
        </p>
      </Card>
    </div>
  )
}

/**
 * useRepoRef is the repository the route names, with the Project from
 * ?project=.
 */
export function useRepoRef(): RepoRef {
  const [view] = useViewState()
  const { location = '', repo = '' } = useParams()
  return { project: view.project ?? '', location, repo }
}

/** useImage is the image path the route names (its slashes kept). */
export const useImage = () => useParams().image ?? ''

/** useDigest is the digest the route names. */
export const useDigest = () => useParams().digest ?? ''

const enc = encodeURIComponent

/** repoPath is the console path of a repository, or of a page under it. */
export const repoPath = (r: Pick<RepoRef, 'location' | 'repo'>, sub = '') =>
  `/ar/locations/${enc(r.location)}/repositories/${enc(r.repo)}${sub ? `/${sub}` : ''}`

/** imagePath is the console path of an image, or of a digest of it. */
export const imagePath = (r: Pick<RepoRef, 'location' | 'repo'>, image: string, digest = '') =>
  repoPath(r, `images/${enc(image)}${digest ? `/${enc(digest)}` : ''}`)

/** hostModeQuery is host mode's state, which decides how pull commands name a registry. */
export const hostModeQuery = () =>
  queryOptions({
    queryKey: ['admin', 'hostmode'],
    queryFn: () => unwrap(admin.GET('/_emu/v1/hostmode')),
    staleTime: 30_000,
  })

/**
 * useRegistryHost is the host docker pulls a location's images from: the
 * real LOCATION-docker.pkg.dev when host mode is ready, otherwise the
 * Instance's registry port.
 */
export function useRegistryHost(location: string) {
  const endpoints = useQuery(endpointsQuery())
  const hostMode = useQuery(hostModeQuery())
  return registryHost(location, endpoints.data?.ar, hostMode.data?.state === 'ready')
}
