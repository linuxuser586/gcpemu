import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Send } from 'lucide-react'
import { useId, useState } from 'react'

import { errorMessage } from '@/api/errors'
import { Field, fieldAria } from '@/components/resource/Field'
import { Button } from '@/components/ui/button'
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
import { Textarea } from '@/components/ui/textarea'
import { toast } from '@/lib/toast'

import { encodePayload } from '../secrets/api'
import { publish, type PubsubMessage } from './api'

/** parseAttributes reads key=value lines; values may hold "=" and commas. */
export function parseAttributes(text: string): Record<string, string> {
  const out: Record<string, string> = {}
  for (const line of text.split('\n')) {
    if (!line.trim()) continue
    const i = line.indexOf('=')
    out[(i < 0 ? line : line.slice(0, i)).trim()] = i < 0 ? '' : line.slice(i + 1).trim()
  }
  return out
}

/** messageError is the API's message for a message it would reject, if any. */
export function messageError(m: PubsubMessage): string | undefined {
  const attrs = m.attributes ?? {}
  if (!m.data && Object.keys(attrs).length === 0) {
    return 'One or more messages in the publish request is empty. Each message must contain either non-empty data, or at least one attribute.'
  }
  for (const k of Object.keys(attrs)) {
    if (!k) return 'Message attribute keys must be non-empty.'
    if (k.startsWith('goog')) {
      return `The attribute key '${k}' is reserved; keys may not begin with 'goog'.`
    }
  }
  return undefined
}

/**
 * Publish publishes one message to a topic: its data as UTF-8 text,
 * attributes and an ordering key.
 */
export function Publish({ project, topic }: { project: string; topic: string }) {
  const [open, setOpen] = useState(false)
  const [data, setData] = useState('')
  const [attributes, setAttributes] = useState('')
  const [orderingKey, setOrderingKey] = useState('')
  const [error, setError] = useState<string>()
  const id = useId()
  const qc = useQueryClient()
  const send = useMutation({
    meta: { toast: false },
    mutationFn: (m: PubsubMessage) => publish(project, topic, [m]),
    onSuccess: (res) => {
      void qc.invalidateQueries({ queryKey: ['pubsub'] })
      toast.success(`Published message ${res.messageIds[0] ?? ''}.`)
      setData('')
      setError(undefined)
    },
    onError: (e) => setError(errorMessage(e)),
  })
  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    const m: PubsubMessage = {}
    if (data) m.data = encodePayload(data)
    const attrs = parseAttributes(attributes)
    if (Object.keys(attrs).length > 0) m.attributes = attrs
    if (orderingKey) m.orderingKey = orderingKey
    const err = messageError(m)
    setError(err)
    if (!err) send.mutate(m)
  }
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        setOpen(o)
        setError(undefined)
      }}
    >
      <DialogTrigger asChild>
        <Button>
          <Send aria-hidden />
          Publish message
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogTitle>Publish message</DialogTitle>
        <DialogDescription>
          Sent to <span className="font-mono">topics.publish</span> on {topic}. The dialog stays
          open to publish more.
        </DialogDescription>
        <form className="flex flex-col gap-3" onSubmit={submit} noValidate>
          <Field id={`${id}-data`} label="Message body" hint="Sent as UTF-8.">
            <Textarea
              rows={4}
              spellCheck={false}
              className="font-mono"
              value={data}
              onChange={(e) => setData(e.target.value)}
              {...fieldAria(`${id}-data`, undefined, 'Sent')}
            />
          </Field>
          <Field id={`${id}-attrs`} label="Attributes" hint="key=value, one per line">
            <Textarea
              rows={2}
              spellCheck={false}
              className="font-mono"
              value={attributes}
              onChange={(e) => setAttributes(e.target.value)}
              {...fieldAria(`${id}-attrs`, undefined, 'key')}
            />
          </Field>
          <Field
            id={`${id}-key`}
            label="Ordering key"
            hint="Subscriptions with message ordering deliver messages of one key in order."
          >
            <Input
              spellCheck={false}
              autoComplete="off"
              value={orderingKey}
              onChange={(e) => setOrderingKey(e.target.value)}
              {...fieldAria(`${id}-key`, undefined, 'Subscriptions')}
            />
          </Field>
          {error && (
            <p role="alert" className="text-sm text-destructive">
              {error}
            </p>
          )}
          <DialogFooter>
            <DialogClose asChild>
              <Button type="button" variant="outline">
                Done
              </Button>
            </DialogClose>
            <Button type="submit" disabled={send.isPending}>
              Publish
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
