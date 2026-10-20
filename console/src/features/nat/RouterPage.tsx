import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Pencil, Plus, Trash2 } from 'lucide-react'

import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { CopyAs } from '@/components/resource/CopyAs'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { toast } from '@/lib/toast'

import { lastSeg } from '../lb/api'
import { subnetsQuery } from '../vpc/api'
import { SubnetList } from '../vpc/VpcLayout'
import {
  coverage,
  deleteRouter,
  routerQuery,
  statusQuery,
  type NatStatus,
  type Router,
  type RouterNat,
  type RouterRef,
} from './api'
import { routerRecipe } from './copy'
import { natPage, routerPage, useRouterRef } from './NatLayout'

/** natIps lists a NAT's active IPs from its router status. */
export const natIps = (s?: NatStatus) => [
  ...(s?.autoAllocatedNatIps ?? []),
  ...(s?.userAllocatedNatIps ?? []),
]

/** allocation describes how a NAT gets its IPs. */
export const allocation = (n: RouterNat) =>
  n.natIpAllocateOption === 'MANUAL_ONLY' ? 'Manual (reserved addresses)' : 'Automatic'

/**
 * RouterPage is one Cloud Router: its settings, its NAT gateways with
 * their IPs, endpoints and covered subnetworks, and the router as the API
 * returns it.
 */
export function RouterPage() {
  const ref = useRouterRef()
  const query = useQuery(routerQuery(ref))
  const r = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const remove = useMutation({
    mutationFn: () => deleteRouter(ref),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(`Deleted router ${ref.router}.`)
      navigate('/nat')
    },
  })
  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to="/nat" className="underline-offset-4 hover:underline">
              Cloud NAT
            </Link>{' '}
            / router
          </p>
          <h1 className="text-2xl font-semibold">
            <span className="font-mono">{ref.router}</span>{' '}
            <span className="text-sm font-normal text-muted-foreground">{ref.region}</span>
          </h1>
        </div>
        {r && (
          <div className="flex flex-wrap gap-2">
            <CopyAs what="router" recipe={() => routerRecipe(r, ref)} />
            <Button variant="outline" asChild>
              <Link to={routerPage(ref, 'edit')}>
                <Pencil aria-hidden />
                Edit router
              </Link>
            </Button>
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Delete router
                </Button>
              }
              title={`Delete router ${ref.router}?`}
              description={
                r.nats?.length
                  ? `Its NAT gateways (${r.nats.map((n) => n.name).join(', ')}) go with it: workloads they cover lose internet egress.`
                  : 'The router has no NAT gateways.'
              }
              confirmLabel="Delete router"
              onConfirm={() => remove.mutateAsync()}
            />
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {r && (
        <>
          <Card aria-labelledby="router-title">
            <CardTitle id="router-title">Router</CardTitle>
            <DetailList
              label="Router details"
              rows={[
                ['Region', <Mono key="r">{ref.region}</Mono>],
                ['Network', <Mono key="n">{lastSeg(r.network)}</Mono>],
                ['Description', r.description],
                ['BGP ASN', r.bgp?.asn !== undefined && <Mono key="a">{r.bgp.asn}</Mono>],
                ['Created', r.creationTimestamp && <Mono key="c">{r.creationTimestamp}</Mono>],
              ]}
            />
          </Card>
          <Nats r={ref} router={r} />
          <Card>
            <CardTitle>Resource</CardTitle>
            <JsonView value={r} label="Router JSON" />
          </Card>
        </>
      )}
    </div>
  )
}

function Nats({ r, router }: { r: RouterRef; router: Router }) {
  const status = useQuery(statusQuery(r))
  const subnets = useQuery(subnetsQuery(r.project))
  const nats = router.nats ?? []
  const byName = new Map((status.data ?? []).map((s) => [s.name, s]))
  return (
    <Card aria-labelledby="nats-title">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <CardTitle id="nats-title">NAT gateways</CardTitle>
        <Button variant="outline" size="sm" asChild>
          <Link to={routerPage(r, 'nats/add')}>
            <Plus aria-hidden />
            Add NAT gateway
          </Link>
        </Button>
      </div>
      {nats.length === 0 && (
        <p className="text-sm text-muted-foreground">
          No NAT gateways on this router: add one to let workloads on its region&apos;s subnetworks
          of <span className="font-mono">{lastSeg(router.network)}</span> reach the internet.
        </p>
      )}
      <QueryStatus query={status} />
      {nats.length > 0 && (
        <Table aria-label="NAT gateways">
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>NAT IPs</TableHead>
              <TableHead>Source ranges</TableHead>
              <TableHead>Covered subnetworks</TableHead>
              <TableHead className="text-right">Endpoints</TableHead>
              <TableHead>Logging</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {nats.map((n) => {
              const s = byName.get(n.name)
              const covered = subnets.data ? coverage(router, n, subnets.data) : []
              return (
                <TableRow key={n.name} data-testid="nat">
                  <TableCell>
                    <Link
                      className="font-mono text-primary underline-offset-4 hover:underline"
                      to={natPage(r, n.name)}
                    >
                      {n.name}
                    </Link>
                  </TableCell>
                  <TableCell>
                    <span className="flex flex-col">
                      <span className="font-mono">{natIps(s).join(', ') || '—'}</span>
                      <span className="text-xs text-muted-foreground">{allocation(n)}</span>
                    </span>
                  </TableCell>
                  <TableCell className="font-mono text-xs">
                    {n.sourceSubnetworkIpRangesToNat}
                  </TableCell>
                  <TableCell>
                    <SubnetList subnets={covered.map((c) => c.subnetwork)} />
                  </TableCell>
                  <TableCell className="text-right font-mono">
                    {s?.numVmEndpointsWithNatMappings ?? 0}
                  </TableCell>
                  <TableCell>{logging(n)}</TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}

/** logging describes a NAT's logConfig. */
export const logging = (n: RouterNat) =>
  n.logConfig?.enable
    ? n.logConfig.filter === 'ERRORS_ONLY'
      ? 'Errors only'
      : n.logConfig.filter === 'TRANSLATIONS_ONLY'
        ? 'Translations only'
        : 'Translations and errors'
    : 'Off'
