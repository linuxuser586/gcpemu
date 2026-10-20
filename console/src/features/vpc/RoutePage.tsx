import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Trash2 } from 'lucide-react'
import { useParams } from 'react-router'

import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { CopyAs } from '@/components/resource/CopyAs'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import { toast } from '@/lib/toast'
import { useViewState } from '@/lib/viewState'

import { deleteRoute, priorityOf, routeQuery } from './api'
import { routeRecipe } from './copy'
import { NetworkLink } from './links'
import { RecordedNotice } from './RecordedNotice'
import { isSystemRoute, nextHop } from './Routes'

/**
 * RoutePage is one route and the route as the API returns it. Routes are
 * immutable, so there is no edit; a system-generated one cannot be
 * deleted here either.
 */
export function RoutePage() {
  const { route: name = '' } = useParams()
  const [view] = useViewState()
  const project = view.project ?? ''
  const query = useQuery(routeQuery(project, name))
  const r = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const remove = useMutation({
    mutationFn: () => deleteRoute(project, name),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(`Deleted route ${name}.`)
      navigate('/compute/routes')
    },
  })
  const system = !!r && isSystemRoute(r)
  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to="/compute/routes" className="underline-offset-4 hover:underline">
              VPC network
            </Link>{' '}
            / route
          </p>
          <h1 className="text-2xl font-semibold">
            <span className="font-mono">{name}</span>
          </h1>
        </div>
        {r && (
          <div className="flex flex-wrap gap-2">
            <CopyAs what="route" recipe={() => routeRecipe(r, project)} />
            {!system && (
              <ConfirmDialog
                trigger={
                  <Button variant="outline">
                    <Trash2 aria-hidden />
                    Delete route
                  </Button>
                }
                title={`Delete route ${name}?`}
                description="Routes cannot be changed: to change one, delete it and create it again."
                confirmLabel="Delete route"
                onConfirm={() => remove.mutateAsync()}
              />
            )}
          </div>
        )}
      </div>
      <RecordedNotice />
      <QueryStatus query={query} />
      {system && (
        <p className="text-sm text-muted-foreground">
          System-generated: made by GCP with the network, and deleted with it. The console shows it
          read-only.
        </p>
      )}
      {r && (
        <>
          <Card aria-labelledby="route-title">
            <CardTitle id="route-title">Route</CardTitle>
            <DetailList
              label="Route details"
              rows={[
                ['Network', <NetworkLink key="n" link={r.network} />],
                ['Destination range', <Mono key="d">{r.destRange}</Mono>],
                ['Priority', <Mono key="p">{priorityOf(r)}</Mono>],
                ['Next hop', nextHop(r)],
                ['Network tags', r.tags?.length ? <Mono key="t">{r.tags.join(', ')}</Mono> : 'All'],
                ['Route type', r.routeType && <Mono key="y">{r.routeType}</Mono>],
                ['Description', r.description],
                ['Created', r.creationTimestamp && <Mono key="c">{r.creationTimestamp}</Mono>],
              ]}
            />
          </Card>
          <Card>
            <CardTitle>Resource</CardTitle>
            <JsonView value={r} label="Route JSON" />
          </Card>
        </>
      )}
    </div>
  )
}
