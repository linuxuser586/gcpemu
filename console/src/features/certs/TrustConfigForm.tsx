import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Plus, Trash2 } from 'lucide-react'
import { useState, type ChangeEvent } from 'react'
import { useFieldArray, useForm, useWatch, type UseFormReturn } from 'react-hook-form'
import { z } from 'zod'

import { useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Field, fieldAria } from '@/components/resource/Field'
import { ResourceEditor, type Body } from '@/components/resource/ResourceEditor'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import { formatLabels, parseLabels } from '@/lib/labels'
import { toast } from '@/lib/toast'
import { useViewState } from '@/lib/viewState'

import {
  CERT_ID,
  certIdMessage,
  createTrustConfig,
  locationsQuery,
  patchTrustConfig,
  trustConfigQuery,
  type TrustConfig,
  type TrustRef,
} from './api'
import { CommonFields, fieldsSchema, updateMask } from './CertificateForm'
import { ChooseProject, trustPath, useTrustRef } from './CertsLayout'
import { parsePem, pemError } from './pem'
import { PemSummary } from './PemSummary'

// Create and edit forms for trust configs (FR-LB-009, FR-UI-011). The
// form has the one trust store the Service supports: its trust anchors
// and intermediate CAs, each pasted or read from a file, with the
// subject and expiry of each certificate shown as it is entered and
// checked as the Service checks it. Allowlisted certificates are left to
// the raw JSON editor. Edit sends a patch of the fields that changed with
// the etag the form read, so that the API refuses it when the trust
// config changed since.

type Obj = Record<string, unknown>
const obj = (v: unknown): Obj =>
  typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Obj) : {}
const str = (v: unknown) => (typeof v === 'string' ? v : '')
const arr = (v: unknown): unknown[] => (Array.isArray(v) ? v : [])

/** ROLES are the trust store's lists the form edits, with the Service's field names. */
const ROLES = {
  anchors: {
    key: 'trustAnchors',
    field: 'trust_stores.trust_anchors.pem_certificate',
    one: 'trust anchor',
    title: 'Trust anchors',
    hint: 'The root CAs that certificates must chain to.',
  },
  intermediates: {
    key: 'intermediateCas',
    field: 'trust_stores.intermediate_cas.pem_certificate',
    one: 'intermediate CA',
    title: 'Intermediate CAs',
    hint: 'CAs between a trust anchor and the certificates it verifies, for peers that do not send them.',
  },
} as const
type Role = keyof typeof ROLES

const pems = z.array(z.object({ pem: z.string() }))

const valuesSchema = z.object({
  id: z.string(),
  location: z.string(),
  anchors: pems,
  intermediates: pems,
  ...fieldsSchema,
})
export type TrustValues = z.infer<typeof valuesSchema>

/** schema checks a trust config as the Service does; creating also checks its ID. */
const schema = (creating: boolean) =>
  valuesSchema.superRefine((v, ctx) => {
    if (creating && !CERT_ID.test(v.id)) {
      ctx.addIssue({ code: 'custom', path: ['id'], message: certIdMessage(v.id, 'trustConfigId') })
    }
    for (const role of Object.keys(ROLES) as Role[]) {
      v[role].forEach(({ pem }, i) => {
        const message = pem.trim() ? pemError(ROLES[role].field, pem) : undefined
        if (message) ctx.addIssue({ code: 'custom', path: [role, i, 'pem'], message })
      })
    }
  })

const defaults: TrustValues = {
  id: '',
  location: 'global',
  description: '',
  labels: '',
  anchors: [{ pem: '' }],
  intermediates: [],
}

/** EDITABLE are the fields the edit form and its JSON body cover. */
const EDITABLE = ['description', 'labels', 'trustStores', 'allowlistedCertificates']

/**
 * trustBody writes the form's values onto a trust config body, keeping
 * the rest of base, such as allowlisted certificates set in JSON.
 */
export function trustBody(v: TrustValues, base: Body): Body {
  const out: Body = { ...base }
  if (v.description) out.description = v.description
  else delete out.description
  const labels = parseLabels(v.labels)
  if (Object.keys(labels).length > 0) out.labels = labels
  else delete out.labels
  const store: Obj = { ...obj(arr(base.trustStores)[0]) }
  for (const role of Object.keys(ROLES) as Role[]) {
    const { key } = ROLES[role]
    const list = v[role].filter((p) => p.pem.trim()).map((p) => ({ pemCertificate: p.pem }))
    if (list.length > 0) store[key] = list
    else delete store[key]
  }
  if (Object.keys(store).length > 0) out.trustStores = [store]
  else delete out.trustStores
  return out
}

/** trustValues reads the form's values out of a trust config body. */
export function trustValues(body: Body, prev: TrustValues): TrustValues {
  const store = obj(arr(body.trustStores)[0])
  const list = (key: string) => arr(store[key]).map((p) => ({ pem: str(obj(p).pemCertificate) }))
  return {
    ...prev,
    description: str(body.description),
    labels: formatLabels(obj(body.labels) as Record<string, string>),
    anchors: list('trustAnchors'),
    intermediates: list('intermediateCas'),
  }
}

/**
 * trustPatch is what body changes of t's editable fields: a patch body of
 * the fields it sets, with t's etag, and the updateMask of every field it
 * changes, including those it clears.
 */
export function trustPatch(t: TrustConfig, body: Body): { body: Body; mask: string[] } {
  const was = t as unknown as Body
  const keys = new Set([...Object.keys(body), ...EDITABLE.filter((k) => k in was)])
  const out: Body = {}
  for (const k of keys) {
    if (k === 'etag' || JSON.stringify(body[k]) === JSON.stringify(was[k])) continue
    out[k] = body[k]
  }
  const mask = updateMask(out)
  for (const k of Object.keys(out)) if (out[k] === undefined) delete out[k]
  return { body: { etag: t.etag, ...out }, mask }
}

/** PemList is one of the trust store's lists of certificates, each pasted or read from a file. */
function PemList({ form, list: role }: { form: UseFormReturn<TrustValues>; list: Role }) {
  const { one, title, hint } = ROLES[role]
  const { fields, append, remove } = useFieldArray({ control: form.control, name: role })
  const values = useWatch({ control: form.control, name: role })
  const errors = form.formState.errors[role]
  const Cap = one.charAt(0).toUpperCase() + one.slice(1)
  const onFile = async (i: number, e: ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    if (!file) return
    form.setValue(`${role}.${i}.pem`, await file.text(), {
      shouldDirty: true,
      shouldValidate: form.formState.isSubmitted,
    })
    e.target.value = ''
  }
  return (
    <fieldset className="flex flex-col gap-3 rounded-md border p-3">
      <legend className="px-1 text-sm font-medium">{title}</legend>
      <p className="text-xs text-muted-foreground">{hint}</p>
      {fields.map((f, i) => {
        const id = `trust-${role}-${i}`
        const error = errors?.[i]?.pem?.message
        const pem = values?.[i]?.pem ?? ''
        return (
          <div key={f.id} className="flex flex-col gap-2">
            <Field
              id={id}
              label={`${Cap} ${i + 1}`}
              error={error}
              hint="Paste the PEM block, or choose a file."
            >
              <Textarea
                rows={4}
                spellCheck={false}
                autoComplete="off"
                className="font-mono text-xs"
                {...fieldAria(id, error, 'hint')}
                {...form.register(`${role}.${i}.pem`)}
              />
            </Field>
            {pem.trim() && !parsePem(pem).error && <PemSummary pem={pem} />}
            <div className="flex flex-wrap items-end justify-between gap-2">
              <label htmlFor={`${id}-file`} className="flex flex-col gap-1 text-sm">
                Read {one} {i + 1} from a file
                <input
                  id={`${id}-file`}
                  type="file"
                  accept=".pem,.crt,.cer,text/plain,application/x-pem-file"
                  className="text-sm file:mr-3 file:rounded-md file:border file:bg-background file:px-3 file:py-1"
                  onChange={(e) => void onFile(i, e)}
                />
              </label>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                aria-label={`Remove ${one} ${i + 1}`}
                onClick={() => remove(i)}
              >
                <Trash2 aria-hidden />
                Remove
              </Button>
            </div>
          </div>
        )
      })}
      <div>
        <Button type="button" variant="outline" size="sm" onClick={() => append({ pem: '' })}>
          <Plus aria-hidden />
          Add {one}
        </Button>
      </div>
    </fieldset>
  )
}

const FIELDS = ['id', 'labels'] as const

export function CreateTrustConfig() {
  const [view] = useViewState()
  const project = view.project ?? ''
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<TrustValues>({
    resolver: zodResolver(schema(true)),
    defaultValues: defaults,
  })
  const errors = form.formState.errors
  const location = useWatch({ control: form.control, name: 'location' })
  const locations = useQuery({ ...locationsQuery(project), enabled: !!project })
  const create = useMutation({
    meta: { toast: false },
    mutationFn: async ({ body, values }: { body: Body; values: TrustValues }) => {
      const r: TrustRef = { project, location: values.location, trust: values.id }
      await createTrustConfig(r, body)
      return r
    },
    onSuccess: (r) => {
      void qc.invalidateQueries({ queryKey: ['certs'] })
      toast.success(`Created trust config ${r.trust}.`)
      navigate(trustPath(r))
    },
  })
  if (!project) return <ChooseProject what="create a trust config in it" />
  return (
    <Card>
      <h1 className="text-lg font-semibold">Create trust config</h1>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={trustBody}
        fromBody={trustValues}
        fields={FIELDS}
        onSubmit={(body, values) => create.mutateAsync({ body, values })}
        submitLabel="Create"
        onCancel={() => navigate('/certs/trustConfigs')}
        jsonHint={
          <>
            Sent to <span className="font-mono">trustConfigs.create</span> in{' '}
            <span className="font-mono">
              projects/{project}/locations/{location}
            </span>{' '}
            with the ID of the form. Allowlisted certificates are set here.
          </>
        }
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id="trust-id" label="Name" error={errors.id?.message} hint="Cannot be changed.">
            <Input
              autoComplete="off"
              spellCheck={false}
              {...fieldAria('trust-id', errors.id?.message, 'hint')}
              {...form.register('id')}
            />
          </Field>
          <Field id="trust-location" label="Location">
            <NativeSelect {...fieldAria('trust-location')} {...form.register('location')}>
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
        <CommonFields form={form} prefix="trust" />
        <PemList form={form} list="anchors" />
        <PemList form={form} list="intermediates" />
      </ResourceEditor>
    </Card>
  )
}

export function EditTrustConfig() {
  const ref = useTrustRef()
  const query = useQuery({ ...trustConfigQuery(ref), enabled: !!ref.project })
  if (!ref.project) return <ChooseProject what="edit its trust configs" />
  if (!query.data) return <QueryStatus query={query} />
  return <EditTrustForm t={query.data} r={ref} />
}

function EditTrustForm({ t: live, r }: { t: TrustConfig; r: TrustRef }) {
  // The trust config as the form read it: a refetch must not move the
  // baseline or the etag, or a change made since would be overwritten.
  const [t] = useState(live)
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  // The body is the trust config's editable fields, so that JSON shows them all.
  const was = t as unknown as Body
  const initialBody: Body = Object.fromEntries(
    EDITABLE.filter((k) => k in was).map((k) => [k, was[k]]),
  )
  const form = useForm<TrustValues>({
    resolver: zodResolver(schema(false)),
    defaultValues: trustValues(initialBody, { ...defaults, location: r.location }),
  })
  const back = () => navigate(trustPath(r))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => {
      const p = trustPatch(t, body)
      if (p.mask.length === 0) return false
      await patchTrustConfig(r, p.body, p.mask)
      return true
    },
    onSuccess: (changed) => {
      void qc.invalidateQueries({ queryKey: ['certs'] })
      toast.success(changed ? `Updated trust config ${r.trust}.` : 'Nothing to change.')
      back()
    },
  })
  return (
    <Card>
      <h1 className="text-lg font-semibold">
        Edit trust config <span className="font-mono">{r.trust}</span>
      </h1>
      <ResourceEditor
        form={form}
        initialBody={initialBody}
        toBody={trustBody}
        fromBody={trustValues}
        fields={FIELDS}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">trustConfigs.patch</span> with an updateMask of the
            fields that changed and etag <span className="font-mono">{t.etag}</span>; a field taken
            out is cleared.
          </>
        }
      >
        <CommonFields form={form} prefix="edit-trust" />
        <PemList form={form} list="anchors" />
        <PemList form={form} list="intermediates" />
      </ResourceEditor>
    </Card>
  )
}
