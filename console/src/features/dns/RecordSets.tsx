import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Pencil, Plus, Search, Trash2 } from 'lucide-react'
import { useState } from 'react'

import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { toast } from '@/lib/toast'

import {
  applyChange,
  isApexRecord,
  rrsetKey,
  rrsetsQuery,
  zoneNetworks,
  type ResourceRecordSet,
} from './api'
import { queryPath, rrsetPath, useZoneRef, zonePath } from './DnsLayout'
import { useZone } from './ZonePage'

/** rrdataLines are a record set's data, or its routing policy's, one per line. */
export function rrdataLines(rs: ResourceRecordSet): string[] {
  if (rs.rrdatas) return rs.rrdatas
  const p = rs.routingPolicy
  if (p?.wrr) {
    return (p.wrr.items ?? []).map((i) => `weight ${i.weight ?? 0}: ${(i.rrdatas ?? []).join(' ')}`)
  }
  if (p?.geo) {
    return (p.geo.items ?? []).map((i) => `${i.location ?? ''}: ${(i.rrdatas ?? []).join(' ')}`)
  }
  return []
}

/**
 * RecordSets lists the zone's record sets. Adding, editing and deleting
 * them are changes (changes.create); deleting several is one change.
 */
export function RecordSets() {
  const z = useZone()
  const ref = useZoneRef()
  const query = useQuery(rrsetsQuery(ref))
  const qc = useQueryClient()
  const [text, setText] = useState('')
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const del = useMutation({
    mutationFn: (deletions: ResourceRecordSet[]) => applyChange(ref, { deletions }),
    onSuccess: (c, deletions) => {
      void qc.invalidateQueries({ queryKey: ['dns'] })
      setSelected(new Set())
      toast.success(
        `Deleted ${deletions.length === 1 ? `record set ${deletions[0]!.name} (${deletions[0]!.type})` : `${deletions.length} record sets`} in change ${c.id}.`,
      )
    },
  })
  const all = query.data ?? []
  const shown = all.filter(
    (rs) => rs.name.includes(text) || rs.type.toLowerCase() === text.toLowerCase(),
  )
  const chosen = all.filter((rs) => selected.has(rrsetKey(rs)))
  const network = zoneNetworks(z)[0]
  const toggle = (rs: ResourceRecordSet) =>
    setSelected((prev) => {
      const next = new Set(prev)
      const k = rrsetKey(rs)
      if (next.has(k)) next.delete(k)
      else next.add(k)
      return next
    })

  return (
    <Card>
      <div className="flex flex-wrap items-end justify-between gap-3">
        <label htmlFor="rrsets-filter" className="flex min-w-48 flex-1 flex-col gap-1 text-sm">
          Filter
          <Input
            id="rrsets-filter"
            type="search"
            placeholder="DNS name or type"
            value={text}
            onChange={(e) => setText(e.target.value)}
          />
        </label>
        <div className="flex flex-wrap gap-2">
          <ConfirmDialog
            trigger={
              <Button variant="outline" disabled={chosen.length === 0}>
                <Trash2 aria-hidden />
                Delete selected
              </Button>
            }
            title={`Delete ${chosen.length} record set${chosen.length === 1 ? '' : 's'}?`}
            description={
              <>
                One change deletes{' '}
                <span className="font-mono">
                  {chosen.map((rs) => `${rs.name} (${rs.type})`).join(', ')}
                </span>
                .
              </>
            }
            confirmLabel="Delete record sets"
            onConfirm={() => del.mutateAsync(chosen)}
          />
          <Button asChild>
            <Link to={zonePath(ref.zone, 'rrsets/create')}>
              <Plus aria-hidden />
              Add record set
            </Link>
          </Button>
        </div>
      </div>
      <QueryStatus query={query} />
      {query.data && all.length > 0 && shown.length === 0 && (
        <p className="text-sm text-muted-foreground">No record sets match the filter.</p>
      )}
      {shown.length > 0 && (
        <Table aria-label="Record sets">
          <TableHeader>
            <TableRow>
              <TableHead className="w-8">
                <span className="sr-only">Select</span>
              </TableHead>
              <TableHead>DNS name</TableHead>
              <TableHead>Type</TableHead>
              <TableHead>TTL</TableHead>
              <TableHead>Data</TableHead>
              <TableHead>
                <span className="sr-only">Actions</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {shown.map((rs) => {
              const label = `${rs.name} (${rs.type})`
              const apex = isApexRecord(rs, z)
              return (
                <TableRow key={rrsetKey(rs)} data-testid="rrset">
                  <TableCell>
                    <input
                      type="checkbox"
                      aria-label={`Select ${label}`}
                      disabled={apex}
                      title={apex ? 'A zone keeps its apex SOA and NS record sets.' : undefined}
                      checked={selected.has(rrsetKey(rs))}
                      onChange={() => toggle(rs)}
                    />
                  </TableCell>
                  <TableCell className="font-mono">{rs.name}</TableCell>
                  <TableCell>{rs.type}</TableCell>
                  <TableCell>{rs.ttl}</TableCell>
                  <TableCell className="font-mono text-xs break-all whitespace-pre-wrap">
                    {rs.routingPolicy && (
                      <span className="font-sans text-muted-foreground">
                        {rs.routingPolicy.wrr ? 'Weighted round robin' : 'Geolocation'}
                        {'\n'}
                      </span>
                    )}
                    {rrdataLines(rs).join('\n')}
                  </TableCell>
                  <TableCell>
                    <div className="flex justify-end gap-1">
                      <Button variant="ghost" size="icon" asChild>
                        <Link
                          to={queryPath(rs.name, rs.type, network)}
                          aria-label={`Query ${label}`}
                          title="Query"
                        >
                          <Search aria-hidden />
                        </Link>
                      </Button>
                      <Button variant="ghost" size="icon" asChild>
                        <Link
                          to={rrsetPath(ref.zone, rs.name, rs.type, 'edit')}
                          aria-label={`Edit ${label}`}
                          title="Edit"
                        >
                          <Pencil aria-hidden />
                        </Link>
                      </Button>
                      {!apex && (
                        <ConfirmDialog
                          trigger={
                            <Button
                              variant="ghost"
                              size="icon"
                              aria-label={`Delete ${label}`}
                              title="Delete"
                            >
                              <Trash2 aria-hidden />
                            </Button>
                          }
                          title={`Delete record set ${label}?`}
                          description="A change deletes the record set; the DNS server stops answering with it at once."
                          confirmLabel="Delete record set"
                          onConfirm={() => del.mutateAsync([rs])}
                        />
                      )}
                    </div>
                  </TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}
