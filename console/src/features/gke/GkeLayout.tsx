import { useQuery } from '@tanstack/react-query'
import { Outlet, useParams } from 'react-router'

import { infoQuery } from '@/api/queries'
import { Card } from '@/components/ui/card'
import { useViewState } from '@/lib/viewState'

import { NotEnabled } from '../services/ServicePage'
import type { ClusterRef } from './api'

/**
 * GkeLayout gates the GKE view (SRS 4.8.3): the Service must be enabled,
 * and clusters belong to a Project, so one must be chosen.
 */
export function GkeLayout() {
  const { data: info } = useQuery(infoQuery())
  const [view] = useViewState()
  if (info && !info.services.includes('gke')) {
    return <NotEnabled id="gke" enabled={info.services} />
  }
  if (!view.project) {
    return (
      <div className="flex flex-col gap-6">
        <h1 className="text-2xl font-semibold">Google Kubernetes Engine</h1>
        <Card role="status">
          <p className="text-sm text-muted-foreground">
            Choose a Project with the Project switcher to see its clusters.
          </p>
        </Card>
      </div>
    )
  }
  return <Outlet />
}

/** useClusterRef is the cluster the route names, in the chosen Project. */
export function useClusterRef(): ClusterRef {
  const { location = '', cluster = '' } = useParams()
  const [view] = useViewState()
  return { project: view.project ?? '', location, cluster }
}

/** clusterPath is the console path of a cluster, or of a page under it. */
export const clusterPath = (location: string, cluster: string, sub = '') =>
  `/gke/locations/${location}/clusters/${cluster}${sub ? `/${sub}` : ''}`
