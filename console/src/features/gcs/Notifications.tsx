import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Plus, Trash2 } from 'lucide-react'
import { useForm } from 'react-hook-form'
import { useParams } from 'react-router'
import { z } from 'zod'

import { gcpFetch } from '@/api/fetch'
import { infoQuery } from '@/api/queries'
import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Field, fieldAria } from '@/components/resource/Field'
import { ResourceEditor, type Body } from '@/components/resource/ResourceEditor'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
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
import { Textarea } from '@/components/ui/textarea'
import { formatLabels, parseLabels } from '@/lib/labels'
import { toast } from '@/lib/toast'
import { useViewState } from '@/lib/viewState'

import {
  createNotification,
  deleteNotification,
  EVENT_TYPES,
  notificationsQuery,
  PAYLOAD_FORMATS,
  shortTopic,
  type Notification,
} from './api'
import { bucketPath, useBucket } from './GcsLayout'

// Pub/Sub notification configs (FR-GCS-007, FR-UI-011). The API creates,
// reads and deletes them; there is no update, so a change is a new
// config and the old one deleted.

const notifPath = (bucket: string, id = '') =>
  bucketPath(bucket, `notifications${id ? `/${encodeURIComponent(id)}` : ''}`)

const events = (n: Notification) =>
  n.event_types?.length ? n.event_types.join(', ') : 'All events'

/** Notifications lists a bucket's notification configs. */
export function Notifications() {
  const bucket = useBucket()
  const query = useQuery(notificationsQuery(bucket))
  const list = query.data ?? []
  return (
    <Card>
      <div className="flex items-start justify-between gap-2">
        <CardTitle>Pub/Sub notifications</CardTitle>
        <Button size="sm" asChild>
          <Link to={notifPath(bucket, 'create')}>
            <Plus aria-hidden />
            Create notification
          </Link>
        </Button>
      </div>
      <QueryStatus query={query} />
      {query.data && list.length === 0 && (
        <p className="text-sm text-muted-foreground">This bucket sends no notifications.</p>
      )}
      {list.length > 0 && (
        <Table aria-label="Notifications">
          <TableHeader>
            <TableRow>
              <TableHead>ID</TableHead>
              <TableHead>Topic</TableHead>
              <TableHead>Events</TableHead>
              <TableHead>Payload</TableHead>
              <TableHead>Object prefix</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {list.map((n) => (
              <TableRow key={n.id} data-testid="notification">
                <TableCell>
                  <Link
                    to={notifPath(bucket, n.id)}
                    className="font-mono text-primary underline-offset-4 hover:underline"
                  >
                    {n.id}
                  </Link>
                </TableCell>
                <TableCell className="font-mono">{shortTopic(n.topic)}</TableCell>
                <TableCell>{events(n)}</TableCell>
                <TableCell className="font-mono">{n.payload_format}</TableCell>
                <TableCell className="font-mono">{n.object_name_prefix ?? ''}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}

/** NotificationPage is one notification config. */
export function NotificationPage() {
  const bucket = useBucket()
  const { id = '' } = useParams()
  const query = useQuery(notificationsQuery(bucket))
  const n = query.data?.find((x) => x.id === id)
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const del = useMutation({
    mutationFn: () => deleteNotification(bucket, id),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['gcs', 'notifications', bucket] })
      toast.success(`Deleted notification ${id}.`)
      navigate(notifPath(bucket))
    },
  })
  if (!query.data) return <QueryStatus query={query} />
  if (!n) {
    return (
      <Card role="status">
        <p className="text-sm text-muted-foreground">
          Bucket {bucket} has no notification <span className="font-mono">{id}</span>.
        </p>
      </Card>
    )
  }
  const attrs = Object.entries(n.custom_attributes ?? {})
  return (
    <Card>
      <div className="flex items-start justify-between gap-2">
        <CardTitle>
          Notification <span className="font-mono">{n.id}</span>
        </CardTitle>
        <ConfirmDialog
          trigger={
            <Button variant="outline" size="sm">
              <Trash2 aria-hidden />
              Delete
            </Button>
          }
          title={`Delete notification ${n.id}?`}
          description="The bucket stops publishing to its topic."
          confirmLabel="Delete notification"
          onConfirm={() => del.mutateAsync()}
        />
      </div>
      <DetailList
        label="Notification details"
        rows={[
          ['Topic', <Mono>{shortTopic(n.topic)}</Mono>],
          ['Events', events(n)],
          ['Payload format', n.payload_format],
          ['Object name prefix', n.object_name_prefix && <Mono>{n.object_name_prefix}</Mono>],
          [
            'Custom attributes',
            attrs.length > 0 && <Mono>{attrs.map(([k, v]) => `${k}=${v}`).join(', ')}</Mono>,
          ],
        ]}
      />
      <JsonView value={n} label="Notification JSON" />
    </Card>
  )
}

// ---- create ----

const TOPIC = /^projects\/[^/]+\/topics\/[^/]+$/

const schema = z.object({
  topic: z
    .string()
    .trim()
    .superRefine((v, ctx) => {
      if (!v) ctx.addIssue({ code: 'custom', message: 'Required' })
      else if (!TOPIC.test(shortTopic(v))) {
        ctx.addIssue({ code: 'custom', message: `Invalid Cloud Pub/Sub topic name: ${v}` })
      }
    }),
  eventTypes: z.array(z.string()),
  payloadFormat: z.string(),
  prefix: z.string(),
  attributes: z.string(),
})
type Values = z.infer<typeof schema>

export function notificationBody(v: Values, base: Body): Body {
  const out: Body = { ...base, topic: v.topic, payload_format: v.payloadFormat }
  const set = (k: string, value: unknown, keep: boolean) => {
    if (keep) out[k] = value
    else delete out[k]
  }
  set('event_types', v.eventTypes, v.eventTypes.length > 0)
  set('object_name_prefix', v.prefix, !!v.prefix)
  const attrs = parseLabels(v.attributes)
  set('custom_attributes', attrs, Object.keys(attrs).length > 0)
  return out
}

export function notificationValues(body: Body): Values {
  const s = (v: unknown) => (typeof v === 'string' ? v : '')
  return {
    topic: s(body.topic),
    eventTypes: Array.isArray(body.event_types) ? (body.event_types as string[]) : [],
    payloadFormat: s(body.payload_format) || 'JSON_API_V1',
    prefix: s(body.object_name_prefix),
    attributes: formatLabels(
      typeof body.custom_attributes === 'object' && body.custom_attributes !== null
        ? (body.custom_attributes as Record<string, string>)
        : {},
    ),
  }
}

/** topicsQuery lists a Project's topics to choose from. */
const topicsQuery = (project: string, enabled: boolean) => ({
  queryKey: ['pubsub', 'topics', project],
  queryFn: async () =>
    (
      await gcpFetch<{ topics?: { name: string }[] }>(
        `/pubsub/v1/projects/${encodeURIComponent(project)}/topics`,
      )
    ).topics ?? [],
  enabled,
})

export function CreateNotification() {
  const bucket = useBucket()
  const [view] = useViewState()
  const project = view.project ?? ''
  const { data: info } = useQuery(infoQuery())
  const topics = useQuery(topicsQuery(project, !!project && !!info?.services.includes('pubsub')))
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const defaults: Values = {
    topic: project ? `projects/${project}/topics/` : '',
    eventTypes: [],
    payloadFormat: 'JSON_API_V1',
    prefix: '',
    attributes: '',
  }
  const form = useForm<Values>({ resolver: zodResolver(schema), defaultValues: defaults })
  const errors = form.formState.errors
  const create = useMutation({
    meta: { toast: false },
    mutationFn: (body: Body) => createNotification(bucket, body),
    onSuccess: (n) => {
      void qc.invalidateQueries({ queryKey: ['gcs', 'notifications', bucket] })
      toast.success(`Created notification ${n.id}.`)
      navigate(notifPath(bucket, n.id))
    },
  })
  return (
    <Card>
      <h2 className="text-lg font-semibold">Create notification</h2>
      <ResourceEditor
        form={form}
        initialBody={notificationBody(defaults, {})}
        toBody={notificationBody}
        fromBody={(body) => notificationValues(body)}
        fields={['topic']}
        onSubmit={(body) => create.mutateAsync(body)}
        submitLabel="Create"
        onCancel={() => navigate(notifPath(bucket))}
        jsonHint={
          <>
            Sent to <span className="font-mono">notifications.insert</span>.
          </>
        }
      >
        <Field
          id="notif-topic"
          label="Topic"
          error={errors.topic?.message}
          hint="projects/PROJECT/topics/TOPIC; the topic must exist."
        >
          <Input
            list="notif-topics"
            autoComplete="off"
            spellCheck={false}
            {...fieldAria('notif-topic', errors.topic?.message, 'projects')}
            {...form.register('topic')}
          />
          <datalist id="notif-topics">
            {topics.data?.map((t) => (
              <option key={t.name} value={t.name} />
            ))}
          </datalist>
        </Field>
        <fieldset className="flex flex-col gap-1.5">
          <legend className="mb-1 text-sm font-medium">Event types</legend>
          <p className="text-xs text-muted-foreground">None selected sends every event.</p>
          {EVENT_TYPES.map((t) => (
            <label key={t} className="flex items-center gap-2 font-mono text-sm">
              <input
                type="checkbox"
                className="size-4"
                value={t}
                {...form.register('eventTypes')}
              />
              {t}
            </label>
          ))}
        </fieldset>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id="notif-payload" label="Payload format">
            <NativeSelect {...fieldAria('notif-payload')} {...form.register('payloadFormat')}>
              {PAYLOAD_FORMATS.map((p) => (
                <option key={p} value={p}>
                  {p}
                </option>
              ))}
            </NativeSelect>
          </Field>
          <Field id="notif-prefix" label="Object name prefix" hint="Only objects under it.">
            <Input
              spellCheck={false}
              {...fieldAria('notif-prefix', undefined, 'Only')}
              {...form.register('prefix')}
            />
          </Field>
        </div>
        <Field id="notif-attrs" label="Custom attributes" hint="key=value, one per line">
          <Textarea
            rows={3}
            spellCheck={false}
            className="font-mono"
            {...fieldAria('notif-attrs', undefined, 'key')}
            {...form.register('attributes')}
          />
        </Field>
      </ResourceEditor>
    </Card>
  )
}
