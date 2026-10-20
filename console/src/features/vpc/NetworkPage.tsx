import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Pencil, Plus, Shuffle, Trash2 } from 'lucide-react'

import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { CopyAs } from '@/components/resource/CopyAs'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import { toast } from '@/lib/toast'

import { relPath } from '../lb/api'
import {
  byPriority,
  deleteNetwork,
  firewallsQuery,
  networkQuery,
  routesQuery,
  subnetsQuery,
  switchToCustomMode,
  type Network,
} from './api'
import { ContainerNetworkSummary } from './ContainerNetworks'
import { networkRecipe } from './copy'
import { FirewallTable } from './Firewalls'
import { isEmulatorDefault, routingMode } from './Networks'
import { RecordedNotice } from './RecordedNotice'
import { RouteTable } from './Routes'
import { bySubnet, SubnetTable } from './Subnetworks'
import { networkPage } from './links'
import { useNetworkRef } from './VpcLayout'

/**
 * NetworkPage is one VPC network: its settings, its subnetworks,
 * firewall rules and routes, and the network as the API returns it.
 */
export function NetworkPage() {
  const ref = useNetworkRef()
  const query = useQuery(networkQuery(ref))
  const subnets = useQuery(subnetsQuery(ref.project))
  const n = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const firewalls = useQuery(firewallsQuery(ref.project))
  const routes = useQuery(routesQuery(ref.project))
  const onNetwork = (r: { network?: string }) => !!n && relPath(r.network) === relPath(n.selfLink)
  const mine = (subnets.data ?? []).filter(onNetwork).sort(bySubnet)
  const myFirewalls = (firewalls.data ?? []).filter(onNetwork).sort(byPriority)
  const myRoutes = (routes.data ?? []).filter(onNetwork).sort(byPriority)
  const add = (what: string) => `/compute/create-${what}?network=${encodeURIComponent(ref.network)}`
  const remove = useMutation({
    mutationFn: () => deleteNetwork(ref),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(`Deleted network ${ref.network}.`)
      navigate('/compute')
    },
  })
  const custom = useMutation({
    mutationFn: () => switchToCustomMode(ref),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(`Network ${ref.network} is now a custom mode network.`)
    },
  })
  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to="/compute" className="underline-offset-4 hover:underline">
              VPC network
            </Link>{' '}
            / network
          </p>
          <h1 className="text-2xl font-semibold">
            <span className="font-mono">{ref.network}</span>
          </h1>
        </div>
        {n && (
          <div className="flex flex-wrap gap-2">
            <CopyAs what="network" recipe={() => networkRecipe(n, ref.project)} />
            <Button variant="outline" asChild>
              <Link to={networkPage(ref.network, 'edit')}>
                <Pencil aria-hidden />
                Edit network
              </Link>
            </Button>
            {n.autoCreateSubnetworks && (
              <ConfirmDialog
                trigger={
                  <Button variant="outline">
                    <Shuffle aria-hidden />
                    Switch to custom mode
                  </Button>
                }
                title={`Switch ${ref.network} to custom mode?`}
                description="Its subnetworks stay, but no new region gets one, and you add subnetworks yourself. A custom mode network cannot be switched back."
                confirmLabel="Switch to custom mode"
                onConfirm={() => custom.mutateAsync()}
              />
            )}
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Delete network
                </Button>
              }
              title={`Delete network ${ref.network}?`}
              description={deleteText(n, mine.length)}
              confirmLabel="Delete network"
              onConfirm={() => remove.mutateAsync()}
            />
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {n && isEmulatorDefault(n) && (
        <p role="note" className="text-sm text-muted-foreground">
          Created by the emulator the first time a resource used the Project&apos;s default network,
          such as a GKE cluster or Cloud SQL instance that named no network. GCP creates it with
          each new Project.
        </p>
      )}
      {n && (
        <>
          <Card aria-labelledby="network-title">
            <CardTitle id="network-title">Network</CardTitle>
            <DetailList
              label="Network details"
              rows={[
                [
                  'Subnet creation mode',
                  n.autoCreateSubnetworks
                    ? 'Auto: a subnetwork in every region'
                    : 'Custom: subnetworks you add',
                ],
                ['MTU', <Mono key="m">{n.mtu ?? 1460}</Mono>],
                ['Dynamic routing mode', <Mono key="r">{routingMode(n)}</Mono>],
                ['Description', n.description],
                ['Created', n.creationTimestamp && <Mono key="c">{n.creationTimestamp}</Mono>],
              ]}
            />
          </Card>
          <Card aria-labelledby="subnets-title">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <CardTitle id="subnets-title">Subnetworks</CardTitle>
              <Button variant="outline" size="sm" asChild>
                <Link to={add('subnetwork')}>
                  <Plus aria-hidden />
                  Add subnetwork
                </Link>
              </Button>
            </div>
            <QueryStatus query={subnets} />
            {subnets.data && mine.length === 0 && (
              <p className="text-sm text-muted-foreground">
                No subnetworks in this network yet: workloads need one in their region.
              </p>
            )}
            {mine.length > 0 && <SubnetTable subnets={mine} />}
          </Card>
          <RecordedNotice />
          <Card aria-labelledby="firewalls-title">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <CardTitle id="firewalls-title">Firewall rules</CardTitle>
              <Button variant="outline" size="sm" asChild>
                <Link to={add('firewall')}>
                  <Plus aria-hidden />
                  Add firewall rule
                </Link>
              </Button>
            </div>
            <QueryStatus query={firewalls} />
            {firewalls.data && myFirewalls.length === 0 && (
              <p className="text-sm text-muted-foreground">No firewall rules in this network.</p>
            )}
            {myFirewalls.length > 0 && <FirewallTable firewalls={myFirewalls} />}
          </Card>
          <Card aria-labelledby="routes-title">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <CardTitle id="routes-title">Routes</CardTitle>
              <Button variant="outline" size="sm" asChild>
                <Link to={add('route')}>
                  <Plus aria-hidden />
                  Add route
                </Link>
              </Button>
            </div>
            <QueryStatus query={routes} />
            {routes.data && myRoutes.length === 0 && (
              <p className="text-sm text-muted-foreground">No routes in this network.</p>
            )}
            {myRoutes.length > 0 && <RouteTable routes={myRoutes} />}
          </Card>
          <ContainerNetworkSummary project={ref.project} network={ref.network} />
          <Card>
            <CardTitle>Resource</CardTitle>
            <JsonView value={n} label="Network JSON" />
          </Card>
        </>
      )}
    </div>
  )
}

/** deleteText says what deleting network n does to its subnetworks. */
function deleteText(n: Network, subnets: number) {
  if (subnets === 0) return 'The network has no subnetworks.'
  if (n.autoCreateSubnetworks) return 'Its auto mode subnetworks go with it, unless one is in use.'
  return 'Delete its subnetworks first: the API refuses to delete a custom mode network that has any.'
}
