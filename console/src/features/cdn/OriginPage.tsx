import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Pencil, Power } from 'lucide-react'
import { useState } from 'react'

import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { DetailList, JsonView, Mono, type DetailRow } from '@/components/resource/DetailList'
import { Badge } from '@/components/ui/badge'
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

import {
  kindQuery,
  patchResource,
  relPath,
  resourcePath,
  resourceQuery,
  text,
  type Resource,
} from '../lb/api'
import { lbPath } from '../lb/LbLayout'
import {
  bytes,
  duration,
  entriesQuery,
  hitRatio,
  isOrigin,
  isOriginColl,
  kindName,
  percent,
  serviceRefs,
  statsQuery,
  type CdnEntry,
} from './api'
import { originPath, useOriginRef, type OriginRef } from './CdnLayout'
import { Invalidate } from './Invalidate'
import { policy } from './Origins'

/**
 * OriginPage is one origin: its hit ratio and traffic, cache settings,
 * cached entries, invalidation through the URL maps that route to it, and
 * the backend as the API returns it. Edit changes the cache settings;
 * removing the origin turns Cloud CDN off for the backend, as GCP's
 * console does.
 */
export function OriginPage() {
  const ref = useOriginRef()
  const ok = isOriginColl(ref.coll)
  const query = useQuery({ ...resourceQuery(ref), enabled: ok })
  const r = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const remove = useMutation({
    mutationFn: () => patchResource(ref, { enableCdn: false, fingerprint: r?.fingerprint }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['cdn'] })
      void qc.invalidateQueries({ queryKey: ['lb'] })
      toast.success(`Turned Cloud CDN off for ${ref.name}.`)
      navigate('/cdn')
    },
  })
  if (!ok) {
    return (
      <Card role="status">
        <p className="text-sm text-muted-foreground">
          {ref.coll} cannot be a Cloud CDN origin: backend services and buckets can.
        </p>
      </Card>
    )
  }
  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to="/cdn" className="underline-offset-4 hover:underline">
              Cloud CDN
            </Link>{' '}
            / {kindName(ref.coll)}
          </p>
          <h1 className="text-2xl font-semibold">
            <span className="font-mono">{ref.name}</span>
          </h1>
        </div>
        {r && isOrigin(r) && (
          <div className="flex flex-wrap gap-2">
            <Button variant="outline" asChild>
              <Link to={originPath(ref, 'edit')}>
                <Pencil aria-hidden />
                Edit cache settings
              </Link>
            </Button>
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Power aria-hidden />
                  Remove origin
                </Button>
              }
              title={`Turn Cloud CDN off for ${ref.name}?`}
              description={`The ${kindName(ref.coll)} keeps serving through the load balancer, uncached; its cache settings are kept. Its cached entries are no longer served.`}
              confirmLabel="Remove origin"
              onConfirm={() => remove.mutateAsync()}
            />
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {r && !isOrigin(r) && (
        <Card role="status">
          <p className="text-sm text-muted-foreground">
            Cloud CDN is off for this {kindName(ref.coll)}.{' '}
            <Link
              to={`/cdn/add?backend=${encodeURIComponent(resourcePath(ref))}`}
              className="text-primary underline-offset-4 hover:underline"
            >
              Add it as an origin
            </Link>
            .
          </p>
        </Card>
      )}
      {r && isOrigin(r) && <OriginPanels origin={r} r={ref} />}
      {r && (
        <Card>
          <CardTitle>Resource</CardTitle>
          <p className="text-sm text-muted-foreground">
            The {kindName(ref.coll)}{' '}
            <Link
              to={lbPath(ref)}
              className="font-mono text-primary underline-offset-4 hover:underline"
            >
              {ref.name}
            </Link>{' '}
            in the Load Balancer view.
          </p>
          <JsonView value={r} label={`${kindName(ref.coll)} JSON`} />
        </Card>
      )}
    </div>
  )
}

function OriginPanels({ origin, r }: { origin: Resource; r: OriginRef }) {
  const stats = useQuery(statsQuery(r.project))
  const maps = useQuery(kindQuery(r.project, 'urlMaps'))
  const self = relPath(origin.selfLink) || resourcePath(r)
  const s = stats.data?.byBackend.get(self)
  const using = (maps.data ?? []).filter((m) => serviceRefs(m).includes(self))
  return (
    <>
      <Card aria-labelledby="traffic-title">
        <CardTitle id="traffic-title">Traffic</CardTitle>
        <QueryStatus query={stats} />
        {stats.data && (
          <DetailList
            label="Origin traffic"
            rows={[
              ['Hit ratio', <Mono key="r">{percent(s && hitRatio(s))}</Mono>],
              ['Hits', <Mono key="h">{s?.hits ?? 0}</Mono>],
              ['Revalidated', <Mono key="v">{s?.revalidated ?? 0}</Mono>],
              ['Misses', <Mono key="m">{s?.misses ?? 0}</Mono>],
              ['Uncacheable', <Mono key="u">{s?.uncacheable ?? 0}</Mono>],
              ['Cached entries', <Mono key="e">{s?.entries ?? 0}</Mono>],
              ['Cached size', <Mono key="b">{bytes(s?.bytes ?? 0)}</Mono>],
            ]}
          />
        )}
      </Card>
      <Card aria-labelledby="settings-title">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <CardTitle id="settings-title">Cache settings</CardTitle>
        </div>
        <DetailList label="Origin cache settings" rows={settings(origin, r)} />
      </Card>
      <Entries backend={self} />
      {maps.data && <Invalidate maps={using} origin={r.name} />}
    </>
  )
}

const secs = (v: unknown) => (typeof v === 'number' ? `${duration(v)}` : undefined)
const list = (v: unknown) =>
  Array.isArray(v) && v.length ? <Mono>{v.map(text).join(', ')}</Mono> : undefined

/** settings are an origin's cdnPolicy, as the cache applies it. */
function settings(o: Resource, r: OriginRef): DetailRow[] {
  const p = (f: string) => policy(o, f)
  const key = (p('cacheKeyPolicy') ?? {}) as Record<string, unknown>
  const mode = text(p('cacheMode')) || 'CACHE_ALL_STATIC'
  const neg = p('negativeCachingPolicy') as { code?: number; ttl?: number }[] | undefined
  const rows: DetailRow[] = [
    ['Cache mode', <Mono key="m">{mode}</Mono>],
    ['Default TTL', secs(p('defaultTtl'))],
    ['Maximum TTL', secs(p('maxTtl'))],
    ['Client TTL', secs(p('clientTtl'))],
    ['Serve while stale', secs(p('serveWhileStale'))],
    [
      'Negative caching',
      p('negativeCaching') ? (
        neg?.length ? (
          <Mono key="n">{neg.map((n) => `${n.code}: ${duration(n.ttl ?? 0)}`).join(', ')}</Mono>
        ) : (
          'On (Cloud CDN’s TTL per status code)'
        )
      ) : (
        'Off'
      ),
    ],
    [
      'Bypass on request headers',
      list(
        (p('bypassCacheOnRequestHeaders') as { headerName?: string }[] | undefined)?.map(
          (h) => h.headerName,
        ),
      ),
    ],
    ['Signed request cache TTL', secs(p('signedUrlCacheMaxAgeSec'))],
    ['Signed URL keys', list(p('signedUrlKeyNames'))],
  ]
  if (r.coll === 'backendServices') {
    const parts = [
      key.includeProtocol && 'protocol',
      key.includeHost && 'host',
      key.includeQueryString && 'query string',
    ].filter(Boolean)
    rows.push(
      ['Cache key', parts.length ? parts.join(', ') : 'path only'],
      ['Query parameters included', list(key.queryStringWhitelist)],
      ['Query parameters excluded', list(key.queryStringBlacklist)],
    )
  } else {
    rows.push(
      ['Cache key', 'path and query string'],
      ['Query parameters included', list(key.queryStringWhitelist)],
    )
  }
  rows.push(
    ['Request headers in the key', list(key.includeHttpHeaders)],
    ['Cookies in the key', list(key.includeNamedCookies)],
  )
  return rows
}

/** Entries lists an origin's cached responses, filtered by path. */
function Entries({ backend }: { backend: string }) {
  const query = useQuery(entriesQuery(backend))
  const [q, setQ] = useState('')
  const all = query.data?.entries ?? []
  const shown = all.filter((e) => `${e.host}${e.path}`.includes(q))
  return (
    <Card aria-labelledby="entries-title">
      <CardTitle id="entries-title">Cached entries</CardTitle>
      <label htmlFor="entries-filter" className="flex max-w-md flex-col gap-1 text-sm">
        Filter
        <Input
          id="entries-filter"
          type="search"
          placeholder="Host or path"
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
      </label>
      <QueryStatus query={query} />
      {query.data && all.length === 0 && (
        <p className="text-sm text-muted-foreground">Nothing cached for this origin yet.</p>
      )}
      {all.length > 0 && shown.length === 0 && (
        <p className="text-sm text-muted-foreground">No entries match the filter.</p>
      )}
      {shown.length > 0 && (
        <Table aria-label="Cached entries">
          <TableHeader>
            <TableRow>
              <TableHead>Host and path</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Content type</TableHead>
              <TableHead className="text-right">Size</TableHead>
              <TableHead>Age / TTL</TableHead>
              <TableHead>Held in</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {shown.map((e) => (
              <TableRow
                key={`${e.cacheKey}\n${e.vary?.join(',') ?? ''}\n${e.stored}`}
                data-testid="entry"
              >
                <TableCell>
                  <span className="font-mono break-all" title={`Cache key: ${e.cacheKey}`}>
                    {e.host}
                    {e.path}
                  </span>
                  <EntryNotes e={e} />
                </TableCell>
                <TableCell className="font-mono">{e.status}</TableCell>
                <TableCell className="font-mono">{e.contentType ?? '—'}</TableCell>
                <TableCell className="text-right font-mono">{bytes(e.bytes)}</TableCell>
                <TableCell>
                  <span className="font-mono">
                    {duration(e.age)} / {duration(e.ttl)}
                  </span>{' '}
                  {e.age < e.ttl ? (
                    <Badge variant="success">Fresh</Badge>
                  ) : (
                    <Badge variant="warning">Stale</Badge>
                  )}
                </TableCell>
                <TableCell>{e.tier}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
      {query.data?.truncated && (
        <p className="text-xs text-muted-foreground">Showing the first {all.length} entries.</p>
      )}
    </Card>
  )
}

function EntryNotes({ e }: { e: CdnEntry }) {
  const notes: string[] = []
  if (e.vary?.length) notes.push(`varies on ${e.vary.join(', ')}`)
  if (e.tags?.length) notes.push(`tags ${e.tags.join(', ')}`)
  if (!notes.length) return null
  return <span className="block text-xs text-muted-foreground">{notes.join(' · ')}</span>
}
