import { useQuery } from '@tanstack/react-query'
import { Outlet, useParams } from 'react-router'

import { infoQuery } from '@/api/queries'
import { Card } from '@/components/ui/card'
import { useViewState } from '@/lib/viewState'

import type { Ref } from '../lb/api'
import { NotEnabled } from '../services/ServicePage'
import type { OriginColl } from './api'

/**
 * CdnLayout gates the Cloud CDN view (SRS 4.8.3): the Service must be
 * enabled, and lb too, which serves the backends CDN caches in front of;
 * origins belong to a Project, so one must be chosen.
 */
export function CdnLayout() {
  const { data: info } = useQuery(infoQuery())
  const [view] = useViewState()
  if (info && !info.services.includes('cdn')) {
    return <NotEnabled id="cdn" enabled={info.services} />
  }
  if (info && !info.services.includes('lb')) {
    return <NotEnabled id="lb" enabled={info.services} />
  }
  if (!view.project) {
    return (
      <div className="flex flex-col gap-6">
        <h1 className="text-2xl font-semibold">Cloud CDN</h1>
        <Card role="status">
          <p className="text-sm text-muted-foreground">
            Choose a Project with the Project switcher to see its Cloud CDN origins.
          </p>
        </Card>
      </div>
    )
  }
  return <Outlet />
}

/** OriginRef names an origin: a backend service or bucket. */
export type OriginRef = Ref & { coll: OriginColl }

/** originPath is the console path of an origin, or of a page under it. */
export const originPath = (r: Ref, sub = '') =>
  `/cdn/${r.region ? `regions/${r.region}` : 'global'}/${r.coll}/${encodeURIComponent(r.name)}${sub ? `/${sub}` : ''}`

/** useOriginRef is the origin the route names, in the chosen Project. */
export function useOriginRef(): OriginRef {
  const { region = '', coll = '', name = '' } = useParams()
  const [view] = useViewState()
  return { project: view.project ?? '', region, coll: coll as OriginColl, name }
}
