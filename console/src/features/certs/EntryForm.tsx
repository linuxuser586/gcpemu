import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm, useWatch, type UseFormReturn } from 'react-hook-form'
import { z } from 'zod'

import { useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Field, fieldAria } from '@/components/resource/Field'
import { ResourceEditor, type Body } from '@/components/resource/ResourceEditor'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import { formatLabels, parseLabels } from '@/lib/labels'
import { toast } from '@/lib/toast'

import {
  CERT_ID,
  certDomains,
  certIdMessage,
  certificatesQuery,
  createEntry,
  entryQuery,
  lastSegment,
  locationOf,
  patchEntry,
  type CertificateMapEntry,
  type EntryRef,
} from './api'
import {
  CommonFields,
  editValues,
  fieldsSchema,
  patchBody,
  patchValues,
  updateMask,
  validHostname,
} from './CertificateForm'
import { ChooseProject, mapPath, useEntryRef, useMapRef } from './CertsLayout'
import { matchLabel } from './MapPage'

// Create and edit forms for certificate map entries (FR-LB-004,
// FR-UI-011), reached from the map's page. An entry serves one hostname
// (or a wildcard of it), or is the map's PRIMARY entry for every other
// hostname, with one to four global certificates of scope DEFAULT.
// Create sends certificateMapEntries.create; edit sends
// certificateMapEntries.patch with the certificates, labels and
// description that changed: what it serves cannot change.

type Obj = Record<string, unknown>
const obj = (v: unknown): Obj =>
  typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Obj) : {}
const str = (v: unknown) => (typeof v === 'string' ? v : '')
const strs = (v: unknown) => (Array.isArray(v) ? v.map(str).filter(Boolean) : [])

/** MAX_CERTS is how many certificates an entry can serve. */
const MAX_CERTS = 4

/** normalHostname is a hostname as the API stores it. */
export const normalHostname = (h: string) => h.toLowerCase().replace(/\.$/, '')

const certificatesSchema = z.array(z.string()).superRefine((v, ctx) => {
  if (v.length === 0) ctx.addIssue({ code: 'custom', message: 'Field certificates is required.' })
  if (v.length > MAX_CERTS)
    ctx.addIssue({
      code: 'custom',
      message: 'A certificate map entry supports at most 4 certificates.',
    })
})

const createSchema = z
  .object({
    id: z.string(),
    serves: z.enum(['hostname', 'PRIMARY']),
    hostname: z.string(),
    certificates: certificatesSchema,
    ...fieldsSchema,
  })
  .superRefine((v, ctx) => {
    const issue = (path: string, message: string) =>
      ctx.addIssue({ code: 'custom', path: [path], message })
    if (!CERT_ID.test(v.id)) issue('id', certIdMessage(v.id, 'certificateMapEntryId'))
    if (v.serves === 'hostname') {
      const h = normalHostname(v.hostname)
      if (!h) issue('hostname', 'Exactly one of hostname or matcher must be set.')
      else if (!validHostname(h.replace(/^\*\./, '')))
        issue('hostname', `Invalid hostname ${JSON.stringify(v.hostname)}.`)
    }
  })
export type EntryValues = z.infer<typeof createSchema>

/** entryBody writes the create form's values onto a certificateMapEntries.create body. */
export function entryBody(v: EntryValues, base: Body): Body {
  const out: Body = { ...base }
  delete out.hostname
  delete out.matcher
  if (v.serves === 'PRIMARY') out.matcher = 'PRIMARY'
  else out.hostname = normalHostname(v.hostname)
  out.certificates = v.certificates
  if (v.description) out.description = v.description
  else delete out.description
  const labels = parseLabels(v.labels)
  if (Object.keys(labels).length > 0) out.labels = labels
  else delete out.labels
  return out
}

/** entryValues reads the create form's values out of a CertificateMapEntry body. */
export const entryValues = (body: Body, prev: EntryValues): EntryValues => ({
  ...prev,
  serves: body.matcher === 'PRIMARY' ? 'PRIMARY' : 'hostname',
  hostname: str(body.hostname),
  certificates: strs(body.certificates),
  description: str(body.description),
  labels: formatLabels(obj(body.labels) as Record<string, string>),
})

const editSchema = z.object({ certificates: certificatesSchema, ...fieldsSchema })
type EditEntryValues = z.infer<typeof editSchema>

const editEntryValues = (e: CertificateMapEntry): EditEntryValues => ({
  ...editValues(e),
  certificates: e.certificates ?? [],
})

/** entryPatchBody writes what the edit form changed onto a certificateMapEntries.patch body. */
export function entryPatchBody(e: CertificateMapEntry, v: EditEntryValues, base: Body): Body {
  const out = patchBody(e, v, base)
  if (v.certificates.join() !== (e.certificates ?? []).join()) out.certificates = v.certificates
  else delete out.certificates
  return out
}

const entryPatchValues =
  (e: CertificateMapEntry) =>
  (body: Body): EditEntryValues => ({
    ...patchValues(e)(body),
    certificates: 'certificates' in body ? strs(body.certificates) : (e.certificates ?? []),
  })

/**
 * CertificatesField offers the Project's global certificates of scope
 * DEFAULT, the only ones a map can serve, and any the entry has that are
 * not among them.
 */
function CertificatesField<V extends { certificates: string[] }>({
  form,
  project,
}: {
  form: UseFormReturn<V>
  project: string
}) {
  const f = form as unknown as UseFormReturn<{ certificates: string[] }>
  const query = useQuery({ ...certificatesQuery(project), enabled: !!project })
  const chosen = useWatch({ control: f.control, name: 'certificates' })
  const servable = (query.data ?? []).filter(
    (c) => locationOf(c.name) === 'global' && (c.scope || 'DEFAULT') === 'DEFAULT',
  )
  const options = [
    ...servable.map((c) => ({ name: c.name, domains: certDomains(c) })),
    ...chosen
      .filter((n) => !servable.some((c) => c.name === n))
      .map((n) => ({ name: n, domains: [] as string[] })),
  ]
  const error = f.formState.errors.certificates?.message
  return (
    <fieldset className="flex flex-col gap-1">
      <legend className="text-sm font-medium">Certificates</legend>
      <p className="text-xs text-muted-foreground">
        Up to {MAX_CERTS} global certificates of scope Default; the load balancer chooses among them
        by the client&apos;s key type.
      </p>
      <QueryStatus query={query} />
      {query.data && options.length === 0 && (
        <p className="text-sm text-muted-foreground">
          No global certificates of scope Default in this Project yet.
        </p>
      )}
      {options.map((c) => (
        <label key={c.name} className="flex items-center gap-2 text-sm">
          <input type="checkbox" value={c.name} {...f.register('certificates')} />
          <span className="font-mono">{lastSegment(c.name)}</span>
          {c.domains.length > 0 && (
            <span className="text-muted-foreground">({c.domains.join(', ')})</span>
          )}
        </label>
      ))}
      {error && (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      )}
    </fieldset>
  )
}

// ---- create ----

export function CreateEntry() {
  const map = useMapRef()
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<EntryValues>({
    resolver: zodResolver(createSchema),
    defaultValues: {
      id: '',
      serves: 'hostname',
      hostname: '',
      certificates: [],
      description: '',
      labels: '',
    },
  })
  const errors = form.formState.errors
  const serves = useWatch({ control: form.control, name: 'serves' })
  const back = () => navigate(mapPath(map))
  const create = useMutation({
    meta: { toast: false },
    mutationFn: async ({ body, values }: { body: Body; values: EntryValues }) => {
      const r: EntryRef = { ...map, entry: values.id }
      await createEntry(r, body)
      return r
    },
    onSuccess: (r) => {
      void qc.invalidateQueries({ queryKey: ['certs'] })
      toast.success(`Created certificate map entry ${r.entry}.`)
      back()
    },
  })
  if (!map.project) return <ChooseProject what="add entries to its certificate maps" />
  return (
    <Card>
      <h1 className="text-lg font-semibold">
        Add entry to certificate map <span className="font-mono">{map.map}</span>
      </h1>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={entryBody}
        fromBody={entryValues}
        fields={['id', 'hostname', 'certificates', 'labels']}
        onSubmit={(body, values) => create.mutateAsync({ body, values })}
        submitLabel="Create"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">certificateMapEntries.create</span> in{' '}
            <span className="font-mono">
              projects/{map.project}/locations/global/certificateMaps/{map.map}
            </span>{' '}
            with the ID of the form.
          </>
        }
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id="entry-id" label="Name" error={errors.id?.message} hint="Cannot be changed.">
            <Input
              autoComplete="off"
              spellCheck={false}
              {...fieldAria('entry-id', errors.id?.message, 'hint')}
              {...form.register('id')}
            />
          </Field>
          <Field id="entry-serves" label="Serves" hint="Cannot be changed.">
            <NativeSelect
              {...fieldAria('entry-serves', undefined, 'hint')}
              {...form.register('serves')}
            >
              <option value="hostname">A hostname</option>
              <option value="PRIMARY">Primary: every hostname no other entry serves</option>
            </NativeSelect>
          </Field>
        </div>
        {serves === 'hostname' && (
          <Field
            id="entry-hostname"
            label="Hostname"
            error={errors.hostname?.message}
            hint="e.g. www.example.com, or *.example.com for one label more."
          >
            <Input
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              {...fieldAria('entry-hostname', errors.hostname?.message, 'hint')}
              {...form.register('hostname')}
            />
          </Field>
        )}
        <CertificatesField form={form} project={map.project} />
        <CommonFields form={form} prefix="entry" />
      </ResourceEditor>
    </Card>
  )
}

// ---- edit ----

export function EditEntry() {
  const ref = useEntryRef()
  const query = useQuery({ ...entryQuery(ref), enabled: !!ref.project })
  if (!ref.project) return <ChooseProject what="edit its certificate maps" />
  if (!query.data) return <QueryStatus query={query} />
  return <EditEntryForm e={query.data} r={ref} />
}

function EditEntryForm({ e, r }: { e: CertificateMapEntry; r: EntryRef }) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<EditEntryValues>({
    resolver: zodResolver(editSchema),
    defaultValues: editEntryValues(e),
  })
  const back = () => navigate(mapPath(r))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => {
      const mask = updateMask(body)
      if (mask.length === 0) return false
      await patchEntry(r, body, mask)
      return true
    },
    onSuccess: (changed) => {
      void qc.invalidateQueries({ queryKey: ['certs'] })
      toast.success(changed ? `Updated certificate map entry ${r.entry}.` : 'Nothing to change.')
      back()
    },
  })
  return (
    <Card>
      <h1 className="text-lg font-semibold">
        Edit entry <span className="font-mono">{r.entry}</span>{' '}
        <span className="text-sm font-normal text-muted-foreground">
          of certificate map <span className="font-mono">{r.map}</span>
        </span>
      </h1>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={(v, base) => entryPatchBody(e, v, base)}
        fromBody={entryPatchValues(e)}
        fields={['certificates', 'labels']}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">certificateMapEntries.patch</span> with an
            updateMask of the body&apos;s fields. What the entry serves cannot change: add another.
          </>
        }
      >
        <p className="text-sm">
          <span className="text-muted-foreground">Serves </span>
          <span className="font-mono">{matchLabel(e)}</span>
        </p>
        <CertificatesField form={form} project={r.project} />
        <CommonFields form={form} prefix="edit-entry" />
      </ResourceEditor>
    </Card>
  )
}
