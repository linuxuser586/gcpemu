import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Plus } from 'lucide-react'
import { useForm, useWatch, type Path, type UseFormReturn } from 'react-hook-form'
import { z } from 'zod'

import { errorMessage } from '@/api/errors'
import { infoQuery } from '@/api/queries'
import { useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Field, fieldAria } from '@/components/resource/Field'
import { ResourceEditor, type Body } from '@/components/resource/ResourceEditor'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import { formatLabels, labelsError, parseLabels } from '@/lib/labels'
import { toast } from '@/lib/toast'
import { useViewState } from '@/lib/viewState'

import {
  apiField,
  createZone,
  invalid,
  isDnsName,
  networkPath,
  networksQuery,
  patchZone,
  required,
  ZONE_NAME,
  zoneNetworks,
  zoneQuery,
  type ManagedZone,
  type ZoneRef,
} from './api'
import { ChooseProject, useZoneRef, zonePath } from './DnsLayout'

// Create and edit forms for managed zones (FR-DNS-001, FR-UI-011): create
// sends managedZones.create; edit sends managedZones.patch with only the
// fields that changed. A private zone's network bindings are its
// privateVisibilityConfig. DNSSEC, forwarding, peering and logging are in
// the JSON editor.

type Obj = Record<string, unknown>
const obj = (v: unknown): Obj =>
  typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Obj) : {}
const str = (v: unknown) => (typeof v === 'string' ? v : '')
const lines = (text: string) =>
  text
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter(Boolean)

const NETWORK = /^(https:\/\/[^/]+\/compute\/v1\/)?projects\/[^/]+\/global\/networks\/[^/]+$/
const NETWORK_NAME = /^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$/

const F = 'entity.managedZone'

/** networkUrls turns the networks field into networkUrls: a bare name is in project. */
export const networkUrls = (text: string, project: string) =>
  lines(text).map((n) =>
    NETWORK_NAME.test(n)
      ? `https://www.googleapis.com/compute/v1/projects/${project}/global/networks/${n}`
      : NETWORK.test(n) && !n.startsWith('https://')
        ? `https://www.googleapis.com/compute/v1/${n}`
        : n,
  )

const fieldsSchema = {
  description: z.string().superRefine((v, ctx) => {
    if (v.length > 1024) ctx.addIssue({ code: 'custom', message: invalid(`${F}.description`, v) })
  }),
  labels: z.string().superRefine((v, ctx) => {
    const err = labelsError(v)
    if (err) ctx.addIssue({ code: 'custom', message: err })
  }),
  networks: z.string().superRefine((v, ctx) => {
    lines(v).forEach((n, i) => {
      if (!NETWORK_NAME.test(n) && !NETWORK.test(n)) {
        ctx.addIssue({
          code: 'custom',
          message: invalid(`${F}.privateVisibilityConfig.networks[${i}].networkUrl`, n),
        })
      }
    })
  }),
}

const createSchema = z.object({
  name: z.string().superRefine((v, ctx) => {
    if (!v) ctx.addIssue({ code: 'custom', message: required(`${F}.name`) })
    else if (!ZONE_NAME.test(v)) ctx.addIssue({ code: 'custom', message: invalid(`${F}.name`, v) })
  }),
  dnsName: z.string().superRefine((v, ctx) => {
    if (!v) ctx.addIssue({ code: 'custom', message: required(`${F}.dnsName`) })
    else if (!isDnsName(v)) ctx.addIssue({ code: 'custom', message: invalid(`${F}.dnsName`, v) })
  }),
  visibility: z.enum(['public', 'private']),
  ...fieldsSchema,
})
export type CreateValues = z.infer<typeof createSchema>

const editSchema = z.object(fieldsSchema)
export type EditValues = z.infer<typeof editSchema>

/** API fields the forms show errors on. */
const FIELDS = [
  [`${F}.name`, 'name'],
  [`${F}.dnsName`, 'dnsName'],
  [`${F}.description`, 'description'],
  [`${F}.visibility`, 'visibility'],
  [`${F}.privateVisibilityConfig`, 'networks'],
  [`${F}.labels`, 'labels'],
  // "The resource 'entity.managedZone' named 'N' already exists"
  [/^entity\.managedZone$/, 'name'],
] as const

/**
 * withFieldError shows an API error on the field it names, then rethrows
 * it for the editor to show above the buttons.
 */
async function withFieldError<V extends Obj>(form: UseFormReturn<V>, send: () => Promise<unknown>) {
  try {
    return await send()
  } catch (e) {
    const f = apiField(errorMessage(e), FIELDS)
    if (f && f in form.getValues()) {
      form.setError(f as Path<V>, { type: 'server', message: errorMessage(e) })
    }
    throw e
  }
}

const bodyNetworks = (body: Body) =>
  (Array.isArray(obj(body.privateVisibilityConfig).networks)
    ? (obj(body.privateVisibilityConfig).networks as unknown[])
    : []
  )
    .map((n) => networkPath(str(obj(n).networkUrl)))
    .join('\n')

/** zoneValues reads the create form's values out of a ManagedZone body. */
export function zoneValues(body: Body, prev: CreateValues): CreateValues {
  return {
    ...prev,
    name: str(body.name),
    dnsName: str(body.dnsName),
    description: str(body.description),
    visibility: body.visibility === 'private' ? 'private' : 'public',
    networks: bodyNetworks(body),
    labels: formatLabels(obj(body.labels) as Record<string, string>),
  }
}

/** zoneBody writes the create form's values onto a managedZones.create body. */
export function zoneBody(project: string) {
  return (v: CreateValues, base: Body): Body => {
    const out: Body = { ...base, name: v.name, dnsName: v.dnsName, visibility: v.visibility }
    if (v.description) out.description = v.description
    else delete out.description
    const labels = parseLabels(v.labels)
    if (Object.keys(labels).length > 0) out.labels = labels
    else delete out.labels
    if (v.visibility === 'private') {
      out.privateVisibilityConfig = {
        ...obj(base.privateVisibilityConfig),
        networks: networkUrls(v.networks, project).map((networkUrl) => ({ networkUrl })),
      }
    } else {
      delete out.privateVisibilityConfig
    }
    return out
  }
}

/** editValues are the edit form's values for z. */
export const editValues = (z: ManagedZone): EditValues => ({
  description: z.description ?? '',
  labels: formatLabels(z.labels),
  networks: zoneNetworks(z).join('\n'),
})

/** patchBody writes what the edit form changed onto a managedZones.patch body. */
export function patchBody(zone: ManagedZone, project: string) {
  return (v: EditValues, base: Body): Body => {
    const was = editValues(zone)
    const out: Body = { ...base }
    const set = (k: string, changed: boolean, value: unknown) => {
      if (changed) out[k] = value
      else delete out[k]
    }
    set('description', v.description !== was.description, v.description)
    set('labels', v.labels !== was.labels, parseLabels(v.labels))
    set(
      'privateVisibilityConfig',
      zone.visibility === 'private' &&
        networkUrls(v.networks, project).map(networkPath).join('\n') !== was.networks,
      {
        ...obj(zone.privateVisibilityConfig),
        networks: networkUrls(v.networks, project).map((networkUrl) => ({ networkUrl })),
      },
    )
    return out
  }
}

/** patchValues reads an edit body back into the form, defaulting to z. */
export const patchValues =
  (zone: ManagedZone) =>
  (body: Body): EditValues => {
    const was = editValues(zone)
    return {
      description: 'description' in body ? str(body.description) : was.description,
      labels:
        'labels' in body ? formatLabels(obj(body.labels) as Record<string, string>) : was.labels,
      networks: 'privateVisibilityConfig' in body ? bodyNetworks(body) : was.networks,
    }
  }

/** NetworksField edits a private zone's network bindings. */
function NetworksField({
  form,
  id,
  project,
}: {
  form: UseFormReturn<EditValues> | UseFormReturn<CreateValues>
  id: string
  project: string
}) {
  const f = form as UseFormReturn<EditValues>
  const error = f.formState.errors.networks?.message
  const text = useWatch({ control: f.control, name: 'networks' })
  const { data: info } = useQuery(infoQuery())
  const compute = !!info?.services.includes('compute')
  const known = useQuery({ ...networksQuery(project), enabled: compute && !!project })
  const bound = new Set(lines(text ?? '').map((n) => networkPath(n).split('/').pop()))
  const hint =
    'A network name in this Project, or projects/PROJECT/global/networks/NETWORK, one per line. Only queries from these networks see the zone.'
  return (
    <Field id={id} label="Networks" error={error} hint={hint}>
      <Textarea
        rows={3}
        spellCheck={false}
        className="font-mono"
        {...fieldAria(id, error, hint)}
        {...f.register('networks')}
      />
      {(known.data ?? []).some((n) => !bound.has(n.name)) && (
        <div className="flex flex-wrap gap-2">
          {(known.data ?? [])
            .filter((n) => !bound.has(n.name))
            .map((n) => (
              <Button
                key={n.name}
                type="button"
                variant="outline"
                size="sm"
                aria-label={`Bind network ${n.name}`}
                onClick={() =>
                  f.setValue('networks', [...lines(text ?? ''), n.name].join('\n'), {
                    shouldDirty: true,
                  })
                }
              >
                <Plus aria-hidden />
                {n.name}
              </Button>
            ))}
        </div>
      )}
    </Field>
  )
}

/** SharedFields are the fields create and edit share. */
function SharedFields({
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
      <Field id={`${prefix}-description`} label="Description" error={errors.description?.message}>
        <Input
          {...fieldAria(`${prefix}-description`, errors.description?.message)}
          {...f.register('description')}
        />
      </Field>
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

const createDefaults: CreateValues = {
  name: '',
  dnsName: '',
  description: '',
  visibility: 'public',
  networks: '',
  labels: '',
}

export function CreateZone() {
  const [view] = useViewState()
  const project = view.project ?? ''
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<CreateValues>({
    resolver: zodResolver(createSchema),
    defaultValues: createDefaults,
  })
  const errors = form.formState.errors
  const visibility = useWatch({ control: form.control, name: 'visibility' })
  const create = useMutation({
    meta: { toast: false },
    mutationFn: (body: Body) => withFieldError(form, () => createZone(project, body)),
    onSuccess: (_, body) => {
      void qc.invalidateQueries({ queryKey: ['dns'] })
      toast.success(`Created zone ${str(body.name)}.`)
      navigate(zonePath(str(body.name)))
    },
  })
  if (!project) return <ChooseProject what="create a managed zone in it" />
  const toBody = zoneBody(project)
  return (
    <Card>
      <h1 className="text-lg font-semibold">Create zone</h1>
      <ResourceEditor
        form={form}
        initialBody={toBody(form.getValues(), {})}
        toBody={toBody}
        fromBody={zoneValues}
        fields={['name', 'dnsName', 'description', 'labels', 'networks']}
        onSubmit={(body) => create.mutateAsync(body)}
        submitLabel="Create"
        onCancel={() => navigate('/dns')}
        jsonHint={
          <>
            Sent to <span className="font-mono">managedZones.create</span> in{' '}
            <span className="font-mono">projects/{project}</span>.
          </>
        }
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field
            id="zone-name"
            label="Zone name"
            error={errors.name?.message}
            hint="Lowercase letters, digits and dashes. Cannot be changed."
          >
            <Input
              autoComplete="off"
              spellCheck={false}
              {...fieldAria('zone-name', errors.name?.message, 'Lowercase')}
              {...form.register('name')}
            />
          </Field>
          <Field
            id="zone-dns-name"
            label="DNS name"
            error={errors.dnsName?.message}
            hint="Ends with a dot, e.g. example.com. Cannot be changed."
          >
            <Input
              autoComplete="off"
              spellCheck={false}
              {...fieldAria('zone-dns-name', errors.dnsName?.message, 'Ends')}
              {...form.register('dnsName')}
            />
          </Field>
        </div>
        <Field
          id="zone-visibility"
          label="Visibility"
          hint="Public zones answer everyone; private ones only their networks. Cannot be changed."
        >
          <NativeSelect
            {...fieldAria('zone-visibility', undefined, 'Public')}
            {...form.register('visibility')}
          >
            <option value="public">Public</option>
            <option value="private">Private</option>
          </NativeSelect>
        </Field>
        {visibility === 'private' && (
          <NetworksField form={form} id="zone-networks" project={project} />
        )}
        <SharedFields form={form} prefix="zone" />
      </ResourceEditor>
    </Card>
  )
}

// ---- edit ----

export function EditZone() {
  const ref = useZoneRef()
  const query = useQuery({ ...zoneQuery(ref), enabled: !!ref.project })
  if (!ref.project) return <ChooseProject what="edit its managed zones" />
  if (!query.data) return <QueryStatus query={query} />
  return <EditZoneForm zone={query.data} r={ref} />
}

function EditZoneForm({ zone, r }: { zone: ManagedZone; r: ZoneRef }) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<EditValues>({
    resolver: zodResolver(editSchema),
    defaultValues: editValues(zone),
  })
  const back = () => navigate(zonePath(r.zone, 'overview'))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => {
      if (Object.keys(body).length === 0) return false
      await withFieldError(form, () => patchZone(r, body))
      return true
    },
    onSuccess: (changed) => {
      void qc.invalidateQueries({ queryKey: ['dns'] })
      toast.success(changed ? `Updated zone ${r.zone}.` : 'Nothing to change.')
      back()
    },
  })
  return (
    <Card>
      <h1 className="text-lg font-semibold">
        Edit zone <span className="font-mono">{r.zone}</span>{' '}
        <span className="text-sm font-normal text-muted-foreground">({zone.dnsName})</span>
      </h1>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={patchBody(zone, r.project)}
        fromBody={patchValues(zone)}
        fields={['description', 'labels', 'networks']}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">managedZones.patch</span>: the fields in the body
            replace the zone&apos;s.
          </>
        }
      >
        {zone.visibility === 'private' && (
          <NetworksField form={form} id="edit-zone-networks" project={r.project} />
        )}
        <SharedFields form={form} prefix="edit-zone" />
      </ResourceEditor>
    </Card>
  )
}
