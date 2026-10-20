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
import { toast } from '@/lib/toast'
import { useViewState } from '@/lib/viewState'

import { isObj } from '../lb/api'
import { nameMessage } from '../lb/spec'
import { insertNetwork, networkQuery, patchNetwork, type Network, type NetworkRef } from './api'
import { mtuError, NAME, str, withFieldError, type Obj } from './form'
import { networkPage } from './links'
import { useNetworkRef } from './VpcLayout'

// Create and edit forms for VPC networks (FR-UI-011): create sends
// networks.insert; edit sends networks.patch with the fields that
// changed. Switching to custom mode is its own method, on the network's
// page; peerings are in the JSON editor.

const ROUTING = ['REGIONAL', 'GLOBAL'] as const
type Routing = '' | (typeof ROUTING)[number]

const mtuField = z.string().superRefine((v, ctx) => {
  const message = mtuError(v)
  if (message) ctx.addIssue({ code: 'custom', message })
})

const editSchema = z.object({
  description: z.string(),
  mtu: mtuField,
  routingMode: z.enum(['', ...ROUTING]),
})
export type EditValues = z.infer<typeof editSchema>

const createSchema = editSchema.extend({
  name: z.string().superRefine((v, ctx) => {
    if (!NAME.test(v)) ctx.addIssue({ code: 'custom', message: nameMessage(v) })
  }),
  mode: z.enum(['custom', 'auto']),
})
export type CreateValues = z.infer<typeof createSchema>

const FIELD: Record<string, string> = {
  name: 'name',
  mtu: 'mtu',
  description: 'description',
  routingConfig: 'routingMode',
}

const obj = (v: unknown): Obj => (isObj(v) ? v : {})

/** setRouting writes the routing mode onto body's routingConfig, keeping its other fields. */
function setRouting(out: Body, base: Body, mode: Routing) {
  const rest = Object.fromEntries(
    Object.entries(obj(base.routingConfig)).filter(([k]) => k !== 'routingMode'),
  )
  if (mode) out.routingConfig = { ...rest, routingMode: mode }
  else if (Object.keys(rest).length) out.routingConfig = rest
  else delete out.routingConfig
}

const routingOf = (body: Body): Routing => {
  const m = obj(body.routingConfig).routingMode
  return m === 'REGIONAL' || m === 'GLOBAL' ? m : ''
}

// ---- create ----

export function CreateNetwork() {
  const [view] = useViewState()
  const project = view.project ?? ''
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<CreateValues>({
    resolver: zodResolver(createSchema),
    defaultValues: { name: '', mode: 'custom', description: '', mtu: '', routingMode: '' },
  })
  const errors = form.formState.errors
  const toBody = (v: CreateValues, base: Body): Body => {
    const out: Body = { ...base, name: v.name, autoCreateSubnetworks: v.mode === 'auto' }
    if (v.description) out.description = v.description
    else delete out.description
    if (v.mtu.trim()) out.mtu = Number(v.mtu)
    else delete out.mtu
    setRouting(out, base, v.routingMode)
    return out
  }
  const fromBody = (body: Body): CreateValues => ({
    name: str(body.name),
    mode: body.autoCreateSubnetworks ? 'auto' : 'custom',
    description: str(body.description),
    mtu: str(body.mtu),
    routingMode: routingOf(body),
  })
  const create = useMutation({
    meta: { toast: false },
    mutationFn: (body: Body) => withFieldError(form, FIELD, () => insertNetwork(project, body)),
    onSuccess: (_, body) => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(`Created network ${str(body.name)}.`)
      navigate(networkPage(str(body.name)))
    },
  })
  return (
    <Card>
      <h1 className="text-lg font-semibold">Create network</h1>
      <ResourceEditor
        form={form}
        initialBody={toBody(form.getValues(), {})}
        toBody={toBody}
        fromBody={fromBody}
        fields={['name', 'mode', 'description', 'mtu', 'routingMode']}
        onSubmit={(body) => create.mutateAsync(body)}
        submitLabel="Create"
        onCancel={() => navigate('/compute')}
        jsonHint={
          <>
            Sent to <span className="font-mono">networks.insert</span>.
          </>
        }
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field
            id="network-name"
            label="Name"
            error={errors.name?.message}
            hint="Lowercase letters, digits and dashes. Cannot be changed."
          >
            <Input
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              {...fieldAria('network-name', errors.name?.message, 'hint')}
              {...form.register('name')}
            />
          </Field>
          <Field
            id="network-mode"
            label="Subnet creation mode"
            hint="Auto adds a subnetwork in every region (10.128.0.0/9); custom leaves them to you. An auto mode network can be switched to custom later, not back."
          >
            <NativeSelect
              {...fieldAria('network-mode', undefined, 'hint')}
              {...form.register('mode')}
            >
              <option value="custom">Custom</option>
              <option value="auto">Auto</option>
            </NativeSelect>
          </Field>
        </div>
        <SharedFields form={form} prefix="network" />
      </ResourceEditor>
    </Card>
  )
}

/** SharedFields are the fields create and edit share. */
function SharedFields({
  form,
  prefix,
}: {
  form: UseFormReturn<CreateValues> | UseFormReturn<EditValues>
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
      <div className="grid gap-4 sm:grid-cols-2">
        <Field
          id={`${prefix}-mtu`}
          label="MTU"
          error={errors.mtu?.message}
          hint="Bytes, 1300 to 8896. Empty is 1460."
        >
          <Input
            inputMode="numeric"
            className="font-mono"
            {...fieldAria(`${prefix}-mtu`, errors.mtu?.message, 'hint')}
            {...f.register('mtu')}
          />
        </Field>
        <Field
          id={`${prefix}-routing`}
          label="Dynamic routing mode"
          error={errors.routingMode?.message}
          hint="Which regions' Cloud Routers learn routes: their own (regional) or all (global)."
        >
          <NativeSelect
            {...fieldAria(`${prefix}-routing`, errors.routingMode?.message, 'hint')}
            {...f.register('routingMode')}
          >
            <option value="">Default (REGIONAL)</option>
            {ROUTING.map((m) => (
              <option key={m} value={m}>
                {m}
              </option>
            ))}
          </NativeSelect>
        </Field>
      </div>
    </>
  )
}

// ---- edit ----

export function EditNetwork() {
  const ref = useNetworkRef()
  const query = useQuery(networkQuery(ref))
  if (!query.data) return <QueryStatus query={query} />
  return <EditNetworkForm network={query.data} r={ref} />
}

const editValues = (n: Network): EditValues => ({
  description: n.description ?? '',
  mtu: str(n.mtu),
  routingMode: routingOf(n),
})

function EditNetworkForm({ network, r }: { network: Network; r: NetworkRef }) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<EditValues>({
    resolver: zodResolver(editSchema),
    defaultValues: editValues(network),
  })
  const was = editValues(network)
  const toBody = (v: EditValues, base: Body): Body => {
    const out: Body = { ...base }
    if (v.description !== was.description) out.description = v.description
    else delete out.description
    // A merge patch: null resets the MTU and the routing mode to their defaults.
    if (v.mtu !== was.mtu) out.mtu = v.mtu.trim() ? Number(v.mtu) : null
    else delete out.mtu
    if (v.routingMode !== was.routingMode)
      out.routingConfig = { routingMode: v.routingMode || null }
    else delete out.routingConfig
    return out
  }
  const fromBody = (body: Body): EditValues => ({
    description: 'description' in body ? str(body.description) : was.description,
    mtu: 'mtu' in body ? str(body.mtu) : was.mtu,
    routingMode: 'routingConfig' in body ? routingOf(body) : was.routingMode,
  })
  const back = () => navigate(networkPage(r.network))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => {
      if (Object.keys(body).length === 0) return false
      await withFieldError(form, FIELD, () => patchNetwork(r, body))
      return true
    },
    onSuccess: (changed) => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(changed ? `Updated network ${r.network}.` : 'Nothing to change.')
      back()
    },
  })
  return (
    <Card>
      <h1 className="text-lg font-semibold">
        Edit network <span className="font-mono">{r.network}</span>
      </h1>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={toBody}
        fromBody={fromBody}
        fields={['description', 'mtu', 'routingMode']}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">networks.patch</span>: objects merge, lists replace.
          </>
        }
      >
        <SharedFields form={form} prefix="edit-network" />
      </ResourceEditor>
    </Card>
  )
}
