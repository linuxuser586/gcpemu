import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm, type UseFormReturn } from 'react-hook-form'
import { z } from 'zod'

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
  bucketNameMessage,
  bucketQuery,
  createBucket,
  patchBucket,
  STORAGE_CLASSES,
  validBucketName,
  type Bucket,
} from './api'
import { bucketPath, browsePath, ChooseProject, useBucket } from './GcsLayout'

// Create and edit forms for buckets (FR-GCS-001, FR-UI-011): create sends
// buckets.insert's Bucket; edit sends a buckets.patch merge patch of only
// the fields that changed. CORS, lifecycle rules and the rest are in the
// JSON editor.

type Obj = Record<string, unknown>
const obj = (v: unknown): Obj =>
  typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Obj) : {}
const str = (v: unknown) => (typeof v === 'string' ? v : '')

const fieldsSchema = {
  storageClass: z.string(),
  versioning: z.boolean(),
  uniformAccess: z.boolean(),
  labels: z.string().superRefine((v, ctx) => {
    const err = labelsError(v)
    if (err) ctx.addIssue({ code: 'custom', message: err })
  }),
  retentionSeconds: z
    .string()
    .trim()
    .regex(/^\d*$/, 'The retention period must be a whole number of seconds.'),
  defaultEventBasedHold: z.boolean(),
  mainPageSuffix: z.string().trim(),
  notFoundPage: z.string().trim(),
}

const createSchema = z.object({
  name: z.string().superRefine((v, ctx) => {
    if (!validBucketName(v)) ctx.addIssue({ code: 'custom', message: bucketNameMessage(v) })
  }),
  location: z.string().trim(),
  ...fieldsSchema,
})
type CreateValues = z.infer<typeof createSchema>

const editSchema = z.object(fieldsSchema)
type EditValues = z.infer<typeof editSchema>

/** bucketValues reads the form's fields out of a Bucket body. */
export function bucketValues(body: Body): CreateValues {
  const iam = obj(obj(body.iamConfiguration).uniformBucketLevelAccess)
  const website = obj(body.website)
  const retention = obj(body.retentionPolicy).retentionPeriod
  return {
    name: str(body.name),
    location: str(body.location),
    storageClass: str(body.storageClass),
    versioning: obj(body.versioning).enabled === true,
    uniformAccess: iam.enabled === true,
    labels: formatLabels(obj(body.labels) as Record<string, string>),
    retentionSeconds:
      typeof retention === 'string' || typeof retention === 'number' ? String(retention) : '',
    defaultEventBasedHold: body.defaultEventBasedHold === true,
    mainPageSuffix: str(website.mainPageSuffix),
    notFoundPage: str(website.notFoundPage),
  }
}

/** bucketBody writes the form's fields onto a buckets.insert body. */
export function bucketBody(v: CreateValues, base: Body): Body {
  const out: Body = { ...base, name: v.name }
  const set = (k: string, value: unknown, keep: boolean) => {
    if (keep) out[k] = value
    else delete out[k]
  }
  set('location', v.location.toUpperCase(), !!v.location)
  set('storageClass', v.storageClass, !!v.storageClass)
  set('versioning', { ...obj(base.versioning), enabled: true }, v.versioning)
  const iam = obj(base.iamConfiguration)
  out.iamConfiguration = {
    ...iam,
    uniformBucketLevelAccess: { ...obj(iam.uniformBucketLevelAccess), enabled: v.uniformAccess },
  }
  const labels = parseLabels(v.labels)
  set('labels', labels, Object.keys(labels).length > 0)
  set(
    'retentionPolicy',
    { ...obj(base.retentionPolicy), retentionPeriod: v.retentionSeconds },
    !!v.retentionSeconds,
  )
  set('defaultEventBasedHold', true, v.defaultEventBasedHold)
  const website: Obj = { ...obj(base.website) }
  if (v.mainPageSuffix) website.mainPageSuffix = v.mainPageSuffix
  else delete website.mainPageSuffix
  if (v.notFoundPage) website.notFoundPage = v.notFoundPage
  else delete website.notFoundPage
  set('website', website, Object.keys(website).length > 0)
  return out
}

/**
 * patchBody writes what the edit form changed onto a buckets.patch body:
 * removed labels are null, as a merge patch deletes them.
 */
export function patchBody(b: Bucket, v: EditValues, base: Body): Body {
  const was = bucketValues(b as unknown as Body)
  const out: Body = { ...base }
  const set = (k: string, changed: boolean, value: unknown) => {
    if (changed) out[k] = value
    else delete out[k]
  }
  set('storageClass', v.storageClass !== was.storageClass, v.storageClass)
  set('versioning', v.versioning !== was.versioning, { enabled: v.versioning })
  set('iamConfiguration', v.uniformAccess !== was.uniformAccess, {
    uniformBucketLevelAccess: { enabled: v.uniformAccess },
  })
  if (v.labels !== was.labels) {
    const next = parseLabels(v.labels)
    const labels: Record<string, string | null> = { ...next }
    for (const k of Object.keys(b.labels ?? {})) if (!(k in next)) labels[k] = null
    out.labels = labels
  } else delete out.labels
  set(
    'retentionPolicy',
    v.retentionSeconds !== was.retentionSeconds,
    v.retentionSeconds ? { retentionPeriod: v.retentionSeconds } : null,
  )
  set(
    'defaultEventBasedHold',
    v.defaultEventBasedHold !== was.defaultEventBasedHold,
    v.defaultEventBasedHold,
  )
  const websiteChanged =
    v.mainPageSuffix !== was.mainPageSuffix || v.notFoundPage !== was.notFoundPage
  set(
    'website',
    websiteChanged,
    v.mainPageSuffix || v.notFoundPage
      ? { mainPageSuffix: v.mainPageSuffix || null, notFoundPage: v.notFoundPage || null }
      : null,
  )
  return out
}

/** patchValues reads an edit body back into the form, defaulting to b. */
export function patchValues(b: Bucket) {
  return (body: Body): EditValues => {
    const was = bucketValues(b as unknown as Body)
    const now = bucketValues(body)
    const has = (k: string) => k in body
    const labels = has('labels')
      ? formatLabels(
          Object.fromEntries(
            Object.entries(obj(body.labels)).filter(([, v]) => v !== null),
          ) as Record<string, string>,
        )
      : was.labels
    return {
      storageClass: has('storageClass') ? now.storageClass : was.storageClass,
      versioning: has('versioning') ? now.versioning : was.versioning,
      uniformAccess: has('iamConfiguration') ? now.uniformAccess : was.uniformAccess,
      labels,
      retentionSeconds: has('retentionPolicy') ? now.retentionSeconds : was.retentionSeconds,
      defaultEventBasedHold: has('defaultEventBasedHold')
        ? now.defaultEventBasedHold
        : was.defaultEventBasedHold,
      mainPageSuffix: has('website') ? now.mainPageSuffix : was.mainPageSuffix,
      notFoundPage: has('website') ? now.notFoundPage : was.notFoundPage,
    }
  }
}

function Checkbox({
  id,
  label,
  hint,
  form,
  name,
}: {
  id: string
  label: string
  hint?: string
  form: UseFormReturn<EditValues> | UseFormReturn<CreateValues>
  name: 'versioning' | 'uniformAccess' | 'defaultEventBasedHold'
}) {
  const register = (form as UseFormReturn<EditValues>).register
  return (
    <div className="flex items-start gap-2">
      <input
        type="checkbox"
        className="mt-0.5 size-4"
        {...fieldAria(id, undefined, hint)}
        {...register(name)}
      />
      <div className="flex flex-col gap-0.5">
        <label htmlFor={id} className="text-sm font-medium">
          {label}
        </label>
        {hint && (
          <p id={`${id}-hint`} className="text-xs text-muted-foreground">
            {hint}
          </p>
        )}
      </div>
    </div>
  )
}

/** BucketFields are the fields create and edit share. */
function BucketFields({
  form,
  prefix,
}: {
  form: UseFormReturn<EditValues> | UseFormReturn<CreateValues>
  prefix: string
}) {
  const f = form as UseFormReturn<EditValues>
  const errors = f.formState.errors
  return (
    <>
      <Field id={`${prefix}-class`} label="Default storage class" hint="Stored, not acted on.">
        <NativeSelect
          {...fieldAria(`${prefix}-class`, undefined, 'Stored')}
          {...f.register('storageClass')}
        >
          <option value="">Default (STANDARD)</option>
          {STORAGE_CLASSES.map((c) => (
            <option key={c} value={c}>
              {c}
            </option>
          ))}
        </NativeSelect>
      </Field>
      <Checkbox
        id={`${prefix}-versioning`}
        label="Object versioning"
        hint="Keep noncurrent generations when objects are replaced or deleted."
        form={form}
        name="versioning"
      />
      <Checkbox
        id={`${prefix}-ubla`}
        label="Uniform bucket-level access"
        hint="Control access with IAM only; object ACLs are off."
        form={form}
        name="uniformAccess"
      />
      <Field
        id={`${prefix}-labels`}
        label="Labels"
        error={errors.labels?.message}
        hint="key=value, one per line"
      >
        <Textarea
          rows={3}
          spellCheck={false}
          className="font-mono"
          {...fieldAria(`${prefix}-labels`, errors.labels?.message, 'key=value')}
          {...f.register('labels')}
        />
      </Field>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field
          id={`${prefix}-retention`}
          label="Retention period (seconds)"
          error={errors.retentionSeconds?.message}
          hint="Objects cannot be deleted or replaced until they are this old. Empty for none."
        >
          <Input
            inputMode="numeric"
            {...fieldAria(`${prefix}-retention`, errors.retentionSeconds?.message, 'Objects')}
            {...f.register('retentionSeconds')}
          />
        </Field>
        <div className="sm:pt-6">
          <Checkbox
            id={`${prefix}-ebh`}
            label="Default event-based hold"
            hint="New objects start with an event-based hold."
            form={form}
            name="defaultEventBasedHold"
          />
        </div>
      </div>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id={`${prefix}-main`} label="Website main page" hint="e.g. index.html">
          <Input
            spellCheck={false}
            {...fieldAria(`${prefix}-main`, undefined, 'e.g.')}
            {...f.register('mainPageSuffix')}
          />
        </Field>
        <Field id={`${prefix}-404`} label="Website not-found page" hint="e.g. 404.html">
          <Input
            spellCheck={false}
            {...fieldAria(`${prefix}-404`, undefined, 'e.g.')}
            {...f.register('notFoundPage')}
          />
        </Field>
      </div>
    </>
  )
}

// ---- create ----

const createDefaults: CreateValues = {
  name: '',
  location: 'US',
  storageClass: '',
  versioning: false,
  uniformAccess: true,
  labels: '',
  retentionSeconds: '',
  defaultEventBasedHold: false,
  mainPageSuffix: '',
  notFoundPage: '',
}

export function CreateBucket() {
  const [view] = useViewState()
  const project = view.project ?? ''
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<CreateValues>({
    resolver: zodResolver(createSchema),
    defaultValues: createDefaults,
  })
  const errors = form.formState.errors
  const create = useMutation({
    meta: { toast: false },
    mutationFn: (body: Body) => createBucket(project, body),
    onSuccess: (b) => {
      void qc.invalidateQueries({ queryKey: ['gcs'] })
      toast.success(`Created bucket ${b.name}.`)
      navigate(browsePath(b.name))
    },
  })
  if (!project) return <ChooseProject what="create a bucket in it" />
  return (
    <Card>
      <h1 className="text-lg font-semibold">Create bucket</h1>
      <ResourceEditor
        form={form}
        initialBody={bucketBody(createDefaults, {})}
        toBody={bucketBody}
        fromBody={(body) => bucketValues(body)}
        fields={['name', 'location', 'storageClass']}
        onSubmit={(body) => create.mutateAsync(body)}
        submitLabel="Create"
        onCancel={() => navigate('/gcs')}
        jsonHint={
          <>
            Sent to <span className="font-mono">buckets.insert</span> in Project{' '}
            <span className="font-mono">{project}</span>.
          </>
        }
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field
            id="bucket-name"
            label="Name"
            error={errors.name?.message}
            hint="Globally unique; cannot be changed."
          >
            <Input
              autoComplete="off"
              spellCheck={false}
              {...fieldAria('bucket-name', errors.name?.message, 'Globally')}
              {...form.register('name')}
            />
          </Field>
          <Field
            id="bucket-location"
            label="Location"
            error={errors.location?.message}
            hint="A multi-region (US), dual-region (NAM4) or region (us-east1)."
          >
            <Input
              spellCheck={false}
              {...fieldAria('bucket-location', errors.location?.message, 'A')}
              {...form.register('location')}
            />
          </Field>
        </div>
        <BucketFields form={form} prefix="bucket" />
      </ResourceEditor>
    </Card>
  )
}

// ---- edit ----

export function EditBucket() {
  const name = useBucket()
  const query = useQuery(bucketQuery(name))
  if (!query.data) return <QueryStatus query={query} />
  return <EditBucketForm b={query.data} />
}

function EditBucketForm({ b }: { b: Bucket }) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<EditValues>({
    resolver: zodResolver(editSchema),
    defaultValues: patchValues(b)({}),
  })
  const back = () => navigate(bucketPath(b.name, 'configuration'))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => {
      if (Object.keys(body).length === 0) return false
      await patchBucket(b.name, body)
      return true
    },
    onSuccess: (changed) => {
      void qc.invalidateQueries({ queryKey: ['gcs'] })
      toast.success(changed ? `Updated bucket ${b.name}.` : 'Nothing to change.')
      back()
    },
  })
  return (
    <Card>
      <h2 className="text-lg font-semibold">
        Edit bucket <span className="font-mono">{b.name}</span>
      </h2>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={(v, base) => patchBody(b, v, base)}
        fromBody={patchValues(b)}
        fields={['storageClass', 'labels', 'retentionSeconds']}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">buckets.patch</span> as a JSON merge patch: null
            removes a field.
          </>
        }
      >
        <BucketFields form={form} prefix="edit-bucket" />
      </ResourceEditor>
    </Card>
  )
}
