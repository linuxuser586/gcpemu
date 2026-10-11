import { useQuery } from '@tanstack/react-query'
import { ChevronDown, ChevronRight, Pause, Play } from 'lucide-react'
import { Fragment, useState } from 'react'
import { useSearchParams } from 'react-router'

import type { RequestEntry } from '@/api/admin'
import { requestsQuery } from '@/api/queries'
import { QueryStatus } from '@/components/QueryStatus'
import { DetailList, Mono } from '@/components/resource/DetailList'
import { Badge } from '@/components/ui/badge'
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
import { useViewState } from '@/lib/viewState'

import type { LbTrace } from './api'
import { KindTabs, RefLink } from './LbLayout'

// The Load Balancer traffic view (SRS 4.8.3): a live trace of the
// Project's load-balanced requests, read from the Request log, whose
// entries the events bridge appends as they happen (FR-UI-006). Each lb
// entry carries how it was served: forwarding rule, matched route,
// backend, cache status and latency. The filters are in the URL (?host=,
// ?path=), so that a trace can be shared.

/** SHOWN caps the rows rendered, newest first. */
export const SHOWN = 500

/** Traced is an lb entry; the Instance always sets time, method and status. */
type Traced = RequestEntry & { lb: LbTrace; time: string; method: string; status: number }

/** matches keeps the entries of project whose host and path contain the filters. */
export function matches(e: RequestEntry, project: string, host: string, path: string): e is Traced {
  return (
    !!e.lb &&
    (e.resource ?? '').startsWith(`projects/${project}/`) &&
    e.lb.host.toLowerCase().includes(host.toLowerCase()) &&
    e.lb.path.includes(path)
  )
}

/** routeText describes the URL map rule that matched. */
export function routeText(t: LbTrace): string {
  switch (t.match) {
    case 'routeRule':
      return `Route rule, priority ${t.priority ?? 0}`
    case 'pathRule':
      return `Path rule ${(t.paths ?? []).join(', ')}`
    case 'pathMatcherDefault':
      return `Default of ${t.pathMatcher ?? 'path matcher'}`
    case 'urlMapDefault':
      return 'URL map default'
    default:
      return '—'
  }
}

const ms = (v?: number) => (v === undefined ? '—' : `${v.toFixed(1)} ms`)

function clock(time: string) {
  const d = new Date(time)
  return `${d.toLocaleTimeString([], { hour12: false })}.${String(d.getMilliseconds()).padStart(3, '0')}`
}

function StatusBadge({ status }: { status: number }) {
  const variant =
    status >= 500
      ? 'destructive'
      : status >= 400
        ? 'warning'
        : status >= 300
          ? 'outline'
          : 'success'
  return <Badge variant={variant}>{status}</Badge>
}

function CacheBadge({ status }: { status?: string }) {
  if (!status) return <>—</>
  if (status === 'disabled') return <span className="text-muted-foreground">off</span>
  return <Badge variant={status === 'hit' ? 'success' : 'outline'}>{status}</Badge>
}

/** Details shows the rest of one request's trace. */
function Details({ e }: { e: Traced }) {
  const t = e.lb
  const mono = (v?: string) => v && <Mono>{v}</Mono>
  return (
    <DetailList
      label={`Trace of ${e.method}`}
      rows={[
        ['Time', mono(e.time)],
        ['Request', mono(e.method)],
        ['Forwarding rule', <RefLink key="fr" link={e.resource} />],
        ['Target proxy', t.targetProxy && <RefLink link={t.targetProxy} />],
        ['URL map', t.urlMap && <RefLink link={t.urlMap} />],
        ['Path matcher', mono(t.pathMatcher)],
        ['Route', routeText(t)],
        ['Redirect', mono(t.redirect)],
        ['Backend', t.backend && <RefLink link={t.backend} />],
        ['Endpoint', mono(t.endpoint)],
        ['Cache status', t.cacheStatus && <CacheBadge status={t.cacheStatus} />],
        ['Status details', mono(t.statusDetails)],
        ['Latency', ms(e.latencyMs)],
        ['Backend latency', t.backendLatencyMs && ms(t.backendLatencyMs)],
      ]}
    />
  )
}

/** Traffic is the live per-request trace of the chosen Project's load balancers. */
export function Traffic() {
  const [view] = useViewState()
  const project = view.project ?? ''
  const [search, setSearch] = useSearchParams()
  // The inputs lead the URL, which follows each keystroke.
  const [host, setHost] = useState(search.get('host') ?? '')
  const [path, setPath] = useState(search.get('path') ?? '')
  const query = useQuery(requestsQuery('lb'))
  // Paused keeps the entries seen when it was pressed.
  const [paused, setPaused] = useState<RequestEntry[]>()
  const [open, setOpen] = useState<string>()

  const setFilter = (k: 'host' | 'path', v: string) => {
    if (k === 'host') setHost(v)
    else setPath(v)
    setSearch(
      (prev) => {
        const next = new URLSearchParams(prev)
        if (v) next.set(k, v)
        else next.delete(k)
        return next
      },
      { replace: true },
    )
  }

  const entries = paused ?? query.data ?? []
  const all = entries.filter((e): e is Traced => matches(e, project, '', ''))
  const shown = all.filter((e) => matches(e, project, host, path)).reverse()

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Cloud Load Balancing</h1>
          <p className="text-sm text-muted-foreground">
            Requests to the load balancers of Project <span className="font-mono">{project}</span>
          </p>
        </div>
        <Button variant="outline" onClick={() => setPaused(paused ? undefined : entries)}>
          {paused ? <Play aria-hidden /> : <Pause aria-hidden />}
          {paused ? 'Resume' : 'Pause'}
        </Button>
      </div>
      <KindTabs coll="traffic" />
      <Card>
        <form
          role="search"
          aria-label="Filter requests"
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => e.preventDefault()}
        >
          <label htmlFor="lb-host" className="flex min-w-48 flex-1 flex-col gap-1 text-sm">
            Host
            <Input
              id="lb-host"
              type="search"
              placeholder="Host contains"
              value={host}
              onChange={(e) => setFilter('host', e.target.value.trim())}
            />
          </label>
          <label htmlFor="lb-path" className="flex min-w-48 flex-1 flex-col gap-1 text-sm">
            Path
            <Input
              id="lb-path"
              type="search"
              placeholder="Path contains"
              value={path}
              onChange={(e) => setFilter('path', e.target.value.trim())}
            />
          </label>
        </form>
        <QueryStatus query={query} />
        <p className="text-sm text-muted-foreground">
          {paused ? 'Paused. ' : query.data ? 'Live. ' : ''}
          {query.data &&
            (shown.length > SHOWN
              ? `Showing the latest ${SHOWN} of ${shown.length} requests.`
              : `${shown.length} ${shown.length === 1 ? 'request' : 'requests'}.`)}
        </p>
        {query.data && all.length === 0 && (
          <p className="text-sm text-muted-foreground">
            No requests to this Project&apos;s load balancers yet. Send one to a forwarding
            rule&apos;s local listener and it appears here.
          </p>
        )}
        {all.length > 0 && shown.length === 0 && (
          <p className="text-sm text-muted-foreground">No requests match the filters.</p>
        )}
        {shown.length > 0 && (
          <Table aria-label="Requests">
            <TableHeader>
              <TableRow>
                <TableHead>
                  <span className="sr-only">Details</span>
                </TableHead>
                <TableHead>Time</TableHead>
                <TableHead>Request</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Forwarding rule</TableHead>
                <TableHead>Route</TableHead>
                <TableHead>Backend</TableHead>
                <TableHead>Cache</TableHead>
                <TableHead className="text-right">Latency</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.slice(0, SHOWN).map((e, i) => {
                const key = `${e.time}#${shown.length - i}`
                const expanded = open === key
                const t = e.lb
                return (
                  <Fragment key={key}>
                    <TableRow data-testid="request">
                      <TableCell>
                        <Button
                          variant="ghost"
                          size="icon"
                          aria-expanded={expanded}
                          aria-label={`Details of ${e.method}`}
                          onClick={() => setOpen(expanded ? undefined : key)}
                        >
                          {expanded ? <ChevronDown aria-hidden /> : <ChevronRight aria-hidden />}
                        </Button>
                      </TableCell>
                      <TableCell className="font-mono whitespace-nowrap">{clock(e.time)}</TableCell>
                      <TableCell className="font-mono break-all">
                        {e.method.split(' ')[0]} {t.host}
                        {t.path}
                      </TableCell>
                      <TableCell>
                        <StatusBadge status={e.status} />
                      </TableCell>
                      <TableCell>
                        <RefLink link={e.resource} />
                      </TableCell>
                      <TableCell>{routeText(t)}</TableCell>
                      <TableCell>
                        {t.redirect ? (
                          <span className="font-mono break-all">→ {t.redirect}</span>
                        ) : (
                          <span className="flex flex-col">
                            <RefLink link={t.backend} />
                            {t.endpoint && (
                              <span className="font-mono text-xs break-all text-muted-foreground">
                                {t.endpoint}
                              </span>
                            )}
                          </span>
                        )}
                      </TableCell>
                      <TableCell>
                        <CacheBadge status={t.cacheStatus} />
                      </TableCell>
                      <TableCell className="text-right font-mono whitespace-nowrap">
                        {ms(e.latencyMs)}
                      </TableCell>
                    </TableRow>
                    {expanded && (
                      <TableRow>
                        <TableCell colSpan={9}>
                          <Details e={e} />
                        </TableCell>
                      </TableRow>
                    )}
                  </Fragment>
                )
              })}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  )
}
