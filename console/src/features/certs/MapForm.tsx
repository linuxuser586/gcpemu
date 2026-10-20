import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm } from 'react-hook-form'
import { z } from 'zod'

import { useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Field, fieldAria } from '@/components/resource/Field'
import { ResourceEditor, type Body } from '@/components/resource/ResourceEditor'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { formatLabels, parseLabels } from '@/lib/labels'
import { toast } from '@/lib/toast'
import { useViewState } from '@/lib/viewState'

import {
  CERT_ID,
  certIdMessage,
  createMap,
  mapQuery,
  patchMap,
  type CertificateMap,
  type MapRef,
} from './api'
import {
  CommonFields,
  editSchema,
  editValues,
  fieldsSchema,
  patchBody,
  patchValues,
  updateMask,
  type EditValues,
} from './CertificateForm'
import { ChooseProject, mapPath, useMapRef } from './CertsLayout'

// Create and edit forms for certificate maps (FR-LB-004, FR-UI-011).
// Maps are always global. Create sends certificateMaps.create; edit sends
// certificateMaps.patch with the labels and description that changed.

type Obj = Record<string, unknown>
const obj = (v: unknown): Obj =>
  typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Obj) : {}
const str = (v: unknown) => (typeof v === 'string' ? v : '')

const createSchema = z.object({ id: z.string(), ...fieldsSchema }).superRefine((v, ctx) => {
  if (!CERT_ID.test(v.id)) {
    ctx.addIssue({ code: 'custom', path: ['id'], message: certIdMessage(v.id, 'certificateMapId') })
  }
})
export type MapValues = z.infer<typeof createSchema>

/** mapBody writes the create form's values onto a certificateMaps.create body. */
export function mapBody(v: MapValues, base: Body): Body {
  const out: Body = { ...base }
  if (v.description) out.description = v.description
  else delete out.description
  const labels = parseLabels(v.labels)
  if (Object.keys(labels).length > 0) out.labels = labels
  else delete out.labels
  return out
}

/** mapValues reads the create form's values out of a CertificateMap body. */
export const mapValues = (body: Body, prev: MapValues): MapValues => ({
  ...prev,
  description: str(body.description),
  labels: formatLabels(obj(body.labels) as Record<string, string>),
})

export function CreateMap() {
  const [view] = useViewState()
  const project = view.project ?? ''
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<MapValues>({
    resolver: zodResolver(createSchema),
    defaultValues: { id: '', description: '', labels: '' },
  })
  const errors = form.formState.errors
  const create = useMutation({
    meta: { toast: false },
    mutationFn: async ({ body, values }: { body: Body; values: MapValues }) => {
      const r: MapRef = { project, map: values.id }
      await createMap(r, body)
      return r
    },
    onSuccess: (r) => {
      void qc.invalidateQueries({ queryKey: ['certs'] })
      toast.success(`Created certificate map ${r.map}.`)
      navigate(mapPath(r))
    },
  })
  if (!project) return <ChooseProject what="create a certificate map in it" />
  return (
    <Card>
      <h1 className="text-lg font-semibold">Create certificate map</h1>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={mapBody}
        fromBody={mapValues}
        fields={['id', 'labels']}
        onSubmit={(body, values) => create.mutateAsync({ body, values })}
        submitLabel="Create"
        onCancel={() => navigate('/certs/maps')}
        jsonHint={
          <>
            Sent to <span className="font-mono">certificateMaps.create</span> in{' '}
            <span className="font-mono">projects/{project}/locations/global</span> with the ID of
            the form. Entries are added on the map&apos;s page.
          </>
        }
      >
        <Field id="map-id" label="Name" error={errors.id?.message} hint="Cannot be changed.">
          <Input
            autoComplete="off"
            spellCheck={false}
            {...fieldAria('map-id', errors.id?.message, 'hint')}
            {...form.register('id')}
          />
        </Field>
        <CommonFields form={form} prefix="map" />
      </ResourceEditor>
    </Card>
  )
}

export function EditMap() {
  const ref = useMapRef()
  const query = useQuery({ ...mapQuery(ref), enabled: !!ref.project })
  if (!ref.project) return <ChooseProject what="edit its certificate maps" />
  if (!query.data) return <QueryStatus query={query} />
  return <EditMapForm m={query.data} r={ref} />
}

function EditMapForm({ m, r }: { m: CertificateMap; r: MapRef }) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<EditValues>({
    resolver: zodResolver(editSchema),
    defaultValues: editValues(m),
  })
  const back = () => navigate(mapPath(r))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => {
      const mask = updateMask(body)
      if (mask.length === 0) return false
      await patchMap(r, body, mask)
      return true
    },
    onSuccess: (changed) => {
      void qc.invalidateQueries({ queryKey: ['certs'] })
      toast.success(changed ? `Updated certificate map ${r.map}.` : 'Nothing to change.')
      back()
    },
  })
  return (
    <Card>
      <h1 className="text-lg font-semibold">
        Edit certificate map <span className="font-mono">{r.map}</span>
      </h1>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={(v, base) => patchBody(m, v, base)}
        fromBody={patchValues(m)}
        fields={['labels']}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">certificateMaps.patch</span> with an updateMask of
            the body&apos;s fields.
          </>
        }
      >
        <CommonFields form={form} prefix="edit-map" />
      </ResourceEditor>
    </Card>
  )
}
