import { useQuery } from '@tanstack/react-query'

import { QueryStatus } from '@/components/QueryStatus'
import { Card, CardTitle } from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'

import { StatusBadge } from '../gke/format'
import { HEALTH_POLL_MS, healthQuery, lastSeg, relPath, type Ref, type Resource } from './api'
import { RefLink } from './LbLayout'

const TONES: Record<string, 'success' | 'warning' | 'destructive' | 'default'> = {
  HEALTHY: 'success',
  UNKNOWN: 'warning',
  DRAINING: 'warning',
  UNHEALTHY: 'destructive',
}

export function HealthBadge({ state }: { state?: string }) {
  const s = state ?? 'UNKNOWN'
  return <StatusBadge status={s} tone={TONES[s] ?? 'default'} />
}

/** groupLabel names a backend group: its name and zone or region. */
const groupLabel = (g: string) => {
  const m = /\/(?:zones|regions)\/([^/]+)\//.exec(relPath(g))
  return `${lastSeg(g)}${m ? ` (${m[1]})` : ''}`
}

/**
 * BackendHealth shows backendServices.getHealth for each backend group:
 * every network endpoint's health as the health checks see it (FR-LB-007).
 */
export function BackendHealth({ bs, r }: { bs: Resource; r: Ref }) {
  const backends = (bs.backends ?? []) as { group: string; balancingMode?: string }[]
  const hc = (bs.healthChecks as string[] | undefined)?.[0]
  return (
    <Card aria-labelledby="health-title">
      <CardTitle id="health-title">Backend health</CardTitle>
      <p className="text-sm text-muted-foreground">
        {hc ? (
          <>
            Probed by health check <RefLink link={hc} />; refreshed every {HEALTH_POLL_MS / 1000} s.
          </>
        ) : (
          'No health check: every endpoint is treated as healthy.'
        )}
      </p>
      {backends.length === 0 && (
        <p className="text-sm text-muted-foreground">No backends. Add a NEG to serve traffic.</p>
      )}
      {backends.map((b) => (
        <GroupHealth key={b.group} r={r} group={b.group} mode={b.balancingMode} />
      ))}
    </Card>
  )
}

function GroupHealth({ r, group, mode }: { r: Ref; group: string; mode?: string }) {
  const query = useQuery(healthQuery(r, group))
  const eps = query.data ?? []
  return (
    <section aria-label={`Backend ${groupLabel(group)}`} className="flex flex-col gap-2">
      <h3 className="text-sm font-medium">
        <span className="font-mono">{groupLabel(group)}</span>
        {mode && <span className="font-normal text-muted-foreground"> · {mode}</span>}
      </h3>
      <QueryStatus query={query} />
      {query.data && eps.length === 0 && (
        <p className="text-sm text-muted-foreground">No endpoints in this group.</p>
      )}
      {eps.length > 0 && (
        <Table aria-label={`Endpoints of ${groupLabel(group)}`}>
          <TableHeader>
            <TableRow>
              <TableHead>Endpoint</TableHead>
              <TableHead>Health</TableHead>
              <TableHead>Instance</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {eps.map((e) => (
              <TableRow key={`${e.ipAddress}:${e.port}`} data-testid="endpoint">
                <TableCell className="font-mono">{`${e.ipAddress}:${e.port}`}</TableCell>
                <TableCell>
                  <HealthBadge state={e.healthState} />
                </TableCell>
                <TableCell className="font-mono">
                  {e.instance ? lastSeg(e.instance) : '—'}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </section>
  )
}
