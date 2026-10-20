import { useQuery } from '@tanstack/react-query'

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

import { useViewState } from '@/lib/viewState'

import { hostOffersQuery } from './api'
import { VpcHeader } from './VpcLayout'

/**
 * OfferedPorts is the Offered ports of the Host attachment (ADR 0014), at which
 * VPC workloads reach listeners on the host's 127.0.0.1. They are the
 * Instance's, so every Project's show until one is chosen.
 */
export function OfferedPorts() {
  const [view] = useViewState()
  const query = useQuery(hostOffersQuery())
  const mine = view.project ? `projects/${view.project}/` : ''
  const shown = query.data?.filter((h) => h.subnetwork.startsWith(mine))
  return (
    <div className="flex flex-col gap-6">
      <VpcHeader />
      <Card>
        <h2 className="text-lg font-semibold">Offered ports</h2>
        <p className="text-sm text-muted-foreground">
          Workloads on a subnetwork reach each offered port at its Host address, which forwards to
          the same port on the host&apos;s 127.0.0.1.
        </p>
        <QueryStatus query={query} />
        {shown && shown.length === 0 && (
          <p className="text-sm text-muted-foreground">
            No ports are offered{mine && ' on this Project’s subnetworks'}. Offer one with{' '}
            <code className="font-mono">
              gcpemu offer &lt;port&gt; --subnetwork projects/P/regions/R/subnetworks/S
            </code>
            .
          </p>
        )}
        {shown && shown.length > 0 && (
          <Table aria-label="Offered ports">
            <TableHeader>
              <TableRow>
                <TableHead>Subnetwork</TableHead>
                <TableHead>Host address</TableHead>
                <TableHead>Ports</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((h) => (
                <TableRow key={h.subnetwork} data-testid="host-offers">
                  <TableCell className="font-mono break-all">
                    {mine ? h.subnetwork.slice(mine.length) : h.subnetwork}
                  </TableCell>
                  <TableCell className="font-mono">{h.hostAddress}</TableCell>
                  <TableCell className="font-mono">
                    {h.offers.map((o) => `${o.port}/${o.protocol}`).join(' ')}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  )
}
