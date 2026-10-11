import { useQuery } from '@tanstack/react-query'

import { QueryStatus } from '@/components/QueryStatus'
import { Badge } from '@/components/ui/badge'
import { Card } from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'

import { time } from '../gcs/format'
import { changesQuery, type ResourceRecordSet } from './api'
import { useZoneRef } from './DnsLayout'
import { rrdataLines } from './RecordSets'

/** PENDING_POLL_MS is how often the list is refreshed while a change is pending. */
const PENDING_POLL_MS = 1000

const line = (sign: string, rs: ResourceRecordSet) =>
  `${sign} ${rs.name} ${rs.type} ${rs.ttl ?? ''} ${rrdataLines(rs).join(' | ')}`

/**
 * Changes is the zone's change history, newest first: every edit of its
 * record sets, as changes.list returns it. Change 0 created the apex SOA
 * and NS. A pending change (FR-CORE-023 latency) is polled until done.
 */
export function Changes() {
  const ref = useZoneRef()
  const query = useQuery({
    ...changesQuery(ref),
    refetchInterval: (q) =>
      q.state.data?.some((c) => c.status === 'pending') ? PENDING_POLL_MS : false,
  })
  return (
    <Card>
      <QueryStatus query={query} />
      {query.data && (
        <Table aria-label="Changes">
          <TableHeader>
            <TableRow>
              <TableHead>ID</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Started</TableHead>
              <TableHead>Record sets</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {query.data.map((c) => (
              <TableRow key={c.id} data-testid="change">
                <TableCell className="font-mono">{c.id}</TableCell>
                <TableCell>
                  <Badge variant={c.status === 'done' ? 'success' : 'warning'}>{c.status}</Badge>
                </TableCell>
                <TableCell>{time(c.startTime)}</TableCell>
                <TableCell className="font-mono text-xs break-all whitespace-pre-wrap">
                  {[
                    ...(c.deletions ?? []).map((rs) => line('−', rs)),
                    ...(c.additions ?? []).map((rs) => line('+', rs)),
                  ].join('\n')}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}
