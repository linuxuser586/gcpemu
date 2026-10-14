import { useQuery } from '@tanstack/react-query'
import { ChevronDown, ChevronRight, Pause, Play, X } from 'lucide-react'
import { useMemo, useState } from 'react'
import { useSearchParams } from 'react-router'

import type { RequestEntry } from '@/api/admin'
import { ApiError } from '@/api/errors'
import { infoQuery, requestsQuery } from '@/api/queries'
import { QueryStatus } from '@/components/QueryStatus'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import { VirtualRows } from '@/components/VirtualRows'
import { serviceName, SERVICES } from '@/lib/services'
import { cn } from '@/lib/utils'

import {
  bodyText,
  FILTERS,
  matches,
  requestQuery,
  size,
  type Filters,
  type RequestBody,
} from './api'

// The Request log view (FR-UI-013): a live tail of every request the
// Instance logs, newest first. The events bridge appends entries as they
// happen (FR-UI-006) and keeps up to 100,000, whose rows are virtualized
// (FR-UI-022). The filters are in the URL, so that a view of the log can
// be shared. Opening an entry reads its request and response bodies,
// which the Instance keeps for API calls while the entry stays in its log.

const ROW_HEIGHT = 40
const GRID =
  'grid min-w-[64rem] grid-cols-[2.25rem_7.5rem_8rem_minmax(0,1.4fr)_minmax(0,1fr)_minmax(0,1fr)_7rem_5.5rem] items-center gap-3 px-3'

function clock(time?: string) {
  if (!time) return '—'
  const d = new Date(time)
  return `${d.toLocaleTimeString([], { hour12: false })}.${String(d.getMilliseconds()).padStart(3, '0')}`
}

const ORDER = SERVICES.map((s) => s.id)

/** rank orders Services as the sidebar does, any it does not list last. */
const rank = (id: string) => (ORDER.includes(id) ? ORDER.indexOf(id) : ORDER.length)

const ms = (v?: number) => (v === undefined ? '—' : `${v.toFixed(1)} ms`)

/** Server-side gRPC codes: UNKNOWN, DEADLINE_EXCEEDED, INTERNAL, UNAVAILABLE, DATA_LOSS. */
const SERVER_CODES = new Set([2, 4, 13, 14, 15])

/** StatusBadge shows an HTTP status, or the code of any other protocol. */
export function StatusBadge({ e }: { e: RequestEntry }) {
  const status = e.status ?? 0
  if (e.protocol === 'http') {
    const variant =
      status >= 500 || status === 0
        ? 'destructive'
        : status >= 400
          ? 'warning'
          : status >= 300
            ? 'outline'
            : 'success'
    return <Badge variant={variant}>{status || 'dropped'}</Badge>
  }
  const variant =
    status === 0
      ? 'success'
      : e.protocol === 'grpc' && SERVER_CODES.has(status)
        ? 'destructive'
        : 'warning'
  return <Badge variant={variant}>{e.code || status}</Badge>
}

function BodyView({ label, body, kept }: { label: string; body?: RequestBody; kept: boolean }) {
  let content
  if (!kept) {
    content = <p className="text-muted-foreground">Bodies are kept only for API calls.</p>
  } else if (!body) {
    content = <p className="text-muted-foreground">No body.</p>
  } else if (body.binary) {
    content = <p className="text-muted-foreground">Binary, {size(body.size)}; not shown.</p>
  } else {
    content = (
      <>
        {body.truncated && (
          <p className="text-muted-foreground">
            The first {size(body.text?.length ?? 0)} of {size(body.size)}.
          </p>
        )}
        <pre className="max-h-[32rem] overflow-auto rounded-md bg-muted p-3 font-mono text-xs break-all whitespace-pre-wrap">
          {bodyText(body)}
        </pre>
      </>
    )
  }
  return (
    <section aria-label={label} className="flex min-w-0 flex-col gap-2 text-sm">
      <h3 className="font-medium">
        {label}
        {body && (
          <span className="ml-2 font-normal text-muted-foreground">
            {[body.contentType, size(body.size)].filter(Boolean).join(', ')}
          </span>
        )}
      </h3>
      {content}
    </section>
  )
}

/** Details shows one entry's fields and bodies. */
function Details({ e, onClose }: { e: RequestEntry; onClose: () => void }) {
  const query = useQuery({ ...requestQuery(e.id ?? 0), enabled: e.id !== undefined })
  const gone = query.error instanceof ApiError && query.error.httpStatus === 404
  const kept = e.protocol === 'http' || e.protocol === 'grpc'
  const mono = (v?: string) => v && <Mono>{v}</Mono>
  return (
    <Card id="request-details" aria-label={`Details of ${e.method}`}>
      <div className="flex items-start justify-between gap-2">
        <h2 className="font-mono text-lg font-semibold break-all">{e.method}</h2>
        <Button variant="ghost" size="icon" aria-label="Close details" onClick={onClose}>
          <X aria-hidden />
        </Button>
      </div>
      <DetailList
        rows={[
          ['Time', mono(e.time)],
          ['Service', e.service && serviceName(e.service)],
          ['Protocol', e.protocol],
          ['Resource', mono(e.resource)],
          ['Principal', mono(e.principal)],
          ['Status', <StatusBadge key="s" e={e} />],
          ['Code', e.protocol === 'http' ? undefined : mono(e.code)],
          ['Latency', ms(e.latencyMs)],
        ]}
      />
      {e.lb && <JsonView label="Load balancer trace" value={e.lb} />}
      {e.nat && <JsonView label="NAT trace" value={e.nat} />}
      {kept && gone ? (
        <p className="text-sm text-muted-foreground">
          The Instance no longer keeps this request&apos;s bodies.
        </p>
      ) : kept && !query.data ? (
        <QueryStatus query={query} rows={2} />
      ) : (
        <div className="grid gap-4 lg:grid-cols-2">
          <BodyView label="Request body" body={query.data?.requestBody} kept={kept} />
          <BodyView label="Response body" body={query.data?.responseBody} kept={kept} />
        </div>
      )}
    </Card>
  )
}

/** TEXT_FILTERS label the text filters and give an example of each. */
const TEXT_FILTERS = [
  ['method', 'Method', 'e.g. POST /storage'],
  ['principal', 'Principal', 'e.g. serviceAccount:'],
  ['status', 'Status', 'e.g. 404, 5xx, NOT_FOUND'],
  ['resource', 'Resource', 'e.g. projects/my-project'],
] as const

/** RequestLog is the live, filterable tail of the Instance's Request log. */
export function RequestLog() {
  const [search, setSearch] = useSearchParams()
  // The inputs lead the URL, which follows each keystroke.
  const [filters, setFilters] = useState<Filters>(() => ({
    service: search.get('service') ?? '',
    method: search.get('method') ?? '',
    principal: search.get('principal') ?? '',
    status: search.get('status') ?? '',
    resource: search.get('resource') ?? '',
  }))
  const query = useQuery(requestsQuery())
  const { data: info } = useQuery(infoQuery())
  // Paused keeps the entries seen when it was pressed.
  const [paused, setPaused] = useState<RequestEntry[]>()
  const [open, setOpen] = useState<RequestEntry>()

  const setFilter = (k: keyof Filters, v: string) => {
    setFilters((f) => ({ ...f, [k]: v }))
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

  const entries = paused ?? query.data
  const shown = useMemo(() => {
    const out: RequestEntry[] = []
    const all = entries ?? []
    for (let i = all.length - 1; i >= 0; i--) if (matches(all[i]!, filters)) out.push(all[i]!)
    return out
  }, [entries, filters])
  const total = entries?.length ?? 0
  const filtered = FILTERS.some((k) => filters[k])
  const services = useMemo(
    () =>
      // The enabled Services, and any other the log names, such as an Instance-wide one.
      [
        ...new Set([
          ...(info?.services ?? []),
          ...(entries ?? []).map((e) => e.service ?? ''),
          filters.service,
        ]),
      ]
        .filter(Boolean)
        .sort((a, b) => rank(a) - rank(b)),
    [info, entries, filters.service],
  )

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Request log</h1>
          <p className="text-sm text-muted-foreground">
            Every request this Instance has received, newest first, for every Project.
          </p>
        </div>
        <Button variant="outline" onClick={() => setPaused(paused ? undefined : (entries ?? []))}>
          {paused ? <Play aria-hidden /> : <Pause aria-hidden />}
          {paused ? 'Resume' : 'Pause'}
        </Button>
      </div>
      <Card>
        <form
          role="search"
          aria-label="Filter requests"
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => e.preventDefault()}
        >
          <label htmlFor="requests-service" className="flex flex-col gap-1 text-sm">
            Service
            <NativeSelect
              id="requests-service"
              value={filters.service}
              onChange={(e) => setFilter('service', e.target.value)}
            >
              <option value="">All Services</option>
              {services.map((id) => (
                <option key={id} value={id}>
                  {serviceName(id)}
                </option>
              ))}
            </NativeSelect>
          </label>
          {TEXT_FILTERS.map(([k, label, placeholder]) => (
            <label
              key={k}
              htmlFor={`requests-${k}`}
              className="flex min-w-40 flex-1 flex-col gap-1 text-sm"
            >
              {label}
              <Input
                id={`requests-${k}`}
                type="search"
                placeholder={placeholder}
                value={filters[k]}
                onChange={(e) => setFilter(k, e.target.value.trim())}
              />
            </label>
          ))}
        </form>
        <QueryStatus query={query} />
        {entries && (
          <p className="text-sm text-muted-foreground">
            {paused ? 'Paused. ' : 'Live. '}
            {filtered
              ? `${shown.length} of ${total} ${total === 1 ? 'request' : 'requests'} match.`
              : `${total} ${total === 1 ? 'request' : 'requests'}.`}
          </p>
        )}
        {entries && total === 0 && (
          <p className="text-sm text-muted-foreground">
            No requests yet. Call any API of this Instance and it appears here.
          </p>
        )}
        {total > 0 && shown.length === 0 && (
          <p className="text-sm text-muted-foreground">No requests match the filters.</p>
        )}
        {shown.length > 0 && (
          <VirtualRows
            label="Requests"
            rows={shown}
            rowHeight={ROW_HEIGHT}
            height={560}
            header={
              <div role="row" className={cn(GRID, 'h-10 text-sm font-medium')}>
                <span role="columnheader">
                  <span className="sr-only">Details</span>
                </span>
                <span role="columnheader">Time</span>
                <span role="columnheader">Service</span>
                <span role="columnheader">Method</span>
                <span role="columnheader">Principal</span>
                <span role="columnheader">Resource</span>
                <span role="columnheader">Status</span>
                <span role="columnheader" className="text-right">
                  Latency
                </span>
              </div>
            }
            render={(e, i) => {
              const expanded = open !== undefined && open.id === e.id
              return (
                <div
                  role="row"
                  aria-rowindex={i + 2}
                  aria-selected={expanded}
                  data-testid="request"
                  className={cn(
                    GRID,
                    'h-full border-b text-sm hover:bg-muted/50',
                    expanded && 'bg-accent',
                  )}
                >
                  <span role="cell">
                    <Button
                      variant="ghost"
                      size="icon"
                      aria-expanded={expanded}
                      aria-controls={expanded ? 'request-details' : undefined}
                      aria-label={`Details of ${e.method}`}
                      onClick={() => setOpen(expanded ? undefined : e)}
                    >
                      {expanded ? <ChevronDown aria-hidden /> : <ChevronRight aria-hidden />}
                    </Button>
                  </span>
                  <span role="cell" className="font-mono whitespace-nowrap">
                    {clock(e.time)}
                  </span>
                  <span role="cell" className="truncate" title={e.service}>
                    {e.service ? serviceName(e.service) : '—'}
                  </span>
                  <span role="cell" className="truncate font-mono" title={e.method}>
                    {e.method}
                  </span>
                  <span role="cell" className="truncate font-mono" title={e.principal}>
                    {e.principal || '—'}
                  </span>
                  <span role="cell" className="truncate font-mono" title={e.resource}>
                    {e.resource || '—'}
                  </span>
                  <span role="cell" className="truncate">
                    <StatusBadge e={e} />
                  </span>
                  <span role="cell" className="text-right font-mono whitespace-nowrap">
                    {ms(e.latencyMs)}
                  </span>
                </div>
              )
            }}
          />
        )}
      </Card>
      {open && <Details key={open.id} e={open} onClose={() => setOpen(undefined)} />}
    </div>
  )
}
