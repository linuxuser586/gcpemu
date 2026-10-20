import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Ban, RotateCcw } from 'lucide-react'
import { useEffect, useRef } from 'react'
import { useParams, useSearchParams } from 'react-router'

import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { toast } from '@/lib/toast'
import { cn } from '@/lib/utils'

import { time } from '../gcs/format'
import {
  accountLabel,
  buildQuery,
  buildRef,
  cancelBuild,
  duration,
  isActive,
  logQuery,
  retryBuild,
  sourceLabel,
  stepLog,
  type Build,
  type BuildRef,
} from './api'
import { buildPath, listPath, StatusBadge } from './BuildLayout'

/** useBuildRef is the Build the route names, as its resource name does. */
export function useBuildRef(): BuildRef {
  const { project = '', location = '', id = '' } = useParams()
  return { project, location, id }
}

/**
 * BuildPage is one Build: its status and details, its steps with their
 * status and timing, and its log, all step or one (?step=), read again
 * while it runs. Cancel stops a running Build; retry starts a new one
 * from it.
 */
export function BuildPage() {
  const ref = useBuildRef()
  const query = useQuery(buildQuery(ref))
  const b = query.data
  const qc = useQueryClient()
  const navigate = useCarriedNavigate()
  const cancel = useMutation({
    mutationFn: () => cancelBuild(ref),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['build'] })
      toast.success(`Cancelled Build ${ref.id}.`)
    },
  })
  const retry = useMutation({
    mutationFn: () => retryBuild(ref),
    onSuccess: (op) => {
      void qc.invalidateQueries({ queryKey: ['build'] })
      const nb = op.metadata?.build
      toast.success(`Retried Build ${ref.id}${nb ? ` as ${nb.id}` : ''}.`)
      if (nb) navigate(buildPath(buildRef(nb)))
    },
  })

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to={listPath(ref.location)} className="underline-offset-4 hover:underline">
              Builds
            </Link>{' '}
            / <span className="font-mono">{ref.location}</span>
          </p>
          <h1 className="text-2xl font-semibold">
            <span className="font-mono">{ref.id}</span>
          </h1>
        </div>
        {b && (
          <div className="flex flex-wrap items-center gap-2">
            <span data-testid="build-status">
              <StatusBadge status={b.status} />
            </span>
            {isActive(b.status) ? (
              <ConfirmDialog
                trigger={
                  <Button variant="outline">
                    <Ban aria-hidden />
                    Cancel Build
                  </Button>
                }
                title={`Cancel Build ${ref.id}?`}
                description="Its running step is stopped and the steps after it do not run."
                confirmLabel="Cancel Build"
                onConfirm={() => cancel.mutateAsync()}
              />
            ) : (
              <Button variant="outline" disabled={retry.isPending} onClick={() => retry.mutate()}>
                <RotateCcw aria-hidden />
                Retry Build
              </Button>
            )}
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {b && (
        <>
          <Details b={b} />
          <Steps b={b} />
          <Card>
            <CardTitle>Build resource</CardTitle>
            <JsonView value={b} label="Build JSON" />
          </Card>
        </>
      )}
    </div>
  )
}

function Details({ b }: { b: Build }) {
  return (
    <Card>
      <CardTitle>Details</CardTitle>
      <DetailList
        label="Build details"
        rows={[
          ['Name', <Mono key="n">{b.name}</Mono>],
          ['Status detail', b.statusDetail],
          [
            'Failure',
            b.failureInfo?.detail &&
              `${b.failureInfo.type ? `${b.failureInfo.type}: ` : ''}${b.failureInfo.detail}`,
          ],
          ['Duration', duration(b.startTime, b.finishTime)],
          ['Source', <Mono key="s">{sourceLabel(b.source)}</Mono>],
          ['Build trigger', b.buildTriggerId && <Mono key="t">{b.buildTriggerId}</Mono>],
          ['Service account', <Mono key="a">{accountLabel(b.serviceAccount)}</Mono>],
          ['Logs bucket', b.logsBucket && <Mono key="l">{b.logsBucket}</Mono>],
          ['Images', b.images?.length ? <Mono key="i">{b.images.join(', ')}</Mono> : undefined],
          ['Created', time(b.createTime)],
          ['Started', time(b.startTime)],
          ['Finished', time(b.finishTime)],
        ]}
      />
    </Card>
  )
}

/** Steps lists a Build's steps and shows the log of all of them or of one. */
function Steps({ b }: { b: Build }) {
  const [search, setSearch] = useSearchParams()
  const steps = b.steps ?? []
  const raw = search.get('step')
  const step =
    raw !== null && Number.isInteger(Number(raw)) && steps[Number(raw)] ? Number(raw) : -1
  const running = isActive(b.status)
  const log = useQuery(logQuery(b))
  const text = step < 0 ? (log.data ?? '') : stepLog(log.data ?? '', step, steps[step]?.id)
  const pre = useRef<HTMLPreElement>(null)
  useEffect(() => {
    if (running && pre.current) pre.current.scrollTop = pre.current.scrollHeight
  }, [log.data, running])
  const show = (i: number) =>
    setSearch(
      (prev) => {
        const next = new URLSearchParams(prev)
        if (i < 0) next.delete('step')
        else next.set('step', String(i))
        return next
      },
      { replace: true },
    )

  return (
    <Card>
      <CardTitle>Steps</CardTitle>
      {steps.length === 0 ? (
        <p className="text-sm text-muted-foreground">This Build has no steps.</p>
      ) : (
        <Table aria-label="Steps">
          <TableHeader>
            <TableRow>
              <TableHead>Step</TableHead>
              <TableHead>ID</TableHead>
              <TableHead>Image</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Duration</TableHead>
              <TableHead>
                <span className="sr-only">Log</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {steps.map((s, i) => (
              <TableRow
                key={i}
                data-testid="step"
                aria-selected={step === i}
                className={cn(step === i && 'bg-accent')}
              >
                <TableCell className="font-mono">{i}</TableCell>
                <TableCell className="font-mono">{s.id}</TableCell>
                <TableCell className="font-mono text-xs break-all">{s.name}</TableCell>
                <TableCell>
                  <StatusBadge status={s.status} />
                </TableCell>
                <TableCell>{duration(s.timing?.startTime, s.timing?.endTime)}</TableCell>
                <TableCell className="text-right">
                  <Button
                    size="sm"
                    variant="outline"
                    aria-label={`Show the log of step ${i}`}
                    aria-pressed={step === i}
                    onClick={() => show(i)}
                  >
                    Log
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h3 className="text-sm font-medium">
          {step < 0
            ? 'Build log'
            : `Log of step ${step}${steps[step]?.id ? ` (${steps[step].id})` : ''}`}
          {running && <span className="ml-2 text-muted-foreground">(updating)</span>}
        </h3>
        {step >= 0 && (
          <Button size="sm" variant="outline" onClick={() => show(-1)}>
            Show the whole log
          </Button>
        )}
      </div>
      <QueryStatus query={log} rows={2} />
      {log.data !== undefined && (
        <pre
          ref={pre}
          // Focusable so that the log scrolls from the keyboard.
          // eslint-disable-next-line jsx-a11y/no-noninteractive-tabindex
          tabIndex={0}
          aria-label={step < 0 ? 'Build log' : `Log of step ${step}`}
          className="max-h-[32rem] overflow-auto rounded-md bg-muted p-3 font-mono text-xs whitespace-pre-wrap outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50"
        >
          {text || (running ? 'No output yet.' : 'No output.')}
        </pre>
      )}
    </Card>
  )
}
