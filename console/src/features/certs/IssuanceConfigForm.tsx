import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm, useWatch } from 'react-hook-form'
import { z } from 'zod'

import { useCarriedNavigate } from '@/components/Link'
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
  createIssuanceConfig,
  locationsQuery,
  type IssuanceRef,
} from './api'
import { CommonFields, fieldsSchema } from './CertificateForm'
import { ChooseProject, issuancePath } from './CertsLayout'

// The create form for certificate issuance configs (FR-LB-004,
// FR-UI-011), checked as the Service checks it, with its messages; the
// lifetime is entered in days, so the form also checks it is a number.
// The CA pool, lifetime, rotation window and key algorithm cannot be
// changed once created, so there is no edit form.

type Obj = Record<string, unknown>
const obj = (v: unknown): Obj =>
  typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Obj) : {}
const str = (v: unknown) => (typeof v === 'string' ? v : '')

const DAY = 86400

const schema = z
  .object({
    id: z.string(),
    location: z.string(),
    caPool: z.string(),
    lifetimeDays: z.string(),
    rotation: z.string(),
    keyAlgorithm: z.string(),
    ...fieldsSchema,
  })
  .superRefine((v, ctx) => {
    const issue = (path: string, message: string) =>
      ctx.addIssue({ code: 'custom', path: [path], message })
    if (!CERT_ID.test(v.id)) issue('id', certIdMessage(v.id, 'certificateIssuanceConfigId'))
    if (!v.caPool.trim())
      issue(
        'caPool',
        'Field certificate_authority_config.certificate_authority_service_config.ca_pool is required.',
      )
    const days = Number(v.lifetimeDays)
    if (!v.lifetimeDays.trim()) issue('lifetimeDays', 'Field lifetime is required.')
    else if (!(days > 0)) issue('lifetimeDays', 'The lifetime must be a positive number of days.')
    const pct = Number(v.rotation)
    if (!Number.isInteger(pct) || pct <= 0 || pct >= 100 || !v.rotation.trim())
      issue('rotation', 'Field rotation_window_percentage must be between 1 and 99.')
  })
export type IssuanceValues = z.infer<typeof schema>

/** issuanceBody writes the form's values onto a certificateIssuanceConfigs.create body. */
export function issuanceBody(v: IssuanceValues, base: Body): Body {
  const out: Body = { ...base }
  const set = (k: string, value: unknown, keep: boolean) => {
    if (keep) out[k] = value
    else delete out[k]
  }
  set('description', v.description, !!v.description)
  const labels = parseLabels(v.labels)
  set('labels', labels, Object.keys(labels).length > 0)
  out.certificateAuthorityConfig = {
    ...obj(base.certificateAuthorityConfig),
    certificateAuthorityServiceConfig: { caPool: v.caPool.trim() },
  }
  const days = Number(v.lifetimeDays)
  set('lifetime', `${Math.round(days * DAY)}s`, days > 0)
  const pct = Number(v.rotation)
  set('rotationWindowPercentage', pct, !!v.rotation.trim() && Number.isFinite(pct))
  set('keyAlgorithm', v.keyAlgorithm, !!v.keyAlgorithm)
  return out
}

/** issuanceValues reads the form's values out of a body. */
export function issuanceValues(body: Body, prev: IssuanceValues): IssuanceValues {
  const secs = Number(str(body.lifetime).replace(/s$/, ''))
  return {
    ...prev,
    description: str(body.description),
    labels: formatLabels(obj(body.labels) as Record<string, string>),
    caPool: str(obj(obj(body.certificateAuthorityConfig).certificateAuthorityServiceConfig).caPool),
    lifetimeDays: secs > 0 ? String(secs / DAY) : '',
    rotation:
      typeof body.rotationWindowPercentage === 'number'
        ? String(body.rotationWindowPercentage)
        : '',
    keyAlgorithm: str(body.keyAlgorithm),
  }
}

export function CreateIssuanceConfig() {
  const [view] = useViewState()
  const project = view.project ?? ''
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<IssuanceValues>({
    resolver: zodResolver(schema),
    defaultValues: {
      id: '',
      location: 'global',
      caPool: '',
      lifetimeDays: '30',
      rotation: '66',
      keyAlgorithm: '',
      description: '',
      labels: '',
    },
  })
  const errors = form.formState.errors
  const location = useWatch({ control: form.control, name: 'location' })
  const locations = useQuery({ ...locationsQuery(project), enabled: !!project })
  const create = useMutation({
    meta: { toast: false },
    mutationFn: async ({ body, values }: { body: Body; values: IssuanceValues }) => {
      const r: IssuanceRef = { project, location: values.location, config: values.id }
      await createIssuanceConfig(r, body)
      return r
    },
    onSuccess: (r) => {
      void qc.invalidateQueries({ queryKey: ['certs'] })
      toast.success(`Created certificate issuance config ${r.config}.`)
      navigate(issuancePath(r))
    },
  })
  if (!project) return <ChooseProject what="create a certificate issuance config in it" />
  return (
    <Card>
      <h1 className="text-lg font-semibold">Create certificate issuance config</h1>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={issuanceBody}
        fromBody={issuanceValues}
        fields={['id', 'caPool', 'lifetimeDays', 'rotation', 'keyAlgorithm', 'labels']}
        onSubmit={(body, values) => create.mutateAsync({ body, values })}
        submitLabel="Create"
        onCancel={() => navigate('/certs/issuanceConfigs')}
        jsonHint={
          <>
            Sent to <span className="font-mono">certificateIssuanceConfigs.create</span> in{' '}
            <span className="font-mono">
              projects/{project}/locations/{location}
            </span>{' '}
            with the ID of the form.
          </>
        }
      >
        <p className="text-sm text-muted-foreground">
          None of these settings can be changed once the config is created.
        </p>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id="issuance-id" label="Name" error={errors.id?.message}>
            <Input
              autoComplete="off"
              spellCheck={false}
              {...fieldAria('issuance-id', errors.id?.message)}
              {...form.register('id')}
            />
          </Field>
          <Field id="issuance-location" label="Location">
            <NativeSelect {...fieldAria('issuance-location')} {...form.register('location')}>
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
          id="issuance-pool"
          label="CA pool"
          error={errors.caPool?.message}
          hint="The CA Service CA pool that issues the certificates, as projects/PROJECT/locations/LOCATION/caPools/POOL."
        >
          <Input
            autoComplete="off"
            spellCheck={false}
            className="font-mono"
            {...fieldAria('issuance-pool', errors.caPool?.message, 'hint')}
            {...form.register('caPool')}
          />
        </Field>
        <div className="grid gap-4 sm:grid-cols-3">
          <Field
            id="issuance-lifetime"
            label="Lifetime (days)"
            error={errors.lifetimeDays?.message}
            hint="How long each certificate is valid."
          >
            <Input
              inputMode="decimal"
              {...fieldAria('issuance-lifetime', errors.lifetimeDays?.message, 'hint')}
              {...form.register('lifetimeDays')}
            />
          </Field>
          <Field
            id="issuance-rotation"
            label="Rotation window (%)"
            error={errors.rotation?.message}
            hint="Renewed when this much of its lifetime is left."
          >
            <Input
              inputMode="numeric"
              {...fieldAria('issuance-rotation', errors.rotation?.message, 'hint')}
              {...form.register('rotation')}
            />
          </Field>
          <Field id="issuance-key" label="Key algorithm">
            <NativeSelect {...fieldAria('issuance-key')} {...form.register('keyAlgorithm')}>
              <option value="">Unspecified</option>
              <option value="RSA_2048">RSA_2048</option>
              <option value="ECDSA_P256">ECDSA_P256</option>
            </NativeSelect>
          </Field>
        </div>
        <CommonFields form={form} prefix="issuance" />
      </ResourceEditor>
    </Card>
  )
}
