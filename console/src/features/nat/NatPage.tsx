import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Pencil, Trash2 } from 'lucide-react'
import { useParams } from 'react-router'

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
import { SubnetList } from '../vpc/links'
import {
  coverage,
  mappingsQuery,
  patchRouter,
  routerPath,
  routerQuery,
  statusQuery,
  withoutNat,
  type Router,
  type RouterNat,
  type RouterRef,
} from './api'
import { natRecipe } from './copy'
import { natPage, routerPage, useRouterRef } from './NatLayout'
import { allocation, logging, natIps } from './RouterPage'
import { TranslationLog } from './Translations'

/** defaultPorts is the minimum ports per VM a NAT applies when unset. */
export const defaultPorts = (n: RouterNat) => (n.enableDynamicPortAllocation ? 32 : 64)

/**
 * NatPage is one NAT gateway: its settings, NAT IPs, the subnetworks it
 * covers, port usage by endpoint and its translation log. Edit and delete
 * patch the router's nats[].
 */
export function NatPage() {
  const ref = useRouterRef()
  const { nat: name = '' } = useParams()
  const query = useQuery(routerQuery(ref))
  const router = query.data
  const nat = router?.nats?.find((n) => n.name === name)
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const remove = useMutation({
    mutationFn: () =>
      patchRouter(ref, { nats: withoutNat(router!, name), fingerprint: router?.fingerprint }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(`Deleted NAT gateway ${name}.`)
      navigate(routerPage(ref))
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
            /{' '}
            <Link to={routerPage(ref)} className="font-mono underline-offset-4 hover:underline">
              {ref.router}
            </Link>{' '}
            / NAT gateway
          </p>
          <h1 className="text-2xl font-semibold">
            <span className="font-mono">{name}</span>{' '}
            <span className="text-sm font-normal text-muted-foreground">{ref.region}</span>
          </h1>
        </div>
        {nat && (
          <div className="flex flex-wrap gap-2">
            <CopyAs what="NAT gateway" recipe={() => natRecipe(router!, nat, ref)} />
            <Button variant="outline" asChild>
              <Link to={natPage(ref, name, 'edit')}>
                <Pencil aria-hidden />
                Edit NAT gateway
              </Link>
            </Button>
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Delete NAT gateway
                </Button>
              }
              title={`Delete NAT gateway ${name}?`}
              description={`Workloads it covers lose internet egress. Router ${ref.router} stays.`}
              confirmLabel="Delete NAT gateway"
              onConfirm={() => remove.mutateAsync()}
            />
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {router && !nat && (
        <Card role="status">
          <p className="text-sm text-muted-foreground">
            Router <span className="font-mono">{ref.router}</span> has no NAT gateway named{' '}
            <span className="font-mono">{name}</span>.
          </p>
        </Card>
      )}
      {router && nat && <NatPanels r={ref} router={router} nat={nat} />}
    </div>
  )
}

function NatPanels({ r, router, nat }: { r: RouterRef; router: Router; nat: RouterNat }) {
  const status = useQuery(statusQuery(r))
  const s = status.data?.find((x) => x.name === nat.name)
  const ips = natIps(s)
  return (
    <>
      <Card aria-labelledby="settings-title">
        <CardTitle id="settings-title">Settings</CardTitle>
        <QueryStatus query={status} />
        <DetailList
          label="NAT gateway settings"
          rows={[
            ['Network', <Mono key="n">{lastSeg(router.network)}</Mono>],
            ['NAT IP allocation', allocation(nat)],
            ['NAT IPs', ips.length ? <Mono key="i">{ips.join(', ')}</Mono> : status.data && '—'],
            [
              'Reserved addresses',
              nat.natIps?.length ? <Mono key="a">{nat.natIps.map(lastSeg).join(', ')}</Mono> : '',
            ],
            ['Source ranges', <Mono key="s">{nat.sourceSubnetworkIpRangesToNat}</Mono>],
            [
              'Minimum ports per VM',
              <Mono key="p">{nat.minPortsPerVm ?? `${defaultPorts(nat)} (default)`}</Mono>,
            ],
            ['Dynamic port allocation', nat.enableDynamicPortAllocation ? 'On' : 'Off'],
            [
              'Maximum ports per VM',
              nat.enableDynamicPortAllocation && nat.maxPortsPerVm ? (
                <Mono key="m">{nat.maxPortsPerVm}</Mono>
              ) : (
                ''
              ),
            ],
            ['Endpoint-independent mapping', nat.enableEndpointIndependentMapping ? 'On' : 'Off'],
            ['Logging', logging(nat)],
          ]}
        />
      </Card>
      <Covered r={r} router={router} nat={nat} />
      <PortUsage r={r} nat={nat} />
      <section aria-labelledby="log-title" className="flex flex-col gap-3">
        <h2 id="log-title" className="text-lg font-semibold">
          Translation log
        </h2>
        <TranslationLog project={r.project} gateway={`${routerPath(r)}/nats/${nat.name}`} />
      </section>
      <Card>
        <CardTitle>Resource</CardTitle>
        <p className="text-sm text-muted-foreground">
          The NAT config, as{' '}
          <Link
            to={routerPage(r)}
            className="font-mono text-primary underline-offset-4 hover:underline"
          >
            {r.router}
          </Link>
          &apos;s nats[] holds it.
        </p>
        <JsonView value={nat} label="NAT JSON" />
      </Card>
    </>
  )
}

function Covered({ r, router, nat }: { r: RouterRef; router: Router; nat: RouterNat }) {
  const subnets = useQuery(subnetsQuery(r.project))
  const covered = subnets.data ? coverage(router, nat, subnets.data) : []
  return (
    <Card aria-labelledby="covered-title">
      <CardTitle id="covered-title">Covered subnetworks</CardTitle>
      <QueryStatus query={subnets} />
      {subnets.data && covered.length === 0 && (
        <p className="text-sm text-muted-foreground">
          No subnetworks of <span className="font-mono">{lastSeg(router.network)}</span> in{' '}
          <span className="font-mono">{r.region}</span> are covered: their workloads cannot reach
          the internet through this NAT.
        </p>
      )}
      {covered.length > 0 && (
        <Table aria-label="Covered subnetworks">
          <TableHeader>
            <TableRow>
              <TableHead>Subnetwork</TableHead>
              <TableHead>Ranges translated</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {covered.map((c) => (
              <TableRow key={c.subnetwork.name} data-testid="covered">
                <TableCell>
                  <SubnetList subnets={[c.subnetwork]} />
                </TableCell>
                <TableCell>
                  <span className="flex flex-wrap gap-x-4">
                    {c.ranges.map((x) => (
                      <span key={x.name}>
                        <span className="font-mono">{x.cidr}</span>{' '}
                        <span className="text-xs text-muted-foreground">{x.name}</span>
                      </span>
                    ))}
                  </span>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}

/** PortUsage is getNatMappingInfo of the NAT: ports allocated to each endpoint. */
function PortUsage({ r, nat }: { r: RouterRef; nat: RouterNat }) {
  const query = useQuery(mappingsQuery(r, nat.name))
  const rows = (query.data ?? []).flatMap((m) =>
    (m.interfaceNatMappings ?? []).map((i) => ({ instance: m.instanceName ?? '', ...i })),
  )
  const total = rows.reduce((t, x) => t + (x.numTotalNatPorts ?? 0), 0)
  return (
    <Card aria-labelledby="ports-title">
      <CardTitle id="ports-title">Port usage</CardTitle>
      <QueryStatus query={query} />
      {query.data && rows.length === 0 && (
        <p className="text-sm text-muted-foreground">
          No endpoints have NAT ports yet. Workloads on covered subnetworks, such as GKE nodes, get{' '}
          {nat.minPortsPerVm ?? defaultPorts(nat)} ports each once they are running.
        </p>
      )}
      {rows.length > 0 && (
        <>
          <p className="text-sm text-muted-foreground">
            {rows.length} {rows.length === 1 ? 'endpoint' : 'endpoints'}, {total} ports allocated.
          </p>
          <Table aria-label="Port usage">
            <TableHeader>
              <TableRow>
                <TableHead>Endpoint</TableHead>
                <TableHead>Source IP</TableHead>
                <TableHead>NAT IP and ports</TableHead>
                <TableHead className="text-right">Ports</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((x) => (
                <TableRow key={`${x.instance}/${x.sourceVirtualIp}`} data-testid="mapping">
                  <TableCell className="font-mono break-all">{lastSeg(x.instance)}</TableCell>
                  <TableCell className="font-mono">{x.sourceVirtualIp}</TableCell>
                  <TableCell className="font-mono">
                    {x.natIpPortRanges?.join(', ') || '—'}
                  </TableCell>
                  <TableCell className="text-right font-mono">{x.numTotalNatPorts ?? 0}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </>
      )}
    </Card>
  )
}
