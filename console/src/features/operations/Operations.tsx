import { useQuery } from '@tanstack/react-query'
import { ChevronDown, ChevronRight, CircleCheck, CircleX, LoaderCircle } from 'lucide-react'
import { useEffect, useState } from 'react'
import { useSearchParams } from 'react-router'

import type { Operation } from '@/api/admin'
import { operationsQuery } from '@/api/queries'
import { QueryStatus } from '@/components/QueryStatus'
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
import { serviceName, SERVICES } from '@/lib/services'
import { useViewState } from '@/lib/viewState'

import { durationMs, formatDuration, matches, operationState, type OperationState } from './format'

/** TICK_MS is how often the durations of running Operations advance. */
export const TICK_MS = 1000

const STATE_LABELS: Record<OperationState, string> = {
  running: 'Running',
  succeeded: 'Succeeded',
  failed: 'Failed',
}

/** useNow is the current time, advancing every TICK_MS while active. */
function useNow(active: boolean): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!active) return
    const tick = () => setNow(Date.now())
    // Catch up at once: now has not advanced while nothing was running.
    const first = setTimeout(tick, 0)
    const t = setInterval(tick, TICK_MS)
    return () => {
      clearTimeout(first)
      clearInterval(t)
    }
  }, [active])
  return now
}

function StateBadge({ state }: { state: OperationState }) {
  switch (state) {
    case 'running':
      return (
        <Badge variant="warning">
          <LoaderCircle className="animate-spin motion-reduce:animate-none" aria-hidden />
          Running
        </Badge>
      )
    case 'failed':
      return (
        <Badge variant="destructive">
          <CircleX aria-hidden />
          Failed
        </Badge>
      )
    default:
      return (
        <Badge variant="success">
          <CircleCheck aria-hidden />
          Done
        </Badge>
      )
  }
}

/** Result is the resulting resource, or the error, of op. */
function Result({ op }: { op: Operation }) {
  if (op.error) {
    return (
      <span className="text-destructive">
        {op.error.code && <span className="font-mono">{op.error.code}: </span>}
        {op.error.message}
      </span>
    )
  }
  return <span className="font-mono break-all">{op.target}</span>
}

function formatTime(iso?: string): string {
  if (!iso) return ''
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}

function Details({ op }: { op: Operation }) {
  const rows: [string, React.ReactElement | string | undefined][] = [
    ['Name', <span className="font-mono break-all">{op.name}</span>],
    ['Project', op.project && <span className="font-mono">{op.project}</span>],
    ['Location', op.location && <span className="font-mono">{op.location}</span>],
    ['Status', op.status && <span className="font-mono">{op.status}</span>],
    ['Started', formatTime(op.startTime)],
    ['Ended', formatTime(op.endTime)],
    [op.error ? 'Error' : 'Resource', <Result op={op} />],
  ]
  return (
    <div className="flex flex-col gap-3 py-2">
      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
        {rows
          .filter(([, v]) => !!v)
          .map(([k, v]) => (
            <div key={k} className="contents">
              <dt className="text-muted-foreground">{k}</dt>
              <dd>{v}</dd>
            </div>
          ))}
      </dl>
      {op.operation && (
        <pre
          aria-label="Operation JSON"
          className="max-h-96 overflow-auto rounded-md bg-muted p-3 font-mono text-xs"
        >
          {JSON.stringify(op.operation, null, 2)}
        </pre>
      )}
    </div>
  )
}

function OperationRow({ op, now }: { op: Operation; now: number }) {
  const [open, setOpen] = useState(false)
  const state = operationState(op)
  const ms = durationMs(op, now)
  const id = `op-${op.service}-${op.name}`.replace(/[^\w-]/g, '-')
  return (
    <>
      <TableRow data-testid="operation" data-state={state}>
        <TableCell className="w-8 pr-0">
          <Button
            variant="ghost"
            size="icon"
            className="size-7"
            aria-expanded={open}
            aria-controls={`${id}-details`}
            aria-label={open ? 'Hide details' : 'Show details'}
            onClick={() => setOpen(!open)}
          >
            {open ? <ChevronDown /> : <ChevronRight />}
          </Button>
        </TableCell>
        <TableCell>
          <StateBadge state={state} />
        </TableCell>
        <TableCell title={op.service}>{serviceName(op.service)}</TableCell>
        <TableCell className="font-mono">{op.type || '—'}</TableCell>
        <TableCell className="max-w-md">
          <Result op={op} />
        </TableCell>
        <TableCell className="whitespace-nowrap" title={op.startTime}>
          {formatTime(op.startTime) || '—'}
        </TableCell>
        <TableCell className="text-right font-mono whitespace-nowrap tabular-nums">
          {ms === undefined ? '—' : formatDuration(ms)}
        </TableCell>
      </TableRow>
      {open && (
        <TableRow id={`${id}-details`} className="bg-muted/30">
          <TableCell />
          <TableCell colSpan={6}>
            <Details op={op} />
          </TableCell>
        </TableRow>
      )}
    </>
  )
}

/**
 * Operations lists every Service's Operations (FR-UI-012) with their live
 * status, duration and resulting resource or error. ?project= scopes it to
 * one Project; ?service=, ?status= and ?q= filter it.
 */
export function Operations() {
  const [view, setView] = useViewState()
  // The input owns the text while typing: a URL update is a navigation,
  // which lands after the next keystroke.
  const [text, setText] = useState(view.q ?? '')
  const [search, setSearch] = useSearchParams()
  const query = useQuery(operationsQuery(view.project))
  const service = search.get('service') ?? ''
  const status = (search.get('status') ?? '') as OperationState | ''
  const all = query.data ?? []
  const now = useNow(all.some((op) => !op.done))

  const setParam = (k: string, v: string) =>
    setSearch((prev) => {
      const next = new URLSearchParams(prev)
      if (v) next.set(k, v)
      else next.delete(k)
      return next
    })

  const order = SERVICES.map((s) => s.id)
  const services = [...new Set(all.map((op) => op.service))].sort(
    (a, b) => order.indexOf(a) - order.indexOf(b),
  )
  // A deep link may name a Service that has no Operations (yet).
  const serviceOptions = service && !services.includes(service) ? [...services, service] : services
  const counts: Record<OperationState, number> = { running: 0, succeeded: 0, failed: 0 }
  for (const op of all) counts[operationState(op)]++
  const shown = all.filter(
    (op) =>
      (!service || op.service === service) &&
      (!status || operationState(op) === status) &&
      matches(op, text),
  )

  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-2xl font-semibold">Operations</h1>
        <p className="text-sm text-muted-foreground">
          {view.project ? (
            <>
              Operations of Project <span className="font-mono">{view.project}</span>
            </>
          ) : (
            'Operations of every Project'
          )}
          , newest first.
        </p>
      </div>
      <Card>
        <form
          role="search"
          aria-label="Filter Operations"
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => e.preventDefault()}
        >
          <label htmlFor="ops-service" className="flex flex-col gap-1 text-sm">
            Service
            <NativeSelect
              id="ops-service"
              value={service}
              onChange={(e) => setParam('service', e.target.value)}
            >
              <option value="">All Services</option>
              {serviceOptions.map((id) => (
                <option key={id} value={id}>
                  {serviceName(id)}
                </option>
              ))}
            </NativeSelect>
          </label>
          <label htmlFor="ops-status" className="flex flex-col gap-1 text-sm">
            Status
            <NativeSelect
              id="ops-status"
              value={status}
              onChange={(e) => setParam('status', e.target.value)}
            >
              <option value="">Any status ({all.length})</option>
              {(Object.keys(STATE_LABELS) as OperationState[]).map((s) => (
                <option key={s} value={s}>
                  {STATE_LABELS[s]} ({counts[s]})
                </option>
              ))}
            </NativeSelect>
          </label>
          <label htmlFor="ops-filter" className="flex min-w-48 flex-1 flex-col gap-1 text-sm">
            Filter
            <Input
              id="ops-filter"
              type="search"
              placeholder="Name, type, resource or error"
              value={text}
              onChange={(e) => {
                setText(e.target.value)
                setView({ q: e.target.value })
              }}
            />
          </label>
        </form>
        <QueryStatus query={query} />
        {query.data && all.length === 0 && (
          <p className="text-sm text-muted-foreground">No Operations yet.</p>
        )}
        {query.data && all.length > 0 && shown.length === 0 && (
          <p className="text-sm text-muted-foreground">No Operations match the filters.</p>
        )}
        {shown.length > 0 && (
          <Table aria-label="Operations">
            <TableHeader>
              <TableRow>
                <TableHead>
                  <span className="sr-only">Details</span>
                </TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Service</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Resource or error</TableHead>
                <TableHead>Started</TableHead>
                <TableHead className="text-right">Duration</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((op) => (
                <OperationRow key={`${op.service}/${op.name}`} op={op} now={now} />
              ))}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  )
}
