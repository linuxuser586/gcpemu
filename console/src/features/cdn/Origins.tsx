import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Plus, Trash2 } from 'lucide-react'
import { useState } from 'react'

import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { DetailList, Mono } from '@/components/resource/DetailList'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
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
import { useViewState } from '@/lib/viewState'

import { kindQuery, refOf, relPath, scopeOf, text, type Resource } from '../lb/api'
import {
  backendsQuery,
  bytes,
  hitRatio,
  isOrigin,
  kindName,
  percent,
  purge,
  requests,
  serviceRefs,
  statsQuery,
  type CdnBackend,
} from './api'
import { originPath } from './CdnLayout'
import { Invalidate } from './Invalidate'

/** policy reads a field of an origin's cdnPolicy. */
export const policy = (r: Resource, field: string): unknown =>
  (r.cdnPolicy as Record<string, unknown> | undefined)?.[field]

/**
 * Origins is the chosen Project's Cloud CDN: the cache's usage, purging
 * it, every origin with its cache mode, entries, size and hit ratio
 * (?q= filters by name), and invalidation through a URL map.
 */
export function Origins() {
  const [view, setView] = useViewState()
  const project = view.project ?? ''
  const [q, setQ] = useState(view.q ?? '')
  const qc = useQueryClient()
  const backends = useQuery(backendsQuery(project))
  const stats = useQuery(statsQuery(project))
  const maps = useQuery(kindQuery(project, 'urlMaps'))
  const purgeAll = useMutation({
    mutationFn: purge,
    onSuccess: (res) => {
      void qc.invalidateQueries({ queryKey: ['cdn'] })
      toast.success(`Purged ${res.purged} cached ${res.purged === 1 ? 'entry' : 'entries'}.`)
    },
  })
  const origins = (backends.data ?? [])
    .filter((b) => isOrigin(b.r))
    .sort((a, b) => a.r.name.localeCompare(b.r.name))
  const shown = origins.filter((b) => b.r.name.includes(q))
  const originLinks = new Set(origins.map((b) => relPath(b.r.selfLink)))
  // URL maps that route to an origin: invalidation goes through them.
  const cdnMaps = (maps.data ?? [])
    .filter((m) => serviceRefs(m).some((s) => originLinks.has(s)))
    .sort((a, b) => a.name.localeCompare(b.name))
  const total = (stats.data?.backends ?? []).reduce(
    (t, b) => ({
      hits: t.hits + b.hits,
      misses: t.misses + b.misses,
      revalidated: t.revalidated + b.revalidated,
      uncacheable: t.uncacheable + b.uncacheable,
    }),
    { hits: 0, misses: 0, revalidated: 0, uncacheable: 0 },
  )

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Cloud CDN</h1>
          <p className="text-sm text-muted-foreground">
            Origins of Project <span className="font-mono">{project}</span>
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          <ConfirmDialog
            trigger={
              <Button variant="outline">
                <Trash2 aria-hidden />
                Purge all
              </Button>
            }
            title="Purge the whole cache?"
            description="Removes every cached entry of every origin, in all Projects of this Instance, like gcpemu cdn purge. GCP has no such call: it is the emulator’s."
            confirmLabel="Purge cache"
            onConfirm={() => purgeAll.mutateAsync()}
          />
          <Button asChild>
            <Link to="/cdn/add">
              <Plus aria-hidden />
              Add origin
            </Link>
          </Button>
        </div>
      </div>
      <Card aria-labelledby="usage-title">
        <CardTitle id="usage-title">Cache</CardTitle>
        <QueryStatus query={stats} />
        {stats.data && (
          <>
            <DetailList
              label="Cache usage"
              rows={[
                ['Entries', <Mono key="e">{stats.data.entries}</Mono>],
                [
                  'Size',
                  <Mono key="b">
                    {bytes(stats.data.bytes)} of {bytes(stats.data.limitBytes)}
                  </Mono>,
                ],
                ['Hit ratio of this Project', <Mono key="h">{percent(hitRatio(total))}</Mono>],
              ]}
            />
            <div
              role="meter"
              aria-label="Cache size"
              aria-valuemin={0}
              aria-valuemax={stats.data.limitBytes}
              aria-valuenow={stats.data.bytes}
              aria-valuetext={`${bytes(stats.data.bytes)} of ${bytes(stats.data.limitBytes)}`}
              className="h-2 overflow-hidden rounded-full bg-muted"
            >
              <div
                className="h-full bg-primary"
                style={{
                  width: `${Math.min(100, (stats.data.bytes / stats.data.limitBytes) * 100)}%`,
                }}
              />
            </div>
            <p className="text-xs text-muted-foreground">
              Entries and size are the Instance’s; the least recently used entries are evicted at
              the limit. Hit ratio counts hits and revalidations over all requests since the
              Instance started or was reset.
            </p>
          </>
        )}
      </Card>
      <Card>
        <form
          role="search"
          aria-label="Filter origins"
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => e.preventDefault()}
        >
          <label htmlFor="cdn-filter" className="flex min-w-48 flex-1 flex-col gap-1 text-sm">
            Filter
            <Input
              id="cdn-filter"
              type="search"
              placeholder="Name"
              value={q}
              onChange={(e) => {
                setQ(e.target.value)
                setView({ q: e.target.value })
              }}
            />
          </label>
        </form>
        <QueryStatus query={backends} />
        {backends.data && origins.length === 0 && (
          <p className="text-sm text-muted-foreground">
            No origins in this Project yet: add one to turn on Cloud CDN for a backend service or
            bucket.
          </p>
        )}
        {backends.data && origins.length > 0 && shown.length === 0 && (
          <p className="text-sm text-muted-foreground">No origins match the filter.</p>
        )}
        {shown.length > 0 && (
          <Table aria-label="Origins">
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Cache mode</TableHead>
                <TableHead className="text-right">Entries</TableHead>
                <TableHead className="text-right">Size</TableHead>
                <TableHead className="text-right">Requests</TableHead>
                <TableHead className="text-right">Hit ratio</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map(({ coll, r }) => {
                const s: CdnBackend | undefined = stats.data?.byBackend.get(relPath(r.selfLink))
                return (
                  <TableRow key={r.selfLink ?? r.name} data-testid="origin">
                    <TableCell>
                      <Link
                        className="font-mono text-primary underline-offset-4 hover:underline"
                        to={originPath(refOf(r, coll, project))}
                      >
                        {r.name}
                      </Link>
                      {scopeOf(r) !== 'global' && (
                        <span className="ml-2 font-mono text-xs text-muted-foreground">
                          {scopeOf(r)}
                        </span>
                      )}
                    </TableCell>
                    <TableCell>{kindName(coll)}</TableCell>
                    <TableCell className="font-mono">
                      {text(policy(r, 'cacheMode')) || 'CACHE_ALL_STATIC'}
                    </TableCell>
                    <TableCell className="text-right font-mono">{s?.entries ?? 0}</TableCell>
                    <TableCell className="text-right font-mono">{bytes(s?.bytes ?? 0)}</TableCell>
                    <TableCell className="text-right font-mono">{s ? requests(s) : 0}</TableCell>
                    <TableCell className="text-right font-mono">
                      {percent(s && hitRatio(s))}
                    </TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        )}
      </Card>
      {maps.data && backends.data && <Invalidate maps={cdnMaps} />}
    </div>
  )
}
