import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useParams } from 'react-router'
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
import { useViewState } from '@/lib/viewState'

import {
  CERT_ID,
  certIdMessage,
  certificatesQuery,
  lastSegment,
  locationOf,
  locationsQuery,
  trustConfigsQuery,
} from './api'
import { CommonFields, fieldsSchema, updateMask } from './CertificateForm'
import { ChooseProject, tlsListPath, tlsPath, useTlsRef } from './CertsLayout'
import {
  createTls,
  CLIENT_VALIDATION_LABELS,
  collOf,
  patchTls,
  TLS_KINDS,
  tlsQuery,
  type TlsColl,
  type TlsRef,
  type TlsResource,
} from './tls'

// Create and edit forms for server TLS policies, backend authentication
// configs and client TLS policies (FR-LB-006, FR-UI-011). The form has the
// fields a load balancer uses, checked as the Service checks them, with
// its messages; certificate providers and validation CAs are left to the
// raw JSON editor. Edit sends a patch of the fields that changed.

type Obj = Record<string, unknown>
const obj = (v: unknown): Obj =>
  typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Obj) : {}
const str = (v: unknown) => (typeof v === 'string' ? v : '')

const valuesSchema = z.object({
  id: z.string(),
  location: z.string(),
  clientValidationMode: z.string(),
  trustConfig: z.string(),
  allowOpen: z.boolean(),
  sni: z.string(),
  wellKnownRoots: z.string(),
  clientCertificate: z.string(),
  ...fieldsSchema,
})
export type TlsValues = z.infer<typeof valuesSchema>

/** schema checks a resource of kind coll as the Service does; creating also checks its ID. */
const schema = (coll: TlsColl, creating: boolean) =>
  valuesSchema.superRefine((v, ctx) => {
    const issue = (path: string, message: string) =>
      ctx.addIssue({ code: 'custom', path: [path], message })
    const { idParam } = TLS_KINDS[coll]
    if (creating && !CERT_ID.test(v.id)) issue('id', certIdMessage(v.id, idParam))
    if (
      coll === 'serverTlsPolicies' &&
      v.clientValidationMode === 'REJECT_INVALID' &&
      !v.trustConfig
    )
      issue(
        'trustConfig',
        'mtls_policy.client_validation_trust_config is required when client_validation_mode is REJECT_INVALID.',
      )
    if (
      coll === 'backendAuthenticationConfigs' &&
      !v.trustConfig &&
      v.wellKnownRoots !== 'PUBLIC_ROOTS'
    )
      issue(
        'trustConfig',
        'Field trust_config is required unless well_known_roots is PUBLIC_ROOTS.',
      )
  })

const defaults: TlsValues = {
  id: '',
  location: 'global',
  description: '',
  labels: '',
  clientValidationMode: '',
  trustConfig: '',
  allowOpen: false,
  sni: '',
  wellKnownRoots: '',
  clientCertificate: '',
}

/** EDITABLE are the fields of each kind that the edit form and its JSON body cover. */
const EDITABLE: Record<TlsColl, string[]> = {
  serverTlsPolicies: ['description', 'labels', 'allowOpen', 'serverCertificate', 'mtlsPolicy'],
  backendAuthenticationConfigs: [
    'description',
    'labels',
    'trustConfig',
    'wellKnownRoots',
    'clientCertificate',
  ],
  clientTlsPolicies: ['description', 'labels', 'sni', 'clientCertificate', 'serverValidationCa'],
}

/** tlsBody writes the form's values onto a body of kind coll, keeping the rest of base. */
export function tlsBody(coll: TlsColl, v: TlsValues, base: Body): Body {
  const out: Body = { ...base }
  const set = (k: string, value: unknown, keep: boolean) => {
    if (keep) out[k] = value
    else delete out[k]
  }
  set('description', v.description, !!v.description)
  const labels = parseLabels(v.labels)
  set('labels', labels, Object.keys(labels).length > 0)
  switch (coll) {
    case 'serverTlsPolicies': {
      set('allowOpen', true, v.allowOpen)
      // Without client validation the trust config goes too, but the
      // validation CAs a mesh policy sets in JSON stay.
      const m: Obj = { ...obj(base.mtlsPolicy) }
      delete m.clientValidationMode
      delete m.clientValidationTrustConfig
      if (v.clientValidationMode) {
        m.clientValidationMode = v.clientValidationMode
        if (v.trustConfig) m.clientValidationTrustConfig = v.trustConfig
      }
      set('mtlsPolicy', m, Object.keys(m).length > 0)
      break
    }
    case 'backendAuthenticationConfigs':
      set('wellKnownRoots', v.wellKnownRoots, !!v.wellKnownRoots)
      set('trustConfig', v.trustConfig, !!v.trustConfig)
      set('clientCertificate', v.clientCertificate, !!v.clientCertificate)
      break
    case 'clientTlsPolicies':
      set('sni', v.sni, !!v.sni)
      break
  }
  return out
}

/** tlsValues reads the form's values out of a body of kind coll. */
export function tlsValues(coll: TlsColl, body: Body, prev: TlsValues): TlsValues {
  const m = obj(body.mtlsPolicy)
  return {
    ...prev,
    description: str(body.description),
    labels: formatLabels(obj(body.labels) as Record<string, string>),
    allowOpen: body.allowOpen === true,
    clientValidationMode: str(m.clientValidationMode),
    trustConfig:
      coll === 'serverTlsPolicies' ? str(m.clientValidationTrustConfig) : str(body.trustConfig),
    sni: str(body.sni),
    wellKnownRoots: str(body.wellKnownRoots),
    // A client TLS policy's client certificate is a provider, edited as JSON.
    clientCertificate: str(body.clientCertificate),
  }
}

/**
 * tlsPatch is what body changes of r's editable fields: a patch body of
 * the fields it sets and the updateMask of every field it changes,
 * including those it clears.
 */
export function tlsPatch(
  coll: TlsColl,
  r: TlsResource,
  body: Body,
): { body: Body; mask: string[] } {
  const was = r as unknown as Body
  const keys = new Set([...Object.keys(body), ...EDITABLE[coll].filter((k) => k in was)])
  const out: Body = {}
  for (const k of keys) {
    if (JSON.stringify(body[k]) === JSON.stringify(was[k])) continue
    out[k] = body[k]
  }
  const mask = updateMask(out)
  for (const k of Object.keys(out)) if (out[k] === undefined) delete out[k]
  return { body: out, mask }
}

/** TlsFields are the kind's own fields, shared by create and edit. */
function TlsFields({
  form,
  coll,
  project,
  location,
}: {
  form: UseFormReturn<TlsValues>
  coll: TlsColl
  project: string
  location: string
}) {
  const errors = form.formState.errors
  const mode = useWatch({ control: form.control, name: 'clientValidationMode' })
  const trust = useWatch({ control: form.control, name: 'trustConfig' })
  const cert = useWatch({ control: form.control, name: 'clientCertificate' })
  const usesTrust =
    coll === 'backendAuthenticationConfigs' || (coll === 'serverTlsPolicies' && !!mode)
  const trusts = useQuery({
    ...trustConfigsQuery(project, location),
    enabled: !!project && usesTrust,
  })
  const certs = useQuery({
    ...certificatesQuery(project),
    enabled: !!project && coll === 'backendAuthenticationConfigs',
  })
  // A name the list lacks, such as another Project's, is kept.
  const withCurrent = (names: string[], cur: string) =>
    cur && !names.includes(cur) ? [...names, cur] : names
  const trustOptions = withCurrent(
    (trusts.data ?? []).map((t) => t.name),
    trust,
  )
  const certOptions = withCurrent(
    (certs.data ?? [])
      .filter((c) => c.scope === 'CLIENT_AUTH' && locationOf(c.name) === location)
      .map((c) => c.name),
    cert,
  )
  const trustField = (
    <Field
      id="tls-trust"
      label="Trust config"
      error={errors.trustConfig?.message}
      hint={
        coll === 'serverTlsPolicies'
          ? 'The CAs that client certificates must chain to.'
          : 'The CAs that backend certificates must chain to.'
      }
    >
      <NativeSelect
        className="font-mono"
        {...fieldAria('tls-trust', errors.trustConfig?.message, 'hint')}
        {...form.register('trustConfig')}
      >
        <option value="">None</option>
        {trustOptions.map((n) => (
          <option key={n} value={n}>
            {lastSegment(n)}
          </option>
        ))}
      </NativeSelect>
    </Field>
  )
  switch (coll) {
    case 'serverTlsPolicies':
      return (
        <>
          <Field
            id="tls-mode"
            label="Client validation"
            hint="Mutual TLS: how the load balancer checks client certificates."
          >
            <NativeSelect
              {...fieldAria('tls-mode', undefined, 'hint')}
              {...form.register('clientValidationMode')}
            >
              {Object.entries(CLIENT_VALIDATION_LABELS).map(([v, label]) => (
                <option key={v} value={v}>
                  {label}
                </option>
              ))}
            </NativeSelect>
          </Field>
          {mode && trustField}
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" {...form.register('allowOpen')} />
            Allow open (accept plaintext as well as TLS; Cloud Service Mesh only)
          </label>
        </>
      )
    case 'backendAuthenticationConfigs':
      return (
        <>
          {trustField}
          <Field
            id="tls-roots"
            label="Well-known roots"
            hint="Public roots trusts the public CAs as well as the trust config."
          >
            <NativeSelect
              {...fieldAria('tls-roots', undefined, 'hint')}
              {...form.register('wellKnownRoots')}
            >
              <option value="">Unspecified</option>
              <option value="NONE">NONE</option>
              <option value="PUBLIC_ROOTS">PUBLIC_ROOTS</option>
            </NativeSelect>
          </Field>
          <Field
            id="tls-client-cert"
            label="Client certificate"
            hint="Presented to backends that ask for one: a certificate of scope Client authentication."
          >
            <NativeSelect
              className="font-mono"
              {...fieldAria('tls-client-cert', undefined, 'hint')}
              {...form.register('clientCertificate')}
            >
              <option value="">None</option>
              {certOptions.map((n) => (
                <option key={n} value={n}>
                  {lastSegment(n)}
                </option>
              ))}
            </NativeSelect>
          </Field>
        </>
      )
    case 'clientTlsPolicies':
      return (
        <Field
          id="tls-sni"
          label="SNI"
          hint="The server name sent in the TLS handshake. Certificates and CAs are set in JSON."
        >
          <Input
            autoComplete="off"
            spellCheck={false}
            className="font-mono"
            {...fieldAria('tls-sni', undefined, 'hint')}
            {...form.register('sni')}
          />
        </Field>
      )
  }
}

const FIELDS = ['id', 'labels', 'trustConfig', 'sni', 'clientCertificate'] as const

/** CreateTlsPolicy creates a resource of the route's kind (:coll). */
export function CreateTlsPolicy() {
  const params = useParams()
  const coll = collOf(params.coll)
  const kind = TLS_KINDS[coll]
  const [view] = useViewState()
  const project = view.project ?? ''
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<TlsValues>({
    resolver: zodResolver(schema(coll, true)),
    defaultValues: defaults,
  })
  const errors = form.formState.errors
  const location = useWatch({ control: form.control, name: 'location' })
  const locations = useQuery({ ...locationsQuery(project), enabled: !!project })
  const create = useMutation({
    meta: { toast: false },
    mutationFn: async ({ body, values }: { body: Body; values: TlsValues }) => {
      const r: TlsRef = { project, location: values.location, coll, id: values.id }
      await createTls(r, body)
      return r
    },
    onSuccess: (r) => {
      void qc.invalidateQueries({ queryKey: ['certs'] })
      toast.success(`Created ${kind.singular} ${r.id}.`)
      navigate(tlsPath(r))
    },
  })
  if (!project) return <ChooseProject what={`create a ${kind.singular} in it`} />
  return (
    <Card>
      <h1 className="text-lg font-semibold">Create {kind.singular}</h1>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={(v, base) => tlsBody(coll, v, base)}
        fromBody={(body, prev) => tlsValues(coll, body, prev)}
        fields={FIELDS}
        onSubmit={(body, values) => create.mutateAsync({ body, values })}
        submitLabel="Create"
        onCancel={() => navigate(tlsListPath(coll))}
        jsonHint={
          <>
            Sent to <span className="font-mono">{coll}.create</span> in{' '}
            <span className="font-mono">
              projects/{project}/locations/{location}
            </span>{' '}
            with the ID of the form.
          </>
        }
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id="tls-id" label="Name" error={errors.id?.message} hint="Cannot be changed.">
            <Input
              autoComplete="off"
              spellCheck={false}
              {...fieldAria('tls-id', errors.id?.message, 'hint')}
              {...form.register('id')}
            />
          </Field>
          <Field id="tls-location" label="Location">
            <NativeSelect {...fieldAria('tls-location')} {...form.register('location')}>
              <option value="global">global</option>
              {(locations.data ?? [])
                .filter((l) => l.locationId !== 'global')
                .map((l) => (
                  <option key={l.locationId} value={l.locationId}>
                    {l.locationId}
                  </option>
                ))}
            </NativeSelect>
          </Field>
        </div>
        <CommonFields form={form} prefix="tls" />
        <TlsFields form={form} coll={coll} project={project} location={location} />
      </ResourceEditor>
    </Card>
  )
}

/** EditTlsPolicy edits a resource of kind coll. */
export function EditTlsPolicy({ coll }: { coll: TlsColl }) {
  const ref = useTlsRef(coll)
  const query = useQuery({ ...tlsQuery(ref), enabled: !!ref.project })
  if (!ref.project)
    return <ChooseProject what={`edit its ${TLS_KINDS[coll].title.toLowerCase()}`} />
  if (!query.data) return <QueryStatus query={query} />
  return <EditTlsForm r={query.data} tref={ref} />
}

function EditTlsForm({ r, tref }: { r: TlsResource; tref: TlsRef }) {
  const { coll } = tref
  const kind = TLS_KINDS[coll]
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  // The body is the resource's editable fields, so that JSON shows them all.
  const was = r as unknown as Body
  const initialBody: Body = Object.fromEntries(
    EDITABLE[coll].filter((k) => k in was).map((k) => [k, was[k]]),
  )
  const form = useForm<TlsValues>({
    resolver: zodResolver(schema(coll, false)),
    defaultValues: tlsValues(coll, initialBody, { ...defaults, location: tref.location }),
  })
  const back = () => navigate(tlsPath(tref))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => {
      const p = tlsPatch(coll, r, body)
      if (p.mask.length === 0) return false
      await patchTls(tref, p.body, p.mask)
      return true
    },
    onSuccess: (changed) => {
      void qc.invalidateQueries({ queryKey: ['certs'] })
      toast.success(changed ? `Updated ${kind.singular} ${tref.id}.` : 'Nothing to change.')
      back()
    },
  })
  return (
    <Card>
      <h1 className="text-lg font-semibold">
        Edit {kind.singular} <span className="font-mono">{tref.id}</span>
      </h1>
      <ResourceEditor
        form={form}
        initialBody={initialBody}
        toBody={(v, base) => tlsBody(coll, v, base)}
        fromBody={(body, prev) => tlsValues(coll, body, prev)}
        fields={FIELDS}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">{coll}.patch</span> with an updateMask of the fields
            that changed; a field taken out is cleared.
          </>
        }
      >
        <CommonFields form={form} prefix="edit-tls" />
        <TlsFields form={form} coll={coll} project={tref.project} location={tref.location} />
      </ResourceEditor>
    </Card>
  )
}
