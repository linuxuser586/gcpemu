import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type { ChangeEvent } from 'react'
import { useForm, useWatch, type Resolver, type UseFormReturn } from 'react-hook-form'
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
  CERT_ID,
  CERT_KINDS,
  certIdMessage,
  certificateQuery,
  createCertificate,
  dnsAuthorizationsQuery,
  issuanceConfigsQuery,
  KIND_LABELS,
  lastSegment,
  locationsQuery,
  patchCertificate,
  SCOPES,
  type CertKind,
  type CertRef,
  type Certificate,
  type DnsAuthorization,
} from './api'
import { certPath, ChooseProject, useCertRef } from './CertsLayout'

// Create and edit forms for certificates (FR-LB-004, FR-UI-011). Create
// sends certificates.create with one of the four ways a certificate is
// issued: self-managed (a PEM certificate and its private key, pasted or
// read from a file), or Google-managed through DNS authorizations, load
// balancer authorization or a certificate issuance config. The form checks
// what the Service checks, with its messages. Edit sends
// certificates.patch with the labels and description that changed.

type Obj = Record<string, unknown>
const obj = (v: unknown): Obj =>
  typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Obj) : {}
const str = (v: unknown) => (typeof v === 'string' ? v : '')
const strs = (v: unknown) => (Array.isArray(v) ? v.map(str).filter(Boolean) : [])

/** domainList reads the Domains field as the API normalizes it. */
export const domainList = (text: string) =>
  text
    .split(/[\s,]+/)
    .map((d) => d.trim().toLowerCase().replace(/\.$/, ''))
    .filter(Boolean)

const LABEL = /^[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?$/

/** validHostname is the Service's domain rule: two or more labels of letters, digits and hyphens. */
export function validHostname(h: string): boolean {
  if (!h || h.length > 253 || !h.includes('.')) return false
  return h.split('.').every((l) => LABEL.test(l))
}

/** fieldsSchema checks the fields every resource of the view has. */
export const fieldsSchema = {
  description: z.string(),
  labels: z.string().superRefine((v, ctx) => {
    const err = labelsError(v)
    if (err) ctx.addIssue({ code: 'custom', message: err })
  }),
}

/**
 * createSchema checks a certificate as certificates.create does; authDomains
 * maps the DNS authorizations the form offers to their domains.
 */
const createSchema = (authDomains: Record<string, string>) =>
  z
    .object({
      id: z.string(),
      location: z.string(),
      scope: z.string(),
      kind: z.enum(CERT_KINDS),
      pemCertificate: z.string(),
      pemPrivateKey: z.string(),
      domains: z.string(),
      dnsAuthorizations: z.array(z.string()),
      issuanceConfig: z.string(),
      ...fieldsSchema,
    })
    .superRefine((v, ctx) => {
      const issue = (path: string, message: string) =>
        ctx.addIssue({ code: 'custom', path: [path], message })
      if (!CERT_ID.test(v.id)) issue('id', certIdMessage(v.id))
      if ((v.scope === 'EDGE_CACHE' || v.scope === 'ALL_REGIONS') && v.location !== 'global') {
        issue('scope', `Certificates with scope ${v.scope} must be created in location "global".`)
      }
      if (v.kind === 'self') {
        if (!v.pemCertificate.trim())
          issue('pemCertificate', 'Field self_managed.pem_certificate is required.')
        if (!v.pemPrivateKey.trim())
          issue('pemPrivateKey', 'Field self_managed.pem_private_key is required.')
        return
      }
      const domains = domainList(v.domains)
      if (domains.length === 0) issue('domains', 'Field managed.domains is required.')
      if (domains.length > 100)
        issue('domains', 'A managed certificate supports at most 100 domains.')
      if (v.kind === 'dns' && v.dnsAuthorizations.length === 0)
        issue('dnsAuthorizations', 'Choose at least one DNS authorization.')
      if (v.kind === 'issuance' && !v.issuanceConfig)
        issue('issuanceConfig', 'Choose a certificate issuance config.')
      const covered = new Set(v.dnsAuthorizations.map((a) => authDomains[a]).filter(Boolean))
      for (const d of domains) {
        const [base, wild] = d.startsWith('*.') ? [d.slice(2), true] : [d, false]
        if (!validHostname(base)) {
          issue('domains', `Invalid domain name "${d}".`)
          return
        }
        if (v.kind === 'issuance') continue
        if (wild && v.kind !== 'dns') {
          issue('domains', `Wildcard domain "${d}" requires DNS authorization.`)
          return
        }
        if (v.kind === 'dns' && v.dnsAuthorizations.length > 0 && !covered.has(base)) {
          issue(
            'domains',
            `Domain "${d}" is not covered by any of the provided DNS authorizations.`,
          )
          return
        }
      }
    })
export type CreateValues = z.infer<ReturnType<typeof createSchema>>

export const editSchema = z.object(fieldsSchema)
export type EditValues = z.infer<typeof editSchema>

/** kindOf is how a certificates.create body issues the certificate. */
function kindOf(body: Body, prev: CertKind): CertKind {
  if ('selfManaged' in body) return 'self'
  if (!('managed' in body)) return prev
  const m = obj(body.managed)
  if (m.issuanceConfig) return 'issuance'
  if (strs(m.dnsAuthorizations).length) return 'dns'
  return prev === 'self' ? 'lb' : prev
}

/** certificateValues reads the create form's values out of a Certificate body. */
export function certificateValues(body: Body, prev: CreateValues): CreateValues {
  const sm = obj(body.selfManaged)
  const m = obj(body.managed)
  return {
    ...prev,
    description: str(body.description),
    labels: formatLabels(obj(body.labels) as Record<string, string>),
    scope: str(body.scope) || 'DEFAULT',
    kind: kindOf(body, prev.kind),
    pemCertificate: 'selfManaged' in body ? str(sm.pemCertificate) : prev.pemCertificate,
    pemPrivateKey: 'selfManaged' in body ? str(sm.pemPrivateKey) : prev.pemPrivateKey,
    domains: 'managed' in body ? strs(m.domains).join('\n') : prev.domains,
    dnsAuthorizations: 'managed' in body ? strs(m.dnsAuthorizations) : prev.dnsAuthorizations,
    issuanceConfig: 'managed' in body ? str(m.issuanceConfig) : prev.issuanceConfig,
  }
}

/** certificateBody writes the create form's values onto a certificates.create body. */
export function certificateBody(v: CreateValues, base: Body): Body {
  const out: Body = { ...base }
  const set = (k: string, value: unknown, keep: boolean) => {
    if (keep) out[k] = value
    else delete out[k]
  }
  set('description', v.description, !!v.description)
  const labels = parseLabels(v.labels)
  set('labels', labels, Object.keys(labels).length > 0)
  set('scope', v.scope, !!v.scope && v.scope !== 'DEFAULT')
  if (v.kind === 'self') {
    delete out.managed
    out.selfManaged = {
      ...obj(base.selfManaged),
      pemCertificate: v.pemCertificate,
      pemPrivateKey: v.pemPrivateKey,
    }
    return out
  }
  delete out.selfManaged
  const m: Obj = { ...obj(base.managed), domains: domainList(v.domains) }
  delete m.dnsAuthorizations
  delete m.issuanceConfig
  if (v.kind === 'dns') m.dnsAuthorizations = v.dnsAuthorizations
  if (v.kind === 'issuance') m.issuanceConfig = v.issuanceConfig
  out.managed = m
  return out
}

/** Described is a resource with the fields every edit form edits. */
type Described = Pick<Certificate, 'description' | 'labels'>

/** editValues are the edit form's values for c. */
export const editValues = (c: Described): EditValues => ({
  description: c.description ?? '',
  labels: formatLabels(c.labels),
})

/** patchBody writes what the edit form changed onto a certificates.patch body. */
export function patchBody(c: Described, v: EditValues, base: Body): Body {
  const was = editValues(c)
  const out: Body = { ...base }
  if (v.description !== was.description) out.description = v.description
  else delete out.description
  if (v.labels !== was.labels) out.labels = parseLabels(v.labels)
  else delete out.labels
  return out
}

/** patchValues reads an edit body back into the form, defaulting to c. */
export const patchValues =
  (c: Described) =>
  (body: Body): EditValues => {
    const was = editValues(c)
    return {
      description: 'description' in body ? str(body.description) : was.description,
      labels:
        'labels' in body ? formatLabels(obj(body.labels) as Record<string, string>) : was.labels,
    }
  }

/** updateMask names the body's fields in the API's snake_case. */
export const updateMask = (body: Body) =>
  Object.keys(body).map((k) => k.replace(/[A-Z]/g, (c) => `_${c.toLowerCase()}`))

/** CommonFields are the fields create and edit share. */
export function CommonFields<V extends EditValues>({
  form,
  prefix,
}: {
  form: UseFormReturn<V>
  prefix: string
}) {
  const f = form as unknown as UseFormReturn<EditValues>
  const errors = f.formState.errors
  return (
    <>
      <Field id={`${prefix}-description`} label="Description">
        <Input {...fieldAria(`${prefix}-description`)} {...f.register('description')} />
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
          {...fieldAria(`${prefix}-labels`, errors.labels?.message, 'hint')}
          {...f.register('labels')}
        />
      </Field>
    </>
  )
}

/** PemField is a PEM block pasted into a text area or read from a file. */
function PemField({
  form,
  name,
  label,
  fileLabel,
}: {
  form: UseFormReturn<CreateValues>
  name: 'pemCertificate' | 'pemPrivateKey'
  label: string
  fileLabel: string
}) {
  const error = form.formState.errors[name]?.message
  const id = `cert-${name}`
  const onFile = async (e: ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    if (!file) return
    form.setValue(name, await file.text(), {
      shouldDirty: true,
      shouldValidate: form.formState.isSubmitted,
    })
    e.target.value = ''
  }
  return (
    <div className="flex flex-col gap-2">
      <Field id={id} label={label} error={error} hint="Paste the PEM block, or choose a file.">
        <Textarea
          rows={5}
          spellCheck={false}
          autoComplete="off"
          className="font-mono text-xs"
          {...fieldAria(id, error, 'hint')}
          {...form.register(name)}
        />
      </Field>
      <label htmlFor={`${id}-file`} className="flex flex-col gap-1 text-sm">
        {fileLabel}
        <input
          id={`${id}-file`}
          type="file"
          accept=".pem,.crt,.cer,.key,text/plain,application/x-pem-file"
          className="text-sm file:mr-3 file:rounded-md file:border file:bg-background file:px-3 file:py-1"
          onChange={(e) => void onFile(e)}
        />
      </label>
    </div>
  )
}

// ---- create ----

const createDefaults: CreateValues = {
  id: '',
  location: 'global',
  scope: 'DEFAULT',
  kind: 'self',
  pemCertificate: '',
  pemPrivateKey: '',
  domains: '',
  dnsAuthorizations: [],
  issuanceConfig: '',
  description: '',
  labels: '',
}

export function CreateCertificate() {
  const [view] = useViewState()
  const project = view.project ?? ''
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  // Coverage is checked against the DNS authorizations the form lists.
  const resolver: Resolver<CreateValues> = (values, ctx, opts) => {
    const auths = qc.getQueryData<DnsAuthorization[]>(
      dnsAuthorizationsQuery(project, values.location).queryKey,
    )
    const domains = Object.fromEntries(
      (auths ?? []).map((a) => [a.name, (a.domain ?? '').toLowerCase()]),
    )
    return zodResolver(createSchema(domains))(values, ctx, opts)
  }
  const form = useForm<CreateValues>({ resolver, defaultValues: createDefaults })
  const errors = form.formState.errors
  const location = useWatch({ control: form.control, name: 'location' })
  const kind = useWatch({ control: form.control, name: 'kind' })
  const locations = useQuery({ ...locationsQuery(project), enabled: !!project })
  const auths = useQuery({
    ...dnsAuthorizationsQuery(project, location),
    enabled: !!project && kind === 'dns',
  })
  const configs = useQuery({
    ...issuanceConfigsQuery(project, location),
    enabled: !!project && kind === 'issuance',
  })
  const create = useMutation({
    meta: { toast: false },
    mutationFn: async ({ body, values }: { body: Body; values: CreateValues }) => {
      const r: CertRef = { project, location: values.location, cert: values.id }
      await createCertificate(r, body)
      return r
    },
    onSuccess: (r) => {
      void qc.invalidateQueries({ queryKey: ['certs'] })
      toast.success(`Created certificate ${r.cert}.`)
      navigate(certPath(r))
    },
  })
  if (!project) return <ChooseProject what="create a certificate in it" />
  return (
    <Card>
      <h1 className="text-lg font-semibold">Create certificate</h1>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={certificateBody}
        fromBody={certificateValues}
        fields={['id', 'labels', 'domains', 'pemCertificate', 'pemPrivateKey']}
        onSubmit={(body, values) => create.mutateAsync({ body, values })}
        submitLabel="Create"
        onCancel={() => navigate('/certs')}
        jsonHint={
          <>
            Sent to <span className="font-mono">certificates.create</span> in{' '}
            <span className="font-mono">
              projects/{project}/locations/{location}
            </span>{' '}
            with the ID of the form.
          </>
        }
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id="cert-id" label="Name" error={errors.id?.message} hint="Cannot be changed.">
            <Input
              autoComplete="off"
              spellCheck={false}
              {...fieldAria('cert-id', errors.id?.message, 'hint')}
              {...form.register('id')}
            />
          </Field>
          <Field id="cert-location" label="Location">
            <NativeSelect {...fieldAria('cert-location')} {...form.register('location')}>
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
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id="cert-kind" label="Type">
            <NativeSelect {...fieldAria('cert-kind')} {...form.register('kind')}>
              {CERT_KINDS.map((k) => (
                <option key={k} value={k}>
                  {KIND_LABELS[k]}
                </option>
              ))}
            </NativeSelect>
          </Field>
          <Field id="cert-scope" label="Scope" error={errors.scope?.message}>
            <NativeSelect
              {...fieldAria('cert-scope', errors.scope?.message)}
              {...form.register('scope')}
            >
              {SCOPES.map(([s, label]) => (
                <option key={s} value={s}>
                  {label}
                </option>
              ))}
            </NativeSelect>
          </Field>
        </div>
        {kind === 'self' ? (
          <>
            <PemField
              form={form}
              name="pemCertificate"
              label="PEM certificate"
              fileLabel="Certificate file"
            />
            <PemField
              form={form}
              name="pemPrivateKey"
              label="PEM private key"
              fileLabel="Private key file"
            />
          </>
        ) : (
          <>
            <Field
              id="cert-domains"
              label="Domains"
              error={errors.domains?.message}
              hint={
                kind === 'dns'
                  ? 'One per line. Each must be the domain of a DNS authorization chosen below, or a wildcard of it.'
                  : kind === 'lb'
                    ? 'One per line. Each must resolve to a forwarding rule of the Project; wildcards need DNS authorization.'
                    : 'One per line.'
              }
            >
              <Textarea
                rows={3}
                spellCheck={false}
                className="font-mono"
                {...fieldAria('cert-domains', errors.domains?.message, 'hint')}
                {...form.register('domains')}
              />
            </Field>
            {kind === 'dns' && (
              <fieldset className="flex flex-col gap-1">
                <legend className="text-sm font-medium">DNS authorizations</legend>
                <QueryStatus query={auths} />
                {auths.data?.length === 0 && (
                  <p className="text-sm text-muted-foreground">
                    No DNS authorizations in <span className="font-mono">{location}</span> yet.
                  </p>
                )}
                {(auths.data ?? []).map((a) => (
                  <label key={a.name} className="flex items-center gap-2 text-sm">
                    <input type="checkbox" value={a.name} {...form.register('dnsAuthorizations')} />
                    <span className="font-mono">{lastSegment(a.name)}</span>
                    <span className="text-muted-foreground">({a.domain})</span>
                  </label>
                ))}
                {errors.dnsAuthorizations?.message && (
                  <p role="alert" className="text-sm text-destructive">
                    {errors.dnsAuthorizations.message}
                  </p>
                )}
              </fieldset>
            )}
            {kind === 'issuance' && (
              <Field
                id="cert-issuance"
                label="Certificate issuance config"
                error={errors.issuanceConfig?.message}
                hint="Issued by the Instance CA, without domain authorization."
              >
                <NativeSelect
                  {...fieldAria('cert-issuance', errors.issuanceConfig?.message, 'hint')}
                  {...form.register('issuanceConfig')}
                >
                  <option value="">Choose…</option>
                  {(configs.data ?? []).map((c) => (
                    <option key={c.name} value={c.name}>
                      {lastSegment(c.name)}
                    </option>
                  ))}
                </NativeSelect>
              </Field>
            )}
          </>
        )}
        <CommonFields form={form} prefix="cert" />
      </ResourceEditor>
    </Card>
  )
}

// ---- edit ----

export function EditCertificate() {
  const ref = useCertRef()
  const query = useQuery({ ...certificateQuery(ref), enabled: !!ref.project })
  if (!ref.project) return <ChooseProject what="edit its certificates" />
  if (!query.data) return <QueryStatus query={query} />
  return <EditCertificateForm c={query.data} r={ref} />
}

function EditCertificateForm({ c, r }: { c: Certificate; r: CertRef }) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<EditValues>({
    resolver: zodResolver(editSchema),
    defaultValues: editValues(c),
  })
  const back = () => navigate(certPath(r))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => {
      const mask = updateMask(body)
      if (mask.length === 0) return false
      await patchCertificate(r, body, mask)
      return true
    },
    onSuccess: (changed) => {
      void qc.invalidateQueries({ queryKey: ['certs'] })
      toast.success(changed ? `Updated certificate ${r.cert}.` : 'Nothing to change.')
      back()
    },
  })
  return (
    <Card>
      <h1 className="text-lg font-semibold">
        Edit certificate <span className="font-mono">{r.cert}</span>{' '}
        <span className="text-sm font-normal text-muted-foreground">({r.location})</span>
      </h1>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={(v, base) => patchBody(c, v, base)}
        fromBody={patchValues(c)}
        fields={['labels']}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">certificates.patch</span> with an updateMask of the
            body&apos;s fields. The certificate itself cannot change: create another.
          </>
        }
      >
        <CommonFields form={form} prefix="edit-cert" />
      </ResourceEditor>
    </Card>
  )
}
