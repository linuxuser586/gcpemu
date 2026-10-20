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

import { lastSeg } from '../lb/api'
import { byPriority, priorityOf, routesQuery, type Route } from './api'
import { NetworkLink, RouteLink } from './links'
import { RecordedNotice } from './RecordedNotice'
import { VpcHeader } from './VpcLayout'

/**
 * isSystemRoute is a route GCP makes and manages itself: the default
 * route to the internet a network gets when it is created (the emulator
 * names it as GCP does), and subnet and dynamic routes. The console
 * shows them read-only.
 */
export const isSystemRoute = (r: Route) =>
  r.routeType === 'SUBNET' ||
  r.routeType === 'BGP' ||
  (r.name.startsWith('default-route-') && r.description === 'Default route to the Internet.')

/** nextHop says where a route sends its traffic. */
export function nextHop(r: Route) {
  if (r.nextHopGateway) return `Gateway ${lastSeg(r.nextHopGateway)}`
  if (r.nextHopIp) return `IP ${r.nextHopIp}`
  if (r.nextHopInstance) return `VM instance ${lastSeg(r.nextHopInstance)}`
  if (r.nextHopIlb) return `Internal load balancer ${lastSeg(r.nextHopIlb)}`
  if (r.nextHopVpnTunnel) return `VPN tunnel ${lastSeg(r.nextHopVpnTunnel)}`
  return '—'
}

/** Routes is the chosen Project's routes. */
export function Routes() {
  const [view] = useViewState()
  const query = useQuery(routesQuery(view.project ?? ''))
  const all = [...(query.data ?? [])].sort(byPriority)
  return (
    <div className="flex flex-col gap-6">
      <VpcHeader
        action={
          <Button asChild>
            <Link to="/compute/create-route">
              <Plus aria-hidden />
              Create route
            </Link>
          </Button>
        }
      />
      <RecordedNotice />
      <Card>
        <QueryStatus query={query} />
        {query.data && all.length === 0 && (
          <p className="text-sm text-muted-foreground">
            No routes in this Project yet: each new network gets a default route to the internet.
          </p>
        )}
        {all.length > 0 && <RouteTable routes={all} network />}
      </Card>
    </div>
  )
}

/** RouteTable lists routes, with their network unless one owns them all. */
export function RouteTable({ routes, network }: { routes: Route[]; network?: boolean }) {
  return (
    <Table aria-label="Routes">
      <TableHeader>
        <TableRow>
          <TableHead>Name</TableHead>
          {network && <TableHead>Network</TableHead>}
          <TableHead>Destination range</TableHead>
          <TableHead className="text-right">Priority</TableHead>
          <TableHead>Next hop</TableHead>
          <TableHead>Network tags</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {routes.map((r) => (
          <TableRow key={r.selfLink ?? r.name} data-testid="route">
            <TableCell>
              <RouteLink name={r.name} />
              {isSystemRoute(r) && (
                <span className="block text-xs text-muted-foreground">System-generated</span>
              )}
            </TableCell>
            {network && (
              <TableCell>
                <NetworkLink link={r.network} />
              </TableCell>
            )}
            <TableCell className="font-mono">{r.destRange}</TableCell>
            <TableCell className="text-right font-mono">{priorityOf(r)}</TableCell>
            <TableCell>{nextHop(r)}</TableCell>
            <TableCell className="font-mono">
              {r.tags?.length ? r.tags.join(', ') : <span className="font-sans">All</span>}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}
