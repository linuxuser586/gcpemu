import { useQuery } from '@tanstack/react-query'
import { Plus } from 'lucide-react'

import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Button } from '@/components/ui/button'
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

import { lastSeg, relPath } from '../lb/api'
import { networksQuery, subnetsQuery, type Network } from './api'
import { networkPage, SubnetLink } from './links'
import { VpcHeader } from './VpcLayout'

/** subnetMode describes how a network gets its subnetworks. */
export const subnetMode = (n: Network) => (n.autoCreateSubnetworks ? 'Auto' : 'Custom')

/** routingMode is a network's dynamic routing mode, REGIONAL by default. */
export const routingMode = (n: Network) => n.routingConfig?.routingMode ?? 'REGIONAL'

/** Networks is the chosen Project's VPC networks, each with its subnetworks. */
export function Networks() {
  const [view] = useViewState()
  const project = view.project ?? ''
  const networks = useQuery(networksQuery(project))
  const subnets = useQuery(subnetsQuery(project))
  const all = [...(networks.data ?? [])].sort((a, b) => a.name.localeCompare(b.name))
  return (
    <div className="flex flex-col gap-6">
      <VpcHeader
        action={
          <Button asChild>
            <Link to="/compute/create-network">
              <Plus aria-hidden />
              Create network
            </Link>
          </Button>
        }
      />
      <Card>
        <QueryStatus query={networks} />
        {networks.data && all.length === 0 && (
          <p className="text-sm text-muted-foreground">
            No VPC networks in this Project yet: create one, then add subnetworks to it for GKE
            clusters, Cloud SQL and load balancers.
          </p>
        )}
        {all.length > 0 && (
          <Table aria-label="Networks">
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Subnet mode</TableHead>
                <TableHead>Subnetworks</TableHead>
                <TableHead className="text-right">MTU</TableHead>
                <TableHead>Routing mode</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {all.map((n) => {
                const mine = (subnets.data ?? [])
                  .filter((s) => relPath(s.network) === relPath(n.selfLink))
                  .sort((a, b) => a.name.localeCompare(b.name))
                return (
                  <TableRow key={n.name} data-testid="network">
                    <TableCell>
                      <Link
                        className="font-mono text-primary underline-offset-4 hover:underline"
                        to={networkPage(n.name)}
                      >
                        {n.name}
                      </Link>
                    </TableCell>
                    <TableCell>{subnetMode(n)}</TableCell>
                    <TableCell>
                      {n.autoCreateSubnetworks && mine.length > 3 ? (
                        <span className="text-muted-foreground">{mine.length} regions</span>
                      ) : (
                        <span className="flex flex-wrap gap-x-2">
                          {mine.map((s) => (
                            <SubnetLink key={s.selfLink} region={lastSeg(s.region)} name={s.name} />
                          ))}
                          {subnets.data && mine.length === 0 && (
                            <span className="text-muted-foreground">None</span>
                          )}
                        </span>
                      )}
                    </TableCell>
                    <TableCell className="text-right font-mono">{n.mtu ?? 1460}</TableCell>
                    <TableCell className="font-mono text-xs">{routingMode(n)}</TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  )
}
