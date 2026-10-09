import { useQuery } from '@tanstack/react-query'
import { Outlet, useParams } from 'react-router'

import { infoQuery } from '@/api/queries'
import { Card } from '@/components/ui/card'
import { useViewState } from '@/lib/viewState'

import { NotEnabled } from '../services/ServicePage'
import { StatusBadge } from '../gke/format'
import { displayState, type DatabaseInstance, type InstanceRef } from './api'

/**
 * SqlLayout gates the Cloud SQL view (SRS 4.8.3): the Service must be
 * enabled, and instances belong to a Project, so one must be chosen.
 */
export function SqlLayout() {
  const { data: info } = useQuery(infoQuery())
  const [view] = useViewState()
  if (info && !info.services.includes('sql')) {
    return <NotEnabled id="sql" enabled={info.services} />
  }
  if (!view.project) {
    return (
      <div className="flex flex-col gap-6">
        <h1 className="text-2xl font-semibold">Cloud SQL</h1>
        <Card role="status">
          <p className="text-sm text-muted-foreground">
            Choose a Project with the Project switcher to see its instances.
          </p>
        </Card>
      </div>
    )
  }
  return <Outlet />
}

/** useInstanceRef is the instance the route names, in the chosen Project. */
export function useInstanceRef(): InstanceRef {
  const { instance = '' } = useParams()
  const [view] = useViewState()
  return { project: view.project ?? '', instance }
}

/** instancePath is the console path of an instance, or of a page under it. */
export const instancePath = (instance: string, sub = '') =>
  `/sql/instances/${encodeURIComponent(instance)}${sub ? `/${sub}` : ''}`

const TONES: Record<string, 'success' | 'warning' | 'destructive' | 'default'> = {
  RUNNABLE: 'success',
  PENDING_CREATE: 'warning',
  PENDING_DELETE: 'warning',
  MAINTENANCE: 'warning',
  FAILED: 'destructive',
  SUSPENDED: 'destructive',
  STOPPED: 'default',
}

/** InstanceStateBadge shows an instance's state; a stopped one is STOPPED. */
export function InstanceStateBadge({ instance }: { instance: DatabaseInstance }) {
  const s = displayState(instance)
  return <StatusBadge status={s} tone={TONES[s] ?? 'default'} />
}
