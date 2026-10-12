import { useQuery } from '@tanstack/react-query'
import { ChevronDown, ChevronRight, Pause, Play } from 'lucide-react'
import { Fragment, useId, useState } from 'react'
import { useSearchParams } from 'react-router'

import type { RequestEntry } from '@/api/admin'
import { requestsQuery } from '@/api/queries'
import { QueryStatus } from '@/components/QueryStatus'
import { DetailList, Mono } from '@/components/resource/DetailList'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { useViewState } from '@/lib/viewState'

import { lastSeg } from '../lb/api'
import { inProject, isEgress, type Egress } from './api'
import { GatewayLink, NatTabs } from './NatLayout'

// The translation log (SRS 4.8.3): every internet egress attempt the
// Project's NAT egress gateways saw, read from the Request log, whose
// entries the events bridge appends as they happen (FR-UI-006). Each is
// translated (OK), dropped because no Cloud NAT covers the source, or
// answered by the offline sink. The filters are in the URL (?src=, ?dst=,
// ?outcome=), so that a log can be shared.

/** SHOWN caps the rows rendered, newest first. */
export const SHOWN = 500

export const OUTCOMES = {
  OK: 'Translated',
  DROPPED: 'Dropped',
  OFFLINE_SINK: 'Offline sink',
} as const

export type Outcome = keyof typeof OUTCOMES

/** Filters narrow the log: substrings of source and destination, an outcome. */
export interface Filters {
  src: string
  dst: string
  outcome: string
}

const endpoint = (ip: string, port: number) => `${ip}:${port}`

/** matches keeps the entries whose source and destination contain the filters. */
export function matches(e: Egress, f: Filters): boolean {
  const t = e.nat
  return (
    `${endpoint(t.srcIp, t.srcPort)} ${lastSeg(t.vm)}`.includes(f.src) &&
    endpoint(t.destIp, t.destPort).includes(f.dst) &&
    (!f.outcome || e.code === f.outcome)
  )
}

function clock(time: string) {
  const d = new Date(time)
  return `${d.toLocaleTimeString([], { hour12: false })}.${String(d.getMilliseconds()).padStart(3, '0')}`
}

export function OutcomeBadge({ code }: { code: string }) {
  const variant = code === 'OK' ? 'success' : code === 'DROPPED' ? 'destructive' : 'warning'
  return <Badge variant={variant}>{OUTCOMES[code as Outcome] ?? code}</Badge>
}

const short = (p?: string) => p?.replace(/^projects\/[^/]+\//, '')

/** Details shows the rest of one connection's trace. */
function Details({ e }: { e: Egress }) {
  const t = e.nat
  const mono = (v?: string) => v && <Mono>{v}</Mono>
  return (
    <DetailList
      label={`Connection ${endpoint(t.srcIp, t.srcPort)} to ${endpoint(t.destIp, t.destPort)}`}
      rows={[
        ['Time', mono(e.time)],
        ['Outcome', <OutcomeBadge key="o" code={e.code} />],
        ['Protocol', mono(e.protocol)],
        ['NAT gateway', <GatewayLink key="g" path={e.resource} />],
        ['Network', mono(short(t.network))],
        ['Subnetwork', mono(short(t.subnetwork))],
        ['Workload', mono(short(t.vm))],
        ['Source', mono(endpoint(t.srcIp, t.srcPort))],
        ['Destination', mono(endpoint(t.destIp, t.destPort))],
        ['NAT IP and port', t.natIp && mono(endpoint(t.natIp, t.natPort ?? 0))],
        ['Sink received', mono(t.request)],
      ]}
    />
  )
}

/** TranslationLogPage is the translation log of the chosen Project's NATs. */
export function TranslationLogPage() {
  const [view] = useViewState()
  const project = view.project ?? ''
  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-2xl font-semibold">Cloud NAT</h1>
        <p className="text-sm text-muted-foreground">
          Internet egress from the VPC networks of Project{' '}
          <span className="font-mono">{project}</span>
        </p>
      </div>
      <NatTabs />
      <TranslationLog project={project} />
    </div>
  )
}

/**
 * TranslationLog is the live log of the Project's egress, or of one NAT
 * gateway's (projects/P/regions/R/routers/ROUTER/nats/NAT).
 */
export function TranslationLog({ project, gateway }: { project: string; gateway?: string }) {
  const id = useId()
  const [search, setSearch] = useSearchParams()
  // The inputs lead the URL, which follows each keystroke.
  const [f, setF] = useState<Filters>({
    src: search.get('src') ?? '',
    dst: search.get('dst') ?? '',
    outcome: search.get('outcome') ?? '',
  })
  const query = useQuery(requestsQuery('nat'))
  // Paused keeps the entries seen when it was pressed.
  const [paused, setPaused] = useState<RequestEntry[]>()
  const [open, setOpen] = useState<string>()

  const setFilter = (k: keyof Filters, v: string) => {
    setF((prev) => ({ ...prev, [k]: v }))
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
  const all = entries
    .filter(isEgress)
    .filter((e) => (gateway ? e.resource === gateway : inProject(e, project)))
  const shown = all.filter((e) => matches(e, f)).reverse()
  const cols = gateway ? 7 : 8

  return (
    <Card aria-label="Translation log">
      <div className="flex flex-wrap items-end gap-3">
        <form
          role="search"
          aria-label="Filter connections"
          className="flex min-w-0 flex-1 flex-wrap items-end gap-3"
          onSubmit={(e) => e.preventDefault()}
        >
          <label htmlFor={`${id}-src`} className="flex min-w-40 flex-1 flex-col gap-1 text-sm">
            Source
            <Input
              id={`${id}-src`}
              type="search"
              placeholder="IP, port or workload"
              value={f.src}
              onChange={(e) => setFilter('src', e.target.value.trim())}
            />
          </label>
          <label htmlFor={`${id}-dst`} className="flex min-w-40 flex-1 flex-col gap-1 text-sm">
            Destination
            <Input
              id={`${id}-dst`}
              type="search"
              placeholder="IP or port"
              value={f.dst}
              onChange={(e) => setFilter('dst', e.target.value.trim())}
            />
          </label>
          <label htmlFor={`${id}-outcome`} className="flex flex-col gap-1 text-sm">
            Outcome
            <NativeSelect
              id={`${id}-outcome`}
              value={f.outcome}
              onChange={(e) => setFilter('outcome', e.target.value)}
            >
              <option value="">Any</option>
              {Object.entries(OUTCOMES).map(([code, label]) => (
                <option key={code} value={code}>
                  {label}
                </option>
              ))}
            </NativeSelect>
          </label>
        </form>
        <Button variant="outline" onClick={() => setPaused(paused ? undefined : entries)}>
          {paused ? <Play aria-hidden /> : <Pause aria-hidden />}
          {paused ? 'Resume' : 'Pause'}
        </Button>
      </div>
      <QueryStatus query={query} />
      <p className="text-sm text-muted-foreground">
        {paused ? 'Paused. ' : query.data ? 'Live. ' : ''}
        {query.data &&
          (shown.length > SHOWN
            ? `Showing the latest ${SHOWN} of ${shown.length} connections.`
            : `${shown.length} ${shown.length === 1 ? 'connection' : 'connections'}.`)}
      </p>
      {query.data && all.length === 0 && (
        <p className="text-sm text-muted-foreground">
          No egress {gateway ? 'through this NAT' : 'from this Project'} yet. Connections to the
          internet from workloads on its subnetworks, such as GKE nodes and pods, appear here as
          they are made.
        </p>
      )}
      {all.length > 0 && shown.length === 0 && (
        <p className="text-sm text-muted-foreground">No connections match the filters.</p>
      )}
      {shown.length > 0 && (
        <Table aria-label="Connections">
          <TableHeader>
            <TableRow>
              <TableHead>
                <span className="sr-only">Details</span>
              </TableHead>
              <TableHead>Time</TableHead>
              <TableHead>Outcome</TableHead>
              <TableHead>Source</TableHead>
              <TableHead>Destination</TableHead>
              <TableHead>NAT IP and port</TableHead>
              {!gateway && <TableHead>NAT gateway</TableHead>}
              <TableHead>Protocol</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {shown.slice(0, SHOWN).map((e, i) => {
              const key = `${e.time}#${shown.length - i}`
              const expanded = open === key
              const t = e.nat
              const label = `${endpoint(t.srcIp, t.srcPort)} to ${endpoint(t.destIp, t.destPort)}`
              return (
                <Fragment key={key}>
                  <TableRow data-testid="connection">
                    <TableCell>
                      <Button
                        variant="ghost"
                        size="icon"
                        aria-expanded={expanded}
                        aria-label={`Details of ${label}`}
                        onClick={() => setOpen(expanded ? undefined : key)}
                      >
                        {expanded ? <ChevronDown aria-hidden /> : <ChevronRight aria-hidden />}
                      </Button>
                    </TableCell>
                    <TableCell className="font-mono whitespace-nowrap">{clock(e.time)}</TableCell>
                    <TableCell>
                      <OutcomeBadge code={e.code} />
                    </TableCell>
                    <TableCell>
                      <span className="flex flex-col">
                        <span className="font-mono">{endpoint(t.srcIp, t.srcPort)}</span>
                        {t.vm && (
                          <span className="font-mono text-xs break-all text-muted-foreground">
                            {lastSeg(t.vm)}
                          </span>
                        )}
                      </span>
                    </TableCell>
                    <TableCell className="font-mono">{endpoint(t.destIp, t.destPort)}</TableCell>
                    <TableCell className="font-mono">
                      {t.natIp ? endpoint(t.natIp, t.natPort ?? 0) : '—'}
                    </TableCell>
                    {!gateway && (
                      <TableCell>
                        <GatewayLink path={e.resource} />
                      </TableCell>
                    )}
                    <TableCell className="font-mono">{e.protocol}</TableCell>
                  </TableRow>
                  {expanded && (
                    <TableRow>
                      <TableCell colSpan={cols}>
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
  )
}
