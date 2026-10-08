import { useQuery } from '@tanstack/react-query'

import type { Info } from '@/api/admin'
import { containersQuery, endpointsQuery, infoQuery, readinessQuery } from '@/api/queries'
import { QueryStatus } from '@/components/QueryStatus'
import { Badge } from '@/components/ui/badge'
import { Card, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { serviceName, SERVICES } from '@/lib/services'

import { EnvCard } from './EnvCard'
import { ResourceCountsCard } from './ResourceCounts'

const serviceOrder = (id: string) => {
  const i = SERVICES.findIndex((s) => s.id === id)
  return i < 0 ? SERVICES.length : i
}

function InstanceCard({ info }: { info: Info }) {
  const rows: [string, React.ReactNode][] = [
    ['Instance', <span className="font-mono">{info.instance}</span>],
    ['Instance ID', <span className="font-mono">{info.id}</span>],
    ['Version', <span className="font-mono">{info.version}</span>],
    [
      'State',
      info.ephemeral ? 'Ephemeral' : <span className="font-mono break-all">{info.dir}</span>,
    ],
    ['IAM enforcement', info.iamMode],
    ['Projects', info.strictProjects ? 'Strict' : 'Created on first use'],
  ]
  return (
    <Card aria-labelledby="instance-title">
      <CardHeader>
        <CardTitle id="instance-title">Instance</CardTitle>
      </CardHeader>
      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
        {rows.map(([k, v]) => (
          <div key={k} className="contents">
            <dt className="text-muted-foreground">{k}</dt>
            <dd>{v}</dd>
          </div>
        ))}
      </dl>
    </Card>
  )
}

function ReadinessCard() {
  const query = useQuery(readinessQuery())
  const services = Object.entries(query.data?.services ?? {}).sort(
    ([a], [b]) => serviceOrder(a) - serviceOrder(b),
  )
  return (
    <Card aria-labelledby="ready-title">
      <CardHeader>
        <CardTitle id="ready-title">Services</CardTitle>
        {query.data && (
          <Badge variant={query.data.ready ? 'success' : 'warning'}>
            {query.data.ready ? 'All ready' : 'Not ready'}
          </Badge>
        )}
      </CardHeader>
      <QueryStatus query={query} />
      {query.data && (
        <Table aria-labelledby="ready-title">
          <TableHeader>
            <TableRow>
              <TableHead>Service</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Reason</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {services.map(([id, s]) => (
              <TableRow key={id} data-testid={`ready-${id}`}>
                <TableCell>
                  {serviceName(id)} <span className="font-mono text-muted-foreground">{id}</span>
                </TableCell>
                <TableCell>
                  <Badge variant={s.ready ? 'success' : 'destructive'}>
                    {s.ready ? 'Ready' : 'Not ready'}
                  </Badge>
                </TableCell>
                <TableCell className="text-muted-foreground">{s.reason}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}

function RuntimeCard({ runtime }: { runtime: Info['runtime'] }) {
  const ok = runtime.reachable
  return (
    <Card aria-labelledby="runtime-title">
      <CardHeader>
        <CardTitle id="runtime-title">Container runtime</CardTitle>
        <Badge variant={ok ? 'success' : 'warning'}>{ok ? 'Reachable' : 'Not reachable'}</Badge>
      </CardHeader>
      <p className="text-sm">
        {runtime.kind === 'none' ? (
          'No container runtime found'
        ) : (
          <>
            <span className="capitalize">{runtime.kind}</span>
            {runtime.version && <span className="font-mono"> {runtime.version}</span>}
          </>
        )}
      </p>
      {runtime.error && <p className="text-sm text-muted-foreground">{runtime.error}</p>}
      <CardDescription>
        Needed only by Services that run Managed containers (Cloud SQL, GKE, Cloud NAT, host mode).
      </CardDescription>
    </Card>
  )
}

function EndpointsCard() {
  const query = useQuery(endpointsQuery())
  const names = Object.keys(query.data ?? {}).sort((a, b) =>
    a === 'gateway' ? -1 : b === 'gateway' ? 1 : a.localeCompare(b),
  )
  return (
    <Card aria-labelledby="endpoints-title">
      <CardHeader>
        <CardTitle id="endpoints-title">Service endpoints</CardTitle>
      </CardHeader>
      <QueryStatus query={query} />
      {query.data && (
        <Table aria-labelledby="endpoints-title">
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Address</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {names.map((n) => (
              <TableRow key={n}>
                <TableCell className="font-mono">{n}</TableCell>
                <TableCell className="font-mono">{query.data[n]}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}

function ContainersCard() {
  const query = useQuery(containersQuery())
  return (
    <Card aria-labelledby="containers-title">
      <CardHeader>
        <CardTitle id="containers-title">Managed containers</CardTitle>
      </CardHeader>
      <QueryStatus query={query} />
      {query.data?.length === 0 && (
        <p className="text-sm text-muted-foreground">This Instance runs no containers.</p>
      )}
      {!!query.data?.length && (
        <Table aria-labelledby="containers-title">
          <TableHeader>
            <TableRow>
              <TableHead>Service</TableHead>
              <TableHead>Resource</TableHead>
              <TableHead>Role</TableHead>
              <TableHead>Name</TableHead>
              <TableHead>Image</TableHead>
              <TableHead>State</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {query.data.map((c) => (
              <TableRow key={c.name}>
                <TableCell className="font-mono">{c.service}</TableCell>
                <TableCell>{c.resource}</TableCell>
                <TableCell>{c.role}</TableCell>
                <TableCell className="font-mono">{c.name}</TableCell>
                <TableCell className="font-mono">{c.image}</TableCell>
                <TableCell>{c.state}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}

/** Dashboard is the Instance-wide overview (FR-UI-010). */
export function Dashboard() {
  const { data: info } = useQuery(infoQuery())
  return (
    <div className="flex flex-col gap-6">
      <h1 className="text-2xl font-semibold">Dashboard</h1>
      <div className="grid gap-6 xl:grid-cols-2">
        <ReadinessCard />
        <div className="flex flex-col gap-6">
          {info && <InstanceCard info={info} />}
          {info && <RuntimeCard runtime={info.runtime} />}
        </div>
        <EndpointsCard />
        <ResourceCountsCard />
        <div className="xl:col-span-2">
          <ContainersCard />
        </div>
        <div className="xl:col-span-2">
          <EnvCard />
        </div>
      </div>
    </div>
  )
}
