import { useQuery } from '@tanstack/react-query'
import { useParams } from 'react-router'

import { infoQuery, resourceCountsQuery } from '@/api/queries'
import { Card } from '@/components/ui/card'
import { serviceInfo } from '@/lib/services'

import { CountList } from '../dashboard/ResourceCounts'

/** NotEnabled explains how to enable a Service a deep link points at. */
export function NotEnabled({ id, enabled }: { id: string; enabled: string[] }) {
  const services = [...enabled, id].join(',')
  return (
    <Card role="status">
      <h1 className="text-lg font-semibold">
        Service <code className="font-mono">{id}</code> is not enabled on this Instance
      </h1>
      <p className="text-sm text-muted-foreground">
        Restart the Instance with the Service in <code className="font-mono">--services</code>:
      </p>
      <pre className="overflow-x-auto rounded-md bg-muted p-3 font-mono text-sm">
        gcpemu start --services {services}
      </pre>
    </Card>
  )
}

/**
 * ServicePage is the landing page of one Service. Views for its resources
 * are added per Service; a deep link to a disabled Service says how to
 * enable it.
 */
export function ServicePage() {
  const { service = '' } = useParams()
  const { data: info } = useQuery(infoQuery())
  const counts = useQuery(resourceCountsQuery())
  const svc = serviceInfo(service)
  if (!svc) {
    return (
      <Card role="status">
        <h1 className="text-lg font-semibold">Page not found</h1>
        <p className="text-sm text-muted-foreground">
          There is no Service with ID <code className="font-mono">{service}</code>.
        </p>
      </Card>
    )
  }
  if (info && !info.services.includes(svc.id)) {
    return <NotEnabled id={svc.id} enabled={info.services} />
  }
  const mine = counts.data?.[svc.id]
  return (
    <div className="flex flex-col gap-6">
      <h1 className="text-2xl font-semibold">{svc.name}</h1>
      <Card>
        <p className="text-sm text-muted-foreground">
          Views for {svc.name} resources are not in the console yet.
        </p>
        {mine && <CountList counts={mine} />}
      </Card>
    </div>
  )
}
