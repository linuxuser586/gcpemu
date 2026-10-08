import { useQuery } from '@tanstack/react-query'

import { infoQuery } from '@/api/queries'
import { QueryStatus } from '@/components/QueryStatus'
import { Card } from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'

import { negOwner, negsQuery } from './api'
import { useCluster } from './ClusterPage'
import { useClusterRef } from './GkeLayout'

const lastSeg = (s?: string) => s?.split('/').pop() ?? ''

/**
 * Negs lists the standalone zonal NEGs the cluster's NEG controller made
 * for Kubernetes Services annotated cloud.google.com/neg (FR-GKE-008).
 * They are compute resources, so they need the compute Service.
 */
export function Negs() {
  const ref = useClusterRef()
  const c = useCluster()
  const { data: info } = useQuery(infoQuery())
  const computeOn = info?.services.includes('compute') ?? false
  const query = useQuery(negsQuery(ref.project, computeOn))
  if (info && !computeOn) {
    return (
      <Card role="status">
        <p className="text-sm text-muted-foreground">
          NEGs are compute resources: start the Instance with the{' '}
          <code className="font-mono">compute</code> Service to see them.
        </p>
      </Card>
    )
  }
  const negs = (query.data ?? [])
    .map((n) => ({ neg: n, owner: negOwner(n) }))
    .filter((x) => x.owner?.clusterUid === c.id)
    .sort(
      (a, b) =>
        a.neg.name.localeCompare(b.neg.name) || (a.neg.zone ?? '').localeCompare(b.neg.zone ?? ''),
    )
  return (
    <Card>
      <QueryStatus query={query} />
      {query.data && negs.length === 0 && (
        <p className="text-sm text-muted-foreground">
          No NEGs. Annotate a Kubernetes Service with{' '}
          <code className="font-mono">cloud.google.com/neg: {'{"exposed_ports": {"80": {}}}'}</code>{' '}
          to get one per zone.
        </p>
      )}
      {negs.length > 0 && (
        <Table aria-label="NEGs">
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Zone</TableHead>
              <TableHead>Kubernetes Service</TableHead>
              <TableHead>Port</TableHead>
              <TableHead>Type</TableHead>
              <TableHead className="text-right">Endpoints</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {negs.map(({ neg, owner }) => (
              <TableRow key={neg.selfLink ?? `${neg.zone}/${neg.name}`} data-testid="neg">
                <TableCell className="font-mono break-all">{neg.name}</TableCell>
                <TableCell className="font-mono">{lastSeg(neg.zone)}</TableCell>
                <TableCell className="font-mono">
                  {owner!.namespace}/{owner!.service}
                </TableCell>
                <TableCell className="font-mono">{owner!.port}</TableCell>
                <TableCell className="font-mono">{neg.networkEndpointType}</TableCell>
                <TableCell className="text-right tabular-nums">{neg.size ?? 0}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}
