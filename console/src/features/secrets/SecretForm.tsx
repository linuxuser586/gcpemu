import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm, useWatch, type UseFormReturn } from 'react-hook-form'
import { z } from 'zod'

import { errorMessage } from '@/api/errors'
import { useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Field, fieldAria } from '@/components/resource/Field'
import { ResourceEditor, type Body } from '@/components/resource/ResourceEditor'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import { formatLabels, labelsError, parseLabels } from '@/lib/labels'
import { toast } from '@/lib/toast'
import { useViewState } from '@/lib/viewState'

import {
  addVersion,
  createSecret,
  duration,
  lastSegment,
  locationsQuery,
  patchSecret,
  SECRET_ID,
  secretIdMessage,
  secretQuery,
  seconds,
  type Secret,
  type SecretRef,
} from './api'
import { ChooseProject, listPath, locationLabel, secretPath, useSecretRef } from './SecretsLayout'

// Create and edit forms for secrets (FR-SM-001, FR-UI-011): create sends
// secrets.create's Secret, then adds the value as version 1 if one is
// given; edit sends secrets.patch with the fields that changed and an
// updateMask naming them. CMEK and the rest are in the JSON editor.

type Obj = Record<string, unknown>
const obj = (v: unknown): Obj =>
  typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Obj) : {}
const str = (v: unknown) => (typeof v === 'string' ? v : '')
const lines = (text: string) =>
  text
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter(Boolean)

const TOPIC = /^projects\/[^/]+\/topics\/[^/]+$/
const RFC3339 = /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:\d\d)$/
const digits = (what: string) =>
  z.string().trim().regex(/^\d*$/, `${what} must be a whole number of seconds.`)
const time = (what: string) =>
  z
    .string()
    .trim()
    .refine(
      (v) => !v || RFC3339.test(v),
      `${what} must be an RFC 3339 time, e.g. 2030-01-01T00:00:00Z.`,
    )

/** pairsError explains the first malformed key=value line, if any. */
function pairsError(text: string, what: string): string | undefined {
  for (const e of lines(text)) {
    if (!e.includes('=') || !e.split('=')[0]!.trim())
      return `Invalid ${what} "${e}": use key=value.`
  }
  return undefined
}

const fieldsSchema = {
  labels: z.string().superRefine((v, ctx) => {
    const err = labelsError(v)
    if (err) ctx.addIssue({ code: 'custom', message: err })
  }),
  annotations: z.string().superRefine((v, ctx) => {
    const err = pairsError(v, 'annotation')
    if (err) ctx.addIssue({ code: 'custom', message: err })
  }),
  topics: z.string().superRefine((v, ctx) => {
    const bad = lines(v).find((t) => !TOPIC.test(t))
    if (bad) ctx.addIssue({ code: 'custom', message: `Topic name "${bad}" is invalid.` })
  }),
  expiration: z.enum(['never', 'ttl', 'time']),
  ttlSeconds: digits('The TTL'),
  expireTime: time('The expire time'),
  nextRotationTime: time('The next rotation time'),
  rotationPeriodSeconds: digits('The rotation period'),
  destroyTtlSeconds: digits('The delayed destruction'),
}

const createSchema = z
  .object({
    id: z.string().superRefine((v, ctx) => {
      if (!SECRET_ID.test(v)) ctx.addIssue({ code: 'custom', message: secretIdMessage(v) })
    }),
    location: z.string(),
    replication: z.enum(['automatic', 'user']),
    replicas: z.string(),
    value: z.string(),
    ...fieldsSchema,
  })
  .superRefine((v, ctx) => {
    if (!v.location && v.replication === 'user' && lines(v.replicas).length === 0) {
      ctx.addIssue({
        code: 'custom',
        path: ['replicas'],
        message: 'A user-managed replication policy needs at least one replica.',
      })
    }
  })
export type CreateValues = z.infer<typeof createSchema>

const editSchema = z.object({
  aliases: z.string().superRefine((v, ctx) => {
    const bad = lines(v).find((e) => !/^[a-zA-Z0-9_-]{1,63}=\d+$/.test(e))
    if (bad)
      ctx.addIssue({
        code: 'custom',
        message: `Invalid version alias "${bad}": use alias=version.`,
      })
  }),
  ...fieldsSchema,
})
export type EditValues = z.infer<typeof editSchema>
type Fields = Omit<EditValues, 'aliases'>

const pairs = (m: Obj) =>
  Object.entries(m)
    .map(([k, v]) => `${k}=${String(v)}`)
    .join('\n')

const parsePairs = (text: string) => {
  const out: Record<string, string> = {}
  for (const e of lines(text)) {
    const [k = '', ...v] = e.split('=')
    out[k.trim()] = v.join('=').trim()
  }
  return out
}

/** fieldValues reads the fields create and edit share out of a Secret body. */
function fieldValues(body: Body): Fields {
  const rot = obj(body.rotation)
  const ttl = str(body.ttl)
  return {
    labels: formatLabels(obj(body.labels) as Record<string, string>),
    annotations: pairs(obj(body.annotations)),
    topics: (Array.isArray(body.topics) ? body.topics : []).map((t) => str(obj(t).name)).join('\n'),
    expiration: ttl ? 'ttl' : body.expireTime ? 'time' : 'never',
    ttlSeconds: seconds(ttl),
    expireTime: str(body.expireTime),
    nextRotationTime: str(rot.nextRotationTime),
    rotationPeriodSeconds: seconds(str(rot.rotationPeriod)),
    destroyTtlSeconds: seconds(str(body.versionDestroyTtl)),
  }
}

/** writeFields writes the shared fields onto out (create: unset fields are dropped). */
function writeFields(v: Fields, out: Body) {
  const set = (k: string, value: unknown, keep: boolean) => {
    if (keep) out[k] = value
    else delete out[k]
  }
  const labels = parseLabels(v.labels)
  set('labels', labels, Object.keys(labels).length > 0)
  const ann = parsePairs(v.annotations)
  set('annotations', ann, Object.keys(ann).length > 0)
  const topics = lines(v.topics).map((name) => ({ name }))
  set('topics', topics, topics.length > 0)
  set('ttl', duration(v.ttlSeconds), v.expiration === 'ttl' && !!v.ttlSeconds)
  set('expireTime', v.expireTime, v.expiration === 'time' && !!v.expireTime)
  const rot: Obj = { ...obj(out.rotation) }
  if (v.nextRotationTime) rot.nextRotationTime = v.nextRotationTime
  else delete rot.nextRotationTime
  if (v.rotationPeriodSeconds) rot.rotationPeriod = duration(v.rotationPeriodSeconds)
  else delete rot.rotationPeriod
  delete rot.managedRotationStatus
  set('rotation', rot, Object.keys(rot).length > 0)
  set('versionDestroyTtl', duration(v.destroyTtlSeconds), !!v.destroyTtlSeconds)
}

/** secretValues reads the create form's values out of a Secret body. */
export function secretValues(body: Body, prev: CreateValues): CreateValues {
  const um = obj(obj(body.replication).userManaged)
  const replicas = Array.isArray(um.replicas) ? um.replicas.map((r) => str(obj(r).location)) : []
  return {
    ...prev,
    replication: 'userManaged' in obj(body.replication) ? 'user' : 'automatic',
    replicas: replicas.join('\n'),
    ...fieldValues(body),
  }
}

/** secretBody writes the create form's values onto a secrets.create body. */
export function secretBody(v: CreateValues, base: Body): Body {
  const out: Body = { ...base }
  if (v.location) {
    delete out.replication
  } else if (v.replication === 'user') {
    out.replication = {
      userManaged: { replicas: lines(v.replicas).map((location) => ({ location })) },
    }
  } else {
    out.replication = { automatic: obj(obj(base.replication).automatic) }
  }
  writeFields(v, out)
  return out
}

/**
 * patchBody writes what the edit form changed onto a secrets.patch body;
 * a field cleared is null, which with the field in updateMask removes it.
 */
export function patchBody(s: Secret, v: EditValues, base: Body): Body {
  const was = editValues(s)
  const now: Body = {}
  writeFields(v, now)
  const out: Body = { ...base }
  const set = (k: string, changed: boolean, value: unknown) => {
    if (changed) out[k] = value
    else delete out[k]
  }
  set('labels', v.labels !== was.labels, now.labels ?? {})
  set('annotations', v.annotations !== was.annotations, now.annotations ?? {})
  set('topics', v.topics !== was.topics, now.topics ?? [])
  const expChanged =
    v.expiration !== was.expiration ||
    (v.expiration === 'ttl' && v.ttlSeconds !== was.ttlSeconds) ||
    (v.expiration === 'time' && v.expireTime !== was.expireTime)
  delete out.ttl
  delete out.expireTime
  if (expChanged) {
    if (now.ttl) out.ttl = now.ttl
    else out.expireTime = now.expireTime ?? null
  }
  set(
    'rotation',
    v.nextRotationTime !== was.nextRotationTime ||
      v.rotationPeriodSeconds !== was.rotationPeriodSeconds,
    now.rotation ?? null,
  )
  set(
    'versionDestroyTtl',
    v.destroyTtlSeconds !== was.destroyTtlSeconds,
    now.versionDestroyTtl ?? null,
  )
  const aliases: Record<string, number> = {}
  for (const [k, n] of Object.entries(parsePairs(v.aliases))) aliases[k] = Number(n)
  set('versionAliases', v.aliases !== was.aliases, aliases)
  return out
}

/** editValues are the edit form's values for s. */
export function editValues(s: Secret): EditValues {
  return { ...fieldValues(s as unknown as Body), aliases: pairs(obj(s.versionAliases)) }
}

/** patchValues reads an edit body back into the form, defaulting to s. */
export function patchValues(s: Secret) {
  return (body: Body): EditValues => {
    const was = editValues(s)
    const now = { ...fieldValues(body), aliases: pairs(obj(body.versionAliases)) }
    const has = (k: string) => k in body
    const exp = has('ttl') || has('expireTime')
    return {
      labels: has('labels') ? now.labels : was.labels,
      annotations: has('annotations') ? now.annotations : was.annotations,
      topics: has('topics') ? now.topics : was.topics,
      expiration: exp ? now.expiration : was.expiration,
      ttlSeconds: exp ? now.ttlSeconds : was.ttlSeconds,
      expireTime: exp ? now.expireTime : was.expireTime,
      nextRotationTime: has('rotation') ? now.nextRotationTime : was.nextRotationTime,
      rotationPeriodSeconds: has('rotation')
        ? now.rotationPeriodSeconds
        : was.rotationPeriodSeconds,
      destroyTtlSeconds: has('versionDestroyTtl') ? now.destroyTtlSeconds : was.destroyTtlSeconds,
      aliases: has('versionAliases') ? now.aliases : was.aliases,
    }
  }
}

/** updateMask names the body's fields in the API's snake_case. */
export const updateMask = (body: Body) =>
  Object.keys(body).map((k) => k.replace(/[A-Z]/g, (c) => `_${c.toLowerCase()}`))

/** SecretFields are the fields create and edit share. */
function SecretFields({
  form,
  prefix,
}: {
  form: UseFormReturn<EditValues> | UseFormReturn<CreateValues>
  prefix: string
}) {
  const f = form as UseFormReturn<EditValues>
  const errors = f.formState.errors
  const expiration = useWatch({ control: f.control, name: 'expiration' })
  const text = (name: 'labels' | 'annotations' | 'topics', label: string, hint: string) => (
    <Field id={`${prefix}-${name}`} label={label} error={errors[name]?.message} hint={hint}>
      <Textarea
        rows={2}
        spellCheck={false}
        className="font-mono"
        {...fieldAria(`${prefix}-${name}`, errors[name]?.message, hint)}
        {...f.register(name)}
      />
    </Field>
  )
  const input = (
    name:
      | 'ttlSeconds'
      | 'expireTime'
      | 'nextRotationTime'
      | 'rotationPeriodSeconds'
      | 'destroyTtlSeconds',
    label: string,
    hint: string,
  ) => (
    <Field id={`${prefix}-${name}`} label={label} error={errors[name]?.message} hint={hint}>
      <Input
        spellCheck={false}
        {...fieldAria(`${prefix}-${name}`, errors[name]?.message, hint)}
        {...f.register(name)}
      />
    </Field>
  )
  return (
    <>
      {text('labels', 'Labels', 'key=value, one per line')}
      {text('annotations', 'Annotations', 'key=value, one per line')}
      {text(
        'topics',
        'Pub/Sub topics',
        'projects/PROJECT/topics/TOPIC, one per line: every change to the secret is published to them.',
      )}
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id={`${prefix}-expiration`} label="Expiration">
          <NativeSelect {...fieldAria(`${prefix}-expiration`)} {...f.register('expiration')}>
            <option value="never">Never</option>
            <option value="ttl">After a duration (TTL)</option>
            <option value="time">At a time</option>
          </NativeSelect>
        </Field>
        {expiration === 'ttl' &&
          input(
            'ttlSeconds',
            'TTL (seconds)',
            'The secret is deleted this long after it is saved.',
          )}
        {expiration === 'time' &&
          input(
            'expireTime',
            'Expire time',
            'RFC 3339, e.g. 2030-01-01T00:00:00Z. The secret is deleted then.',
          )}
      </div>
      <div className="grid gap-4 sm:grid-cols-2">
        {input(
          'nextRotationTime',
          'Next rotation time',
          'RFC 3339; needs topics, at least 5 minutes ahead.',
        )}
        {input(
          'rotationPeriodSeconds',
          'Rotation period (seconds)',
          'At least 3600. Empty for one rotation.',
        )}
      </div>
      {input(
        'destroyTtlSeconds',
        'Delayed destruction (seconds)',
        'Destroyed versions stay disabled this long first (at least 86400). Empty to destroy at once.',
      )}
    </>
  )
}

// ---- create ----

const createDefaults: CreateValues = {
  id: '',
  location: '',
  replication: 'automatic',
  replicas: '',
  value: '',
  labels: '',
  annotations: '',
  topics: '',
  expiration: 'never',
  ttlSeconds: '',
  expireTime: '',
  nextRotationTime: '',
  rotationPeriodSeconds: '',
  destroyTtlSeconds: '',
}

export function CreateSecret() {
  const [view] = useViewState()
  const project = view.project ?? ''
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const locations = useQuery({ ...locationsQuery(project), enabled: !!project })
  const form = useForm<CreateValues>({
    resolver: zodResolver(createSchema),
    defaultValues: { ...createDefaults, location: view.location ?? '' },
  })
  const errors = form.formState.errors
  const location = useWatch({ control: form.control, name: 'location' })
  const replication = useWatch({ control: form.control, name: 'replication' })
  const create = useMutation({
    meta: { toast: false },
    mutationFn: async ({ body, values }: { body: Body; values: CreateValues }) => {
      const s = await createSecret(project, values.location, values.id, body)
      const ref: SecretRef = { project, location: values.location, secret: values.id }
      if (values.value) {
        try {
          await addVersion(ref, values.value)
        } catch (e) {
          toast.error(
            `Created secret ${values.id}, but adding its value failed: ${errorMessage(e)}`,
          )
        }
      }
      return { s, ref }
    },
    onSuccess: ({ s, ref }) => {
      void qc.invalidateQueries({ queryKey: ['secrets'] })
      toast.success(`Created secret ${lastSegment(s.name)}.`)
      navigate(secretPath(ref))
    },
  })
  if (!project) return <ChooseProject what="create a secret in it" />
  return (
    <Card>
      <h1 className="text-lg font-semibold">Create secret</h1>
      <ResourceEditor
        form={form}
        initialBody={secretBody(form.getValues(), {})}
        toBody={secretBody}
        fromBody={secretValues}
        fields={['id', 'replicas', 'labels', 'topics']}
        onSubmit={(body, values) => create.mutateAsync({ body, values })}
        submitLabel="Create"
        onCancel={() => navigate(listPath(form.getValues('location')))}
        jsonHint={
          <>
            Sent to <span className="font-mono">secrets.create</span> in{' '}
            <span className="font-mono">
              projects/{project}
              {location ? `/locations/${location}` : ''}
            </span>{' '}
            with the ID and location of the form; the secret value, if any, is then added as version
            1.
          </>
        }
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id="secret-id" label="Name" error={errors.id?.message} hint="Cannot be changed.">
            <Input
              autoComplete="off"
              spellCheck={false}
              {...fieldAria('secret-id', errors.id?.message, 'Cannot')}
              {...form.register('id')}
            />
          </Field>
          <Field
            id="secret-location"
            label="Location"
            hint="Global secrets replicate; regional ones stay in one region."
          >
            <NativeSelect
              {...fieldAria('secret-location', undefined, 'Global')}
              {...form.register('location')}
            >
              <option value="">Global</option>
              {(locations.data ?? []).map((l) => (
                <option key={l.locationId} value={l.locationId}>
                  {l.locationId}
                </option>
              ))}
            </NativeSelect>
          </Field>
        </div>
        {!location && (
          <div className="grid gap-4 sm:grid-cols-2">
            <Field id="secret-replication" label="Replication" hint="Stored, not acted on.">
              <NativeSelect
                {...fieldAria('secret-replication', undefined, 'Stored')}
                {...form.register('replication')}
              >
                <option value="automatic">Automatic</option>
                <option value="user">User-managed</option>
              </NativeSelect>
            </Field>
            {replication === 'user' && (
              <Field
                id="secret-replicas"
                label="Replica locations"
                error={errors.replicas?.message}
                hint="Regions, one per line"
              >
                <Textarea
                  rows={2}
                  spellCheck={false}
                  className="font-mono"
                  {...fieldAria('secret-replicas', errors.replicas?.message, 'Regions')}
                  {...form.register('replicas')}
                />
              </Field>
            )}
          </div>
        )}
        <Field
          id="secret-value"
          label="Secret value"
          hint="Optional: added as version 1. More versions can be added later."
        >
          <Textarea
            rows={3}
            spellCheck={false}
            autoComplete="off"
            className="font-mono"
            {...fieldAria('secret-value', undefined, 'Optional')}
            {...form.register('value')}
          />
        </Field>
        <SecretFields form={form} prefix="secret" />
      </ResourceEditor>
    </Card>
  )
}

// ---- edit ----

export function EditSecret() {
  const ref = useSecretRef()
  const query = useQuery({ ...secretQuery(ref), enabled: !!ref.project })
  if (!ref.project) return <ChooseProject what="edit its secrets" />
  if (!query.data) return <QueryStatus query={query} />
  return <EditSecretForm s={query.data} r={ref} />
}

function EditSecretForm({ s, r }: { s: Secret; r: SecretRef }) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<EditValues>({
    resolver: zodResolver(editSchema),
    defaultValues: editValues(s),
  })
  const errors = form.formState.errors
  const back = () => navigate(secretPath(r, 'overview'))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => {
      const mask = updateMask(body)
      if (mask.length === 0) return false
      await patchSecret(r, body, mask)
      return true
    },
    onSuccess: (changed) => {
      void qc.invalidateQueries({ queryKey: ['secrets'] })
      toast.success(changed ? `Updated secret ${r.secret}.` : 'Nothing to change.')
      back()
    },
  })
  return (
    <Card>
      <h2 className="text-lg font-semibold">
        Edit secret <span className="font-mono">{r.secret}</span>{' '}
        <span className="text-sm font-normal text-muted-foreground">
          ({locationLabel(r.location)})
        </span>
      </h2>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={(v, base) => patchBody(s, v, base)}
        fromBody={patchValues(s)}
        fields={['labels', 'topics', 'aliases']}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">secrets.patch</span> with an updateMask of the
            body&apos;s fields: null removes a field.
          </>
        }
      >
        <SecretFields form={form} prefix="edit-secret" />
        <Field
          id="edit-secret-aliases"
          label="Version aliases"
          error={errors.aliases?.message}
          hint="alias=version, one per line, e.g. prod=3"
        >
          <Textarea
            rows={2}
            spellCheck={false}
            className="font-mono"
            {...fieldAria('edit-secret-aliases', errors.aliases?.message, 'alias')}
            {...form.register('aliases')}
          />
        </Field>
      </ResourceEditor>
    </Card>
  )
}
