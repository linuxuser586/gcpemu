import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm, useWatch } from 'react-hook-form'
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
  createDnsAuthorization,
  DNS_AUTH_TYPES,
  dnsAuthorizationQuery,
  locationsQuery,
  patchDnsAuthorization,
  type DnsAuthorization,
  type DnsAuthRef,
} from './api'
import {
  CommonFields,
  editSchema,
  editValues,
  fieldsSchema,
  patchBody,
  patchValues,
  updateMask,
  validHostname,
  type EditValues,
} from './CertificateForm'
import { ChooseProject, dnsAuthPath, useDnsAuthRef } from './CertsLayout'

// Create and edit forms for DNS authorizations (FR-LB-004, FR-UI-011).
// Create sends dnsAuthorizations.create, checked as the Service checks it,
// with its messages; the domain and type cannot change after, so edit
// sends dnsAuthorizations.patch with the labels and description that
// changed.

type Obj = Record<string, unknown>
const obj = (v: unknown): Obj =>
  typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Obj) : {}
const str = (v: unknown) => (typeof v === 'string' ? v : '')

/** normalDomain is a domain as the Service stores it: lower case, without the root dot. */
export const normalDomain = (d: string) => d.trim().toLowerCase().replace(/\.$/, '')

const createSchema = z
  .object({
    id: z.string(),
    location: z.string(),
    domain: z.string(),
    type: z.string(),
    ...fieldsSchema,
  })
  .superRefine((v, ctx) => {
    const issue = (path: string, message: string) =>
      ctx.addIssue({ code: 'custom', path: [path], message })
    if (!CERT_ID.test(v.id)) issue('id', certIdMessage(v.id, 'dnsAuthorizationId'))
    const d = normalDomain(v.domain)
    if (!d) issue('domain', 'Field domain is required.')
    else if (d.startsWith('*.') || !validHostname(d))
      issue(
        'domain',
        `Invalid domain ${JSON.stringify(d)}: must be a fully qualified domain name without a wildcard.`,
      )
    if (v.type === 'FIXED_RECORD' && v.location !== 'global')
      issue(
        'type',
        'DNS authorizations of type FIXED_RECORD are only supported in location "global".',
      )
  })
export type DnsAuthValues = z.infer<typeof createSchema>

/** dnsAuthBody writes the create form's values onto a dnsAuthorizations.create body. */
export function dnsAuthBody(v: DnsAuthValues, base: Body): Body {
  const out: Body = { ...base, domain: normalDomain(v.domain) }
  const set = (k: string, value: unknown, keep: boolean) => {
    if (keep) out[k] = value
    else delete out[k]
  }
  set('type', v.type, !!v.type)
  set('description', v.description, !!v.description)
  const labels = parseLabels(v.labels)
  set('labels', labels, Object.keys(labels).length > 0)
  return out
}

/** dnsAuthValues reads the create form's values out of a body. */
export const dnsAuthValues = (body: Body, prev: DnsAuthValues): DnsAuthValues => ({
  ...prev,
  domain: str(body.domain),
  type: str(body.type),
  description: str(body.description),
  labels: formatLabels(obj(body.labels) as Record<string, string>),
})

export function CreateDnsAuthorization() {
  const [view] = useViewState()
  const project = view.project ?? ''
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<DnsAuthValues>({
    resolver: zodResolver(createSchema),
    defaultValues: {
      id: '',
      location: 'global',
      domain: '',
      type: '',
      description: '',
      labels: '',
    },
  })
  const errors = form.formState.errors
  const location = useWatch({ control: form.control, name: 'location' })
  const locations = useQuery({ ...locationsQuery(project), enabled: !!project })
  const create = useMutation({
    meta: { toast: false },
    mutationFn: async ({ body, values }: { body: Body; values: DnsAuthValues }) => {
      const r: DnsAuthRef = { project, location: values.location, auth: values.id }
      await createDnsAuthorization(r, body)
      return r
    },
    onSuccess: (r) => {
      void qc.invalidateQueries({ queryKey: ['certs'] })
      toast.success(`Created DNS authorization ${r.auth}.`)
      navigate(dnsAuthPath(r))
    },
  })
  if (!project) return <ChooseProject what="create a DNS authorization in it" />
  return (
    <Card>
      <h1 className="text-lg font-semibold">Create DNS authorization</h1>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={dnsAuthBody}
        fromBody={dnsAuthValues}
        fields={['id', 'domain', 'type', 'labels']}
        onSubmit={(body, values) => create.mutateAsync({ body, values })}
        submitLabel="Create"
        onCancel={() => navigate('/certs/dnsAuthorizations')}
        jsonHint={
          <>
            Sent to <span className="font-mono">dnsAuthorizations.create</span> in{' '}
            <span className="font-mono">
              projects/{project}/locations/{location}
            </span>{' '}
            with the ID of the form.
          </>
        }
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id="auth-id" label="Name" error={errors.id?.message} hint="Cannot be changed.">
            <Input
              autoComplete="off"
              spellCheck={false}
              {...fieldAria('auth-id', errors.id?.message, 'hint')}
              {...form.register('id')}
            />
          </Field>
          <Field id="auth-location" label="Location">
            <NativeSelect {...fieldAria('auth-location')} {...form.register('location')}>
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
        <Field
          id="auth-domain"
          label="Domain"
          error={errors.domain?.message}
          hint="The domain to prove control of, without a wildcard: it covers *.domain as well. Cannot be changed."
        >
          <Input
            autoComplete="off"
            spellCheck={false}
            className="font-mono"
            placeholder="example.com"
            {...fieldAria('auth-domain', errors.domain?.message, 'hint')}
            {...form.register('domain')}
          />
        </Field>
        <Field
          id="auth-type"
          label="Type"
          error={errors.type?.message}
          hint="A per-project record lets several Projects authorize the same domain. Cannot be changed."
        >
          <NativeSelect
            {...fieldAria('auth-type', errors.type?.message, 'hint')}
            {...form.register('type')}
          >
            <option value="">Default (fixed record in global, per-project elsewhere)</option>
            {Object.entries(DNS_AUTH_TYPES).map(([v, label]) => (
              <option key={v} value={v}>
                {label}
              </option>
            ))}
          </NativeSelect>
        </Field>
        <CommonFields form={form} prefix="auth" />
      </ResourceEditor>
    </Card>
  )
}

export function EditDnsAuthorization() {
  const ref = useDnsAuthRef()
  const query = useQuery({ ...dnsAuthorizationQuery(ref), enabled: !!ref.project })
  if (!ref.project) return <ChooseProject what="edit its DNS authorizations" />
  if (!query.data) return <QueryStatus query={query} />
  return <EditDnsAuthorizationForm a={query.data} r={ref} />
}

function EditDnsAuthorizationForm({ a, r }: { a: DnsAuthorization; r: DnsAuthRef }) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<EditValues>({
    resolver: zodResolver(editSchema),
    defaultValues: editValues(a),
  })
  const back = () => navigate(dnsAuthPath(r))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => {
      const mask = updateMask(body)
      if (mask.length === 0) return false
      await patchDnsAuthorization(r, body, mask)
      return true
    },
    onSuccess: (changed) => {
      void qc.invalidateQueries({ queryKey: ['certs'] })
      toast.success(changed ? `Updated DNS authorization ${r.auth}.` : 'Nothing to change.')
      back()
    },
  })
  return (
    <Card>
      <h1 className="text-lg font-semibold">
        Edit DNS authorization <span className="font-mono">{r.auth}</span>
      </h1>
      <p className="text-sm text-muted-foreground">
        For domain <span className="font-mono">{a.domain}</span>, with a{' '}
        {(DNS_AUTH_TYPES[a.type ?? ''] ?? a.type ?? 'record').toLowerCase()}. The domain and type
        cannot be changed.
      </p>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={(v, base) => patchBody(a, v, base)}
        fromBody={patchValues(a)}
        fields={['labels']}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">dnsAuthorizations.patch</span> with an updateMask of
            the body&apos;s fields.
          </>
        }
      >
        <CommonFields form={form} prefix="edit-auth" />
      </ResourceEditor>
    </Card>
  )
}
