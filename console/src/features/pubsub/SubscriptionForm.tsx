import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm, useWatch, type UseFormReturn } from 'react-hook-form'
import { useParams, useSearchParams } from 'react-router'
import { z } from 'zod'

import { useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Field, fieldAria } from '@/components/resource/Field'
import { ResourceEditor, type Body } from '@/components/resource/ResourceEditor'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import { formatLabels, parseLabels } from '@/lib/labels'
import { toast } from '@/lib/toast'

import {
  createSubscription,
  duration,
  invalidName,
  patchSubscription,
  seconds,
  subscriptionName,
  subscriptionQuery,
  topicName,
  topicsQuery,
  validId,
  type Subscription,
} from './api'
import { ChooseProject, subscriptionPath, useProject } from './PubSubLayout'
import { labelsField, retentionField } from './TopicForm'

// Create and edit forms for subscriptions (FR-PS-001, FR-PS-004,
// FR-PS-006, FR-UI-011): create sends subscriptions.create's
// Subscription; edit sends subscriptions.patch with the fields that
// changed and an updateMask naming them. Push attributes, payload
// unwrapping and the rest are in the JSON editor.

type Obj = Record<string, unknown>
const obj = (v: unknown): Obj =>
  typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Obj) : {}
const str = (v: unknown) => (typeof v === 'string' ? v : '')
const num = (v: unknown) => (typeof v === 'number' ? String(v) : '')

const TOPIC = /^projects\/[^/]+\/topics\/[^/]+$/
const whole = /^\d+$/

const fieldsSchema = {
  labels: labelsField,
  delivery: z.enum(['pull', 'push']),
  pushEndpoint: z.string().trim(),
  oidcServiceAccount: z.string().trim(),
  oidcAudience: z.string().trim(),
  ackDeadline: z
    .string()
    .trim()
    .superRefine((v, ctx) => {
      const n = Number(v)
      if (v && (!whole.test(v) || n < 10 || n > 600)) {
        ctx.addIssue({
          code: 'custom',
          message: `Invalid ack_deadline_seconds: ${v}. The minimum deadline you can specify is 10 seconds. The maximum deadline you can specify is 600 seconds.`,
        })
      }
    }),
  retentionSeconds: retentionField,
  retainAcked: z.boolean(),
  expiration: z.enum(['never', 'ttl']),
  expirationTtlSeconds: z.string().trim(),
  exactlyOnce: z.boolean(),
  deadLetterTopic: z
    .string()
    .trim()
    .superRefine((v, ctx) => {
      if (v && !TOPIC.test(v)) ctx.addIssue({ code: 'custom', message: invalidName('topics', v) })
    }),
  maxDeliveryAttempts: z.string().trim(),
  retry: z.enum(['immediate', 'backoff']),
  minBackoff: z.string().trim(),
  maxBackoff: z.string().trim(),
}

/** checks are the API's rules across fields. */
function checks(v: Fields, ctx: z.RefinementCtx) {
  const issue = (path: keyof Fields, message: string) =>
    ctx.addIssue({ code: 'custom', path: [path], message })
  if (v.delivery === 'push') {
    let ok = false
    try {
      const u = new URL(v.pushEndpoint)
      ok = (u.protocol === 'http:' || u.protocol === 'https:') && !!u.host
    } catch {
      ok = false
    }
    if (!ok) {
      issue(
        'pushEndpoint',
        `Invalid push_config.push_endpoint: ${JSON.stringify(v.pushEndpoint)} is not a valid URL.`,
      )
    }
    if (v.oidcAudience && !v.oidcServiceAccount) {
      issue('oidcServiceAccount', 'push_config.oidc_token.service_account_email must be set.')
    }
    if (v.exactlyOnce) {
      issue('exactlyOnce', 'Exactly once delivery is not supported for push subscriptions.')
    }
  }
  if (v.expiration === 'ttl') {
    const n = Number(v.expirationTtlSeconds)
    if (!whole.test(v.expirationTtlSeconds) || n < 86400) {
      issue(
        'expirationTtlSeconds',
        'Invalid expiration_policy.ttl: the minimum allowed value is 1 day.',
      )
    }
  }
  if (v.deadLetterTopic && v.maxDeliveryAttempts) {
    const n = Number(v.maxDeliveryAttempts)
    if (!whole.test(v.maxDeliveryAttempts) || n < 5 || n > 100) {
      issue(
        'maxDeliveryAttempts',
        `Invalid dead_letter_policy.max_delivery_attempts: ${v.maxDeliveryAttempts}. It must be between 5 and 100.`,
      )
    }
  }
  if (v.retry === 'backoff') {
    const lo = v.minBackoff === '' ? 10 : Number(v.minBackoff)
    const hi = v.maxBackoff === '' ? 600 : Number(v.maxBackoff)
    const bad = (s: string) => s !== '' && !whole.test(s)
    if (bad(v.minBackoff) || bad(v.maxBackoff) || lo > 600 || hi > 600 || lo > hi) {
      issue(
        'maxBackoff',
        'Invalid retry_policy: backoffs must be between 0 and 600 seconds and minimum_backoff must not exceed maximum_backoff.',
      )
    }
  }
}

const editObject = z.object(fieldsSchema)
type Fields = z.infer<typeof editObject>
const editSchema = editObject.superRefine(checks)
export type EditValues = Fields

const createSchema = (project: string) =>
  editObject
    .extend({
      id: z.string().superRefine((v, ctx) => {
        if (!validId(v)) {
          ctx.addIssue({
            code: 'custom',
            message: invalidName('subscriptions', subscriptionName(project, v)),
          })
        }
      }),
      topic: z
        .string()
        .trim()
        .superRefine((v, ctx) => {
          if (!v)
            ctx.addIssue({
              code: 'custom',
              message: 'The topic field in the Subscription must be set.',
            })
          else if (!TOPIC.test(v))
            ctx.addIssue({ code: 'custom', message: invalidName('topics', v) })
        }),
      ordering: z.boolean(),
      filter: z.string(),
    })
    .superRefine(checks)
export type CreateValues = Fields & { id: string; topic: string; ordering: boolean; filter: string }

const fieldDefaults: Fields = {
  labels: '',
  delivery: 'pull',
  pushEndpoint: '',
  oidcServiceAccount: '',
  oidcAudience: '',
  ackDeadline: '10',
  retentionSeconds: '604800',
  retainAcked: false,
  expiration: 'ttl',
  expirationTtlSeconds: '2678400',
  exactlyOnce: false,
  deadLetterTopic: '',
  maxDeliveryAttempts: '5',
  retry: 'immediate',
  minBackoff: '10',
  maxBackoff: '600',
}

/** fieldValues reads the fields create and edit share out of a Subscription body. */
function fieldValues(body: Body): Fields {
  const push = obj(body.pushConfig)
  const oidc = obj(push.oidcToken)
  const exp = 'expirationPolicy' in body ? obj(body.expirationPolicy) : undefined
  const dl = obj(body.deadLetterPolicy)
  const rp = 'retryPolicy' in body ? obj(body.retryPolicy) : undefined
  return {
    labels: formatLabels(obj(body.labels) as Record<string, string>),
    delivery: str(push.pushEndpoint) ? 'push' : 'pull',
    pushEndpoint: str(push.pushEndpoint),
    oidcServiceAccount: str(oidc.serviceAccountEmail),
    oidcAudience: str(oidc.audience),
    ackDeadline: num(body.ackDeadlineSeconds),
    retentionSeconds: seconds(str(body.messageRetentionDuration)),
    retainAcked: body.retainAckedMessages === true,
    expiration: exp && !str(exp.ttl) ? 'never' : 'ttl',
    expirationTtlSeconds: exp ? seconds(str(exp.ttl)) : fieldDefaults.expirationTtlSeconds,
    exactlyOnce: body.enableExactlyOnceDelivery === true,
    deadLetterTopic: str(dl.deadLetterTopic),
    maxDeliveryAttempts: num(dl.maxDeliveryAttempts) || fieldDefaults.maxDeliveryAttempts,
    retry: rp ? 'backoff' : 'immediate',
    minBackoff: rp ? seconds(str(rp.minimumBackoff)) : fieldDefaults.minBackoff,
    maxBackoff: rp ? seconds(str(rp.maximumBackoff)) : fieldDefaults.maxBackoff,
  }
}

/** writeFields writes the shared fields onto out, dropping unset ones. */
function writeFields(v: Fields, out: Body) {
  const set = (k: string, value: unknown, keep: boolean) => {
    if (keep) out[k] = value
    else delete out[k]
  }
  const labels = parseLabels(v.labels)
  set('labels', labels, Object.keys(labels).length > 0)
  const push: Obj = { ...obj(out.pushConfig) }
  if (v.delivery === 'push') {
    push.pushEndpoint = v.pushEndpoint
    if (v.oidcServiceAccount) {
      push.oidcToken = {
        serviceAccountEmail: v.oidcServiceAccount,
        ...(v.oidcAudience ? { audience: v.oidcAudience } : {}),
      }
    } else delete push.oidcToken
  }
  set('pushConfig', push, v.delivery === 'push')
  set('ackDeadlineSeconds', Number(v.ackDeadline), !!v.ackDeadline)
  set('messageRetentionDuration', duration(v.retentionSeconds), !!v.retentionSeconds)
  set('retainAckedMessages', true, v.retainAcked)
  set(
    'expirationPolicy',
    v.expiration === 'never' ? {} : { ttl: duration(v.expirationTtlSeconds) },
    v.expiration === 'never' || !!v.expirationTtlSeconds,
  )
  set('enableExactlyOnceDelivery', true, v.exactlyOnce)
  set(
    'deadLetterPolicy',
    {
      deadLetterTopic: v.deadLetterTopic,
      ...(v.maxDeliveryAttempts ? { maxDeliveryAttempts: Number(v.maxDeliveryAttempts) } : {}),
    },
    !!v.deadLetterTopic,
  )
  set(
    'retryPolicy',
    {
      ...(v.minBackoff ? { minimumBackoff: duration(v.minBackoff) } : {}),
      ...(v.maxBackoff ? { maximumBackoff: duration(v.maxBackoff) } : {}),
    },
    v.retry === 'backoff',
  )
}

/** subscriptionValues reads the create form's values out of a Subscription body. */
export function subscriptionValues(body: Body, prev: CreateValues): CreateValues {
  return {
    ...prev,
    topic: str(body.topic),
    ordering: body.enableMessageOrdering === true,
    filter: str(body.filter),
    ...fieldValues(body),
  }
}

/** subscriptionBody writes the create form's values onto a subscriptions.create body. */
export function subscriptionBody(v: CreateValues, base: Body): Body {
  const out: Body = { ...base, topic: v.topic }
  if (v.ordering) out.enableMessageOrdering = true
  else delete out.enableMessageOrdering
  if (v.filter) out.filter = v.filter
  else delete out.filter
  writeFields(v, out)
  return out
}

// The edit form's fields grouped by the Subscription field they write.
const GROUPS: [field: string, values: (keyof Fields)[], cleared: unknown][] = [
  ['labels', ['labels'], {}],
  ['pushConfig', ['delivery', 'pushEndpoint', 'oidcServiceAccount', 'oidcAudience'], {}],
  ['ackDeadlineSeconds', ['ackDeadline'], null],
  ['messageRetentionDuration', ['retentionSeconds'], null],
  ['retainAckedMessages', ['retainAcked'], false],
  ['expirationPolicy', ['expiration', 'expirationTtlSeconds'], null],
  ['enableExactlyOnceDelivery', ['exactlyOnce'], false],
  ['deadLetterPolicy', ['deadLetterTopic', 'maxDeliveryAttempts'], null],
  ['retryPolicy', ['retry', 'minBackoff', 'maxBackoff'], null],
]

/** editValues are the edit form's values for s. */
export const editValues = (s: Subscription) => fieldValues(s as unknown as Body)

/**
 * patchBody writes what the edit form changed onto a subscriptions.patch
 * body; a field cleared is null (or its empty value), which with the
 * field in the updateMask removes it.
 */
export function patchBody(s: Subscription, v: EditValues, base: Body): Body {
  const was = editValues(s)
  const now: Body = {}
  writeFields(v, now)
  const out: Body = { ...base }
  for (const [field, keys, cleared] of GROUPS) {
    delete out[field]
    if (keys.some((k) => v[k] !== was[k])) out[field] = now[field] ?? cleared
  }
  return out
}

/** patchValues reads an edit body back into the form, defaulting to s. */
export const patchValues =
  (s: Subscription) =>
  (body: Body): EditValues => {
    const was = editValues(s)
    const now = fieldValues(body)
    const out = { ...was }
    for (const [field, keys] of GROUPS) {
      if (!(field in body)) continue
      for (const k of keys) (out as Record<string, unknown>)[k] = now[k]
    }
    return out
  }

/** SubscriptionFields are the fields create and edit share. */
function SubscriptionFields({
  form,
  prefix,
}: {
  form: UseFormReturn<EditValues> | UseFormReturn<CreateValues>
  prefix: string
}) {
  const f = form as UseFormReturn<EditValues>
  const errors = f.formState.errors
  const [delivery, expiration, deadLetterTopic, retry] = useWatch({
    control: f.control,
    name: ['delivery', 'expiration', 'deadLetterTopic', 'retry'],
  })
  const input = (name: keyof Fields, label: string, hint?: string, numeric = false) => (
    <Field id={`${prefix}-${name}`} label={label} error={errors[name]?.message} hint={hint}>
      <Input
        spellCheck={false}
        autoComplete="off"
        inputMode={numeric ? 'numeric' : undefined}
        {...fieldAria(`${prefix}-${name}`, errors[name]?.message, hint)}
        {...f.register(name)}
      />
    </Field>
  )
  const check = (name: 'retainAcked' | 'exactlyOnce', label: string, hint: string) => (
    <div className="flex flex-col gap-1">
      <div className="flex items-start gap-2">
        <input
          type="checkbox"
          className="mt-0.5 size-4"
          {...fieldAria(`${prefix}-${name}`, errors[name]?.message, hint)}
          {...f.register(name)}
        />
        <div className="flex flex-col gap-0.5">
          <label htmlFor={`${prefix}-${name}`} className="text-sm font-medium">
            {label}
          </label>
          <p id={`${prefix}-${name}-hint`} className="text-xs text-muted-foreground">
            {hint}
          </p>
        </div>
      </div>
      {errors[name]?.message && (
        <p id={`${prefix}-${name}-error`} role="alert" className="text-sm text-destructive">
          {errors[name]?.message}
        </p>
      )}
    </div>
  )
  return (
    <>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id={`${prefix}-delivery`} label="Delivery type">
          <NativeSelect {...fieldAria(`${prefix}-delivery`)} {...f.register('delivery')}>
            <option value="pull">Pull</option>
            <option value="push">Push</option>
          </NativeSelect>
        </Field>
        {input('ackDeadline', 'Acknowledgement deadline (seconds)', '10 to 600', true)}
      </div>
      {delivery === 'push' && (
        <>
          {input(
            'pushEndpoint',
            'Push endpoint',
            'An http or https URL the emulator POSTs messages to.',
          )}
          <div className="grid gap-4 sm:grid-cols-2">
            {input(
              'oidcServiceAccount',
              'OIDC service account',
              'Optional: sign an OIDC token as this service account.',
            )}
            {input('oidcAudience', 'OIDC audience', 'Optional: the push endpoint by default.')}
          </div>
        </>
      )}
      {check(
        'exactlyOnce',
        'Exactly-once delivery',
        'Acks of expired leases fail instead of succeeding silently. Pull subscriptions only.',
      )}
      <div className="grid gap-4 sm:grid-cols-2">
        {input(
          'retentionSeconds',
          'Message retention (seconds)',
          '600 to 2678400; 604800 (7 days) by default.',
          true,
        )}
        <div className="pt-6">
          {check(
            'retainAcked',
            'Retain acknowledged messages',
            'Keep acked messages for the retention period, to replay them by seek.',
          )}
        </div>
      </div>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id={`${prefix}-expiration`} label="Expiration">
          <NativeSelect {...fieldAria(`${prefix}-expiration`)} {...f.register('expiration')}>
            <option value="ttl">After a period of inactivity</option>
            <option value="never">Never</option>
          </NativeSelect>
        </Field>
        {expiration === 'ttl' &&
          input(
            'expirationTtlSeconds',
            'Inactivity period (seconds)',
            'At least 86400. Stored, not acted on.',
            true,
          )}
      </div>
      <div className="grid gap-4 sm:grid-cols-2">
        {input(
          'deadLetterTopic',
          'Dead-letter topic',
          'projects/PROJECT/topics/TOPIC. Empty for no dead lettering.',
        )}
        {deadLetterTopic &&
          input('maxDeliveryAttempts', 'Maximum delivery attempts', '5 to 100', true)}
      </div>
      <div className="grid gap-4 sm:grid-cols-3">
        <Field id={`${prefix}-retry`} label="Retry policy">
          <NativeSelect {...fieldAria(`${prefix}-retry`)} {...f.register('retry')}>
            <option value="immediate">Retry immediately</option>
            <option value="backoff">Exponential backoff</option>
          </NativeSelect>
        </Field>
        {retry === 'backoff' && (
          <>
            {input('minBackoff', 'Minimum backoff (seconds)', '0 to 600', true)}
            {input('maxBackoff', 'Maximum backoff (seconds)', '0 to 600', true)}
          </>
        )}
      </div>
      <Field
        id={`${prefix}-labels`}
        label="Labels"
        error={errors.labels?.message}
        hint="key=value, one per line"
      >
        <Textarea
          rows={2}
          spellCheck={false}
          className="font-mono"
          {...fieldAria(`${prefix}-labels`, errors.labels?.message, 'key')}
          {...f.register('labels')}
        />
      </Field>
    </>
  )
}

// ---- create ----

export function CreateSubscription() {
  const project = useProject()
  const [search] = useSearchParams()
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const topics = useQuery({ ...topicsQuery(project), enabled: !!project })
  const preset = search.get('topic')
  const form = useForm<CreateValues>({
    resolver: zodResolver(createSchema(project)),
    defaultValues: {
      ...fieldDefaults,
      id: '',
      topic: preset ? topicName(project, preset) : '',
      ordering: false,
      filter: '',
    },
  })
  const errors = form.formState.errors
  const create = useMutation({
    meta: { toast: false },
    mutationFn: ({ body, id }: { body: Body; id: string }) => createSubscription(project, id, body),
    onSuccess: (_, { id }) => {
      void qc.invalidateQueries({ queryKey: ['pubsub'] })
      toast.success(`Created subscription ${id}.`)
      navigate(subscriptionPath(id))
    },
  })
  if (!project) return <ChooseProject what="create a subscription in it" />
  return (
    <Card>
      <h1 className="text-lg font-semibold">Create subscription</h1>
      <ResourceEditor
        form={form}
        initialBody={subscriptionBody(form.getValues(), {})}
        toBody={subscriptionBody}
        fromBody={subscriptionValues}
        fields={['id', 'topic', 'filter', 'labels']}
        onSubmit={(body, values) => create.mutateAsync({ body, id: values.id })}
        submitLabel="Create"
        onCancel={() => navigate('/pubsub/subscriptions')}
        jsonHint={
          <>
            Sent to <span className="font-mono">subscriptions.create</span> as{' '}
            <span className="font-mono">projects/{project}/subscriptions/</span> with the
            subscription ID of the form.
          </>
        }
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field
            id="sub-id"
            label="Subscription ID"
            error={errors.id?.message}
            hint="Starts with a letter; 3 to 255 letters, digits and - _ . ~ + %. Cannot be changed."
          >
            <Input
              autoComplete="off"
              spellCheck={false}
              {...fieldAria('sub-id', errors.id?.message, 'Starts')}
              {...form.register('id')}
            />
          </Field>
          <Field
            id="sub-topic"
            label="Topic"
            error={errors.topic?.message}
            hint="projects/PROJECT/topics/TOPIC, of any Project. Cannot be changed."
          >
            <Input
              list="sub-topics"
              autoComplete="off"
              spellCheck={false}
              {...fieldAria('sub-topic', errors.topic?.message, 'projects')}
              {...form.register('topic')}
            />
            <datalist id="sub-topics">
              {topics.data?.map((t) => (
                <option key={t.name} value={t.name} />
              ))}
            </datalist>
          </Field>
        </div>
        <Field
          id="sub-filter"
          label="Filter"
          error={errors.filter?.message}
          hint='Only messages whose attributes match are delivered, e.g. attributes.kind = "order". Cannot be changed.'
        >
          <Input
            spellCheck={false}
            autoComplete="off"
            className="font-mono"
            {...fieldAria('sub-filter', errors.filter?.message, 'Only')}
            {...form.register('filter')}
          />
        </Field>
        <div className="flex items-start gap-2">
          <input
            type="checkbox"
            className="mt-0.5 size-4"
            {...fieldAria('sub-ordering', undefined, 'Messages')}
            {...form.register('ordering')}
          />
          <div className="flex flex-col gap-0.5">
            <label htmlFor="sub-ordering" className="text-sm font-medium">
              Message ordering
            </label>
            <p id="sub-ordering-hint" className="text-xs text-muted-foreground">
              Messages with the same ordering key are delivered in publish order. Cannot be changed.
            </p>
          </div>
        </div>
        <SubscriptionFields form={form} prefix="sub" />
      </ResourceEditor>
    </Card>
  )
}

// ---- edit ----

export function EditSubscription() {
  const project = useProject()
  const { subscription = '' } = useParams()
  const query = useQuery({ ...subscriptionQuery(project, subscription), enabled: !!project })
  if (!project) return <ChooseProject what="edit its subscriptions" />
  if (!query.data) return <QueryStatus query={query} />
  return <EditSubscriptionForm s={query.data} project={project} id={subscription} />
}

function EditSubscriptionForm({
  s,
  project,
  id,
}: {
  s: Subscription
  project: string
  id: string
}) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<EditValues>({
    resolver: zodResolver(editSchema),
    defaultValues: editValues(s),
  })
  const back = () => navigate(subscriptionPath(id, 'overview'))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => {
      const mask = Object.keys(body)
      if (mask.length === 0) return false
      await patchSubscription(project, id, body, mask)
      return true
    },
    onSuccess: (changed) => {
      void qc.invalidateQueries({ queryKey: ['pubsub'] })
      toast.success(changed ? `Updated subscription ${id}.` : 'Nothing to change.')
      back()
    },
  })
  return (
    <Card>
      <h2 className="text-lg font-semibold">
        Edit subscription <span className="font-mono">{id}</span>
      </h2>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={(v, base) => patchBody(s, v, base)}
        fromBody={patchValues(s)}
        fields={['labels']}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">subscriptions.patch</span> as the subscription, with
            an updateMask of its fields: null clears a field.
          </>
        }
      >
        <SubscriptionFields form={form} prefix="edit-sub" />
      </ResourceEditor>
    </Card>
  )
}
