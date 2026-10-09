import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Check, Download, Eraser, Eye, History } from 'lucide-react'
import { useId, useState } from 'react'

import { errorMessage } from '@/api/errors'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { Field, fieldAria } from '@/components/resource/Field'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogTitle,
  DialogTrigger,
} from '@/components/ui/dialog'
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
import { toast } from '@/lib/toast'

import { time } from '../gcs/format'
import {
  acknowledge,
  lastSegment,
  nack,
  pull,
  seek,
  snapshotsQuery,
  statsQuery,
  type ReceivedMessage,
  type Subscription,
} from './api'
import { messageData, pairs } from './format'
import { useProject } from './PubSubLayout'
import { useSubscription } from './SubscriptionPage'

interface Pulled {
  rm: ReceivedMessage
  /** peeked messages were returned to the subscription and cannot be acked. */
  peeked: boolean
  /** leaseEnd is when an unacked lease runs out (ms since the epoch). */
  leaseEnd: number
}

const plural = (n: number, one: string) => `${n} ${one}${n === 1 ? '' : 's'}`

/**
 * Messages pulls messages and acks them, peeks at messages without acking
 * them (they are nacked at once, which counts as a delivery attempt),
 * seeks the subscription to a time or snapshot and purges it.
 */
export function Messages() {
  const s = useSubscription()
  const project = useProject()
  const id = lastSegment(s.name)
  const qc = useQueryClient()
  const [max, setMax] = useState('10')
  const [error, setError] = useState<string>()
  const [pulled, setPulled] = useState<Pulled[]>([])
  const maxId = useId()

  const fetch = useMutation({
    meta: { toast: false },
    mutationFn: async (peek: boolean) => {
      const n = Number(max)
      if (!/^\d+$/.test(max) || n < 1) {
        throw new Error('The value for max_messages must be greater than 0.')
      }
      const got = await pull(project, id, Math.min(n, 1000))
      if (peek && got.length > 0)
        await nack(
          project,
          id,
          got.map((m) => m.ackId),
        )
      return { got, peek }
    },
    onSuccess: ({ got, peek }) => {
      setError(undefined)
      const leaseEnd = Date.now() + (s.ackDeadlineSeconds ?? 10) * 1000
      setPulled(got.map((rm) => ({ rm, peeked: peek, leaseEnd })))
      if (got.length === 0) toast.success('No messages to pull.')
      else toast.success(`${peek ? 'Peeked at' : 'Pulled'} ${plural(got.length, 'message')}.`)
    },
    onError: (e) => setError(errorMessage(e)),
  })
  const ack = useMutation({
    mutationFn: (ids: string[]) => acknowledge(project, id, ids),
    onSuccess: (_, ids) => {
      void qc.invalidateQueries({ queryKey: ['pubsub', 'stats'] })
      setPulled((ps) => ps.filter((p) => !ids.includes(p.rm.ackId)))
      toast.success(`Acknowledged ${plural(ids.length, 'message')}.`)
    },
  })
  const purge = useMutation({
    mutationFn: async () => {
      const { now } = await qc.fetchQuery({ ...statsQuery(project), staleTime: 0 })
      await seek(project, id, { time: now })
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['pubsub'] })
      setPulled([])
      toast.success(`Purged subscription ${id}.`)
    },
  })
  const leased = pulled.filter((p) => !p.peeked)

  return (
    <Card>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <CardTitle>Messages</CardTitle>
        <div className="flex flex-wrap gap-2">
          <Seek s={s} onDone={() => setPulled([])} />
          <ConfirmDialog
            trigger={
              <Button variant="outline">
                <Eraser aria-hidden />
                Purge messages
              </Button>
            }
            title={`Purge subscription ${id}?`}
            description="Every message published so far is acknowledged, by seeking the subscription to now. Retained messages can still be replayed by seeking back."
            confirmLabel="Purge messages"
            onConfirm={() => purge.mutateAsync()}
          />
        </div>
      </div>
      <form
        className="flex flex-wrap items-end gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          fetch.mutate(false)
        }}
        noValidate
      >
        <Field id={maxId} label="Messages at most">
          <Input
            className="w-28"
            inputMode="numeric"
            value={max}
            onChange={(e) => setMax(e.target.value)}
            {...fieldAria(maxId, error)}
          />
        </Field>
        <Button type="submit" disabled={fetch.isPending}>
          <Download aria-hidden />
          Pull
        </Button>
        <Button
          type="button"
          variant="outline"
          disabled={fetch.isPending}
          onClick={() => fetch.mutate(true)}
        >
          <Eye aria-hidden />
          Peek
        </Button>
        {leased.length > 1 && (
          <Button
            type="button"
            variant="outline"
            disabled={ack.isPending}
            onClick={() => ack.mutate(leased.map((p) => p.rm.ackId))}
          >
            <Check aria-hidden />
            Ack all
          </Button>
        )}
      </form>
      <p className="text-xs text-muted-foreground">
        Pull leases messages for the acknowledgement deadline ({s.ackDeadlineSeconds ?? 10} s):
        unacked ones are redelivered after it. Peek returns them at once without acking them; both
        count as a delivery attempt.
      </p>
      {error && (
        <p id={`${maxId}-error`} role="alert" className="text-sm text-destructive">
          {error}
        </p>
      )}
      {pulled.length > 0 && <PulledTable pulled={pulled} onAck={(ackId) => ack.mutate([ackId])} />}
    </Card>
  )
}

function PulledTable({ pulled, onAck }: { pulled: Pulled[]; onAck: (ackId: string) => void }) {
  return (
    <Table aria-label="Pulled messages">
      <TableHeader>
        <TableRow>
          <TableHead>Message ID</TableHead>
          <TableHead>Published</TableHead>
          <TableHead>Data</TableHead>
          <TableHead>Attributes</TableHead>
          <TableHead>Ordering key</TableHead>
          <TableHead>Attempt</TableHead>
          <TableHead>
            <span className="sr-only">Actions</span>
          </TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {pulled.map(({ rm, peeked, leaseEnd }) => {
          const m = rm.message
          const data = messageData(m)
          return (
            <TableRow key={rm.ackId} data-testid="message">
              <TableCell className="font-mono text-xs">{m.messageId}</TableCell>
              <TableCell>{time(m.publishTime)}</TableCell>
              <TableCell className="max-w-md">
                <pre className="font-mono text-xs break-all whitespace-pre-wrap">{data.text}</pre>
                {data.binary && <Badge variant="outline">base64</Badge>}
              </TableCell>
              <TableCell className="font-mono text-xs">{pairs(m.attributes)}</TableCell>
              <TableCell className="font-mono text-xs">{m.orderingKey}</TableCell>
              <TableCell>{rm.deliveryAttempt}</TableCell>
              <TableCell>
                {peeked ? (
                  <Badge>Peeked</Badge>
                ) : (
                  <div className="flex flex-col items-end gap-1">
                    <Button
                      size="sm"
                      variant="outline"
                      aria-label={`Ack message ${m.messageId ?? ''}`}
                      onClick={() => onAck(rm.ackId)}
                    >
                      Ack
                    </Button>
                    <span className="text-xs whitespace-nowrap text-muted-foreground">
                      Lease ends {new Date(leaseEnd).toLocaleTimeString()}
                    </span>
                  </div>
                )}
              </TableCell>
            </TableRow>
          )
        })}
      </TableBody>
    </Table>
  )
}

/** localNow is now as a datetime-local input's value. */
function localNow(): string {
  const d = new Date()
  d.setMinutes(d.getMinutes() - d.getTimezoneOffset())
  return d.toISOString().slice(0, 19)
}

/**
 * Seek moves the subscription to a time, acking what was published
 * before it and redelivering retained messages published after, or to a
 * snapshot of its topic.
 */
function Seek({ s, onDone }: { s: Subscription; onDone: () => void }) {
  const project = useProject()
  const id = lastSegment(s.name)
  const [open, setOpen] = useState(false)
  const [target, setTarget] = useState<'time' | 'snapshot'>('time')
  const [when, setWhen] = useState('')
  const [snapshot, setSnapshot] = useState('')
  const [error, setError] = useState<string>()
  const fid = useId()
  const qc = useQueryClient()
  const snapshots = useQuery({ ...snapshotsQuery(project), enabled: open })
  const mine = (snapshots.data ?? []).filter((sn) => sn.topic === s.topic)
  const go = useMutation({
    meta: { toast: false },
    mutationFn: () => {
      if (target === 'snapshot') {
        if (!snapshot)
          throw new Error('One of the fields time or snapshot must be set in the SeekRequest.')
        return seek(project, id, { snapshot })
      }
      const t = new Date(when)
      if (!when || Number.isNaN(t.getTime())) {
        throw new Error('One of the fields time or snapshot must be set in the SeekRequest.')
      }
      return seek(project, id, { time: t.toISOString() })
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['pubsub'] })
      toast.success(
        target === 'snapshot'
          ? `Seeked ${id} to snapshot ${lastSegment(snapshot)}.`
          : `Seeked ${id} to ${new Date(when).toLocaleString()}.`,
      )
      onDone()
      setOpen(false)
    },
    onError: (e) => setError(errorMessage(e)),
  })
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        setOpen(o)
        setError(undefined)
        if (o) setWhen(localNow())
      }}
    >
      <DialogTrigger asChild>
        <Button variant="outline">
          <History aria-hidden />
          Seek
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogTitle>Seek {id}</DialogTitle>
        <DialogDescription>
          Messages published before the time are acknowledged; those published after it are
          delivered again if the subscription or its topic retained them.
        </DialogDescription>
        <form
          className="flex flex-col gap-3"
          noValidate
          onSubmit={(e) => {
            e.preventDefault()
            setError(undefined)
            go.mutate()
          }}
        >
          <Field id={`${fid}-target`} label="Seek to">
            <NativeSelect
              {...fieldAria(`${fid}-target`)}
              value={target}
              onChange={(e) => setTarget(e.target.value as 'time' | 'snapshot')}
            >
              <option value="time">A time</option>
              <option value="snapshot">A snapshot</option>
            </NativeSelect>
          </Field>
          {target === 'time' ? (
            <Field id={`${fid}-time`} label="Time" hint="Your local time.">
              <Input
                type="datetime-local"
                step={1}
                value={when}
                onChange={(e) => setWhen(e.target.value)}
                {...fieldAria(`${fid}-time`, undefined, 'Your')}
              />
            </Field>
          ) : (
            <Field
              id={`${fid}-snapshot`}
              label="Snapshot"
              hint="Snapshots of the subscription's topic."
            >
              <NativeSelect
                {...fieldAria(`${fid}-snapshot`, undefined, 'Snapshots')}
                value={snapshot}
                onChange={(e) => setSnapshot(e.target.value)}
              >
                <option value="">
                  {mine.length ? 'Choose a snapshot' : 'No snapshots of the topic'}
                </option>
                {mine.map((sn) => (
                  <option key={sn.name} value={sn.name}>
                    {lastSegment(sn.name)}
                  </option>
                ))}
              </NativeSelect>
            </Field>
          )}
          {target === 'snapshot' && <QueryStatus query={snapshots} rows={1} />}
          {error && (
            <p role="alert" className="text-sm text-destructive">
              {error}
            </p>
          )}
          <DialogFooter>
            <DialogClose asChild>
              <Button type="button" variant="outline">
                Cancel
              </Button>
            </DialogClose>
            <Button type="submit" disabled={go.isPending}>
              Seek
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
