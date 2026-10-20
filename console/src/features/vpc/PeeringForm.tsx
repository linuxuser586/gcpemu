import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useId } from 'react'
import { useForm, type UseFormReturn } from 'react-hook-form'
import { useParams } from 'react-router'
import { z } from 'zod'

import { useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Field, fieldAria } from '@/components/resource/Field'
import { ResourceEditor, type Body } from '@/components/resource/ResourceEditor'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import { toast } from '@/lib/toast'

import {
  addPeering,
  networkQuery,
  networksQuery,
  updatePeering,
  type NetworkPeering,
  type NetworkRef,
} from './api'
import { NAME, networkRef, networkValue, str, withFieldError } from './form'
import { networkPage } from './links'
import { useNetworkRef } from './VpcLayout'

// Add and edit forms for a network's peerings (FR-UI-011): add sends
// networks.addPeering; edit sends networks.updatePeering, which replaces
// the route exchange settings, so it sends them all. A peering's name and
// peer network cannot change.

const editSchema = z.object({
  importCustomRoutes: z.boolean(),
  exportCustomRoutes: z.boolean(),
  importSubnetRoutesWithPublicIp: z.boolean(),
  exportSubnetRoutesWithPublicIp: z.boolean(),
  stackType: z.enum(['IPV4_ONLY', 'IPV4_IPV6']),
})
export type EditValues = z.infer<typeof editSchema>

const addSchema = editSchema
  .extend({ name: z.string(), network: z.string() })
  .superRefine((v, ctx) => {
    if (!NAME.test(v.name))
      ctx.addIssue({
        code: 'custom',
        path: ['name'],
        message: `Invalid value for field 'networkPeering.name': '${v.name}'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'`,
      })
    if (!v.network.trim())
      ctx.addIssue({
        code: 'custom',
        path: ['network'],
        message: "Required field 'networkPeering.network' not specified",
      })
  })
export type AddValues = z.infer<typeof addSchema>

const BOOLS = [
  'importCustomRoutes',
  'exportCustomRoutes',
  'importSubnetRoutesWithPublicIp',
  'exportSubnetRoutesWithPublicIp',
] as const

/** peeringField is the form field a networkPeering error names. */
const peeringField = (message: string) => {
  const f = /field '?networkPeering\.([^':.[]+)/.exec(message)?.[1]
  return f && (f === 'name' || f === 'network' || f === 'stackType') ? f : undefined
}

/** peeringOf is the networkPeering of a request body. */
const peeringOf = (body: Body): Body =>
  typeof body.networkPeering === 'object' && body.networkPeering
    ? (body.networkPeering as Body)
    : {}

/** putSettings writes the route exchange settings onto a networkPeering. */
function putSettings(p: Body, v: EditValues) {
  for (const k of BOOLS) p[k] = v[k]
  p.stackType = v.stackType
}

const BLANK_SETTINGS: EditValues = {
  importCustomRoutes: false,
  exportCustomRoutes: false,
  importSubnetRoutesWithPublicIp: false,
  exportSubnetRoutesWithPublicIp: false,
  stackType: 'IPV4_ONLY',
}

/** readSettings reads the route exchange settings out of a networkPeering. */
const readSettings = (p: Body, prev: EditValues): EditValues => ({
  importCustomRoutes: p.importCustomRoutes === true,
  exportCustomRoutes: p.exportCustomRoutes === true,
  importSubnetRoutesWithPublicIp: p.importSubnetRoutesWithPublicIp === true,
  exportSubnetRoutesWithPublicIp: p.exportSubnetRoutesWithPublicIp === true,
  stackType: p.stackType === 'IPV4_IPV6' ? 'IPV4_IPV6' : prev.stackType,
})

const LABELS: Record<(typeof BOOLS)[number], string> = {
  importCustomRoutes: 'Import custom routes',
  exportCustomRoutes: 'Export custom routes',
  importSubnetRoutesWithPublicIp: 'Import subnet routes with public IPs',
  exportSubnetRoutesWithPublicIp: 'Export subnet routes with public IPs',
}

/** SettingsFields are the route exchange settings add and edit share. */
function SettingsFields({
  form,
  prefix,
}: {
  form: UseFormReturn<AddValues> | UseFormReturn<EditValues>
  prefix: string
}) {
  const f = form as UseFormReturn<EditValues>
  return (
    <>
      <fieldset className="flex flex-col gap-2">
        <legend className="text-sm font-medium">Route exchange</legend>
        {BOOLS.map((k) => (
          <label key={k} className="flex items-center gap-2 text-sm">
            <input type="checkbox" className="size-4" {...f.register(k)} />
            {LABELS[k]}
          </label>
        ))}
        <p className="text-xs text-muted-foreground">
          Recorded: the emulator stores these but exchanges no routes.
        </p>
      </fieldset>
      <Field id={`${prefix}-stack`} label="Stack type">
        <NativeSelect {...fieldAria(`${prefix}-stack`, undefined)} {...f.register('stackType')}>
          <option value="IPV4_ONLY">IPV4_ONLY</option>
          <option value="IPV4_IPV6">IPV4_IPV6</option>
        </NativeSelect>
      </Field>
    </>
  )
}

// ---- add ----

export function AddPeering() {
  const r = useNetworkRef()
  const id = useId()
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const networks = useQuery(networksQuery(r.project))
  const form = useForm<AddValues>({
    resolver: zodResolver(addSchema),
    defaultValues: {
      name: '',
      network: '',
      importCustomRoutes: false,
      exportCustomRoutes: false,
      importSubnetRoutesWithPublicIp: false,
      exportSubnetRoutesWithPublicIp: true,
      stackType: 'IPV4_ONLY',
    },
  })
  const errors = form.formState.errors
  const toBody = (v: AddValues, base: Body): Body => {
    const p: Body = {
      ...peeringOf(base),
      name: v.name,
      network: networkRef(v.network, r.project),
      exchangeSubnetRoutes: true,
    }
    putSettings(p, v)
    return { ...base, networkPeering: p }
  }
  const fromBody = (body: Body, prev: AddValues): AddValues => {
    const p = peeringOf(body)
    return {
      ...readSettings(p, prev),
      name: str(p.name),
      network: networkValue(p.network, r.project),
    }
  }
  const back = () => navigate(networkPage(r.network))
  const add = useMutation({
    meta: { toast: false },
    mutationFn: (body: Body) =>
      withFieldError(
        form,
        {},
        () => addPeering(r, peeringOf(body) as NetworkPeering),
        peeringField,
      ),
    onSuccess: (_, body) => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(`Added peering ${str(peeringOf(body).name)}.`)
      back()
    },
  })
  return (
    <Card>
      <h1 className="text-lg font-semibold">
        Add peering to <span className="font-mono">{r.network}</span>
      </h1>
      <ResourceEditor
        form={form}
        initialBody={toBody(form.getValues(), {})}
        toBody={toBody}
        fromBody={fromBody}
        fields={['name', 'network', 'stackType']}
        onSubmit={(body) => add.mutateAsync(body)}
        submitLabel="Add"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">networks.addPeering</span>. The peering is active
            once the peer network has a peering back to this one.
          </>
        }
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field
            id="peering-name"
            label="Name"
            error={errors.name?.message}
            hint="Lowercase letters, digits and dashes. Cannot be changed."
          >
            <Input
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              {...fieldAria('peering-name', errors.name?.message, 'hint')}
              {...form.register('name')}
            />
          </Field>
          <Field
            id="peering-network"
            label="Peer network"
            error={errors.network?.message}
            hint="A VPC network of this Project, or projects/PROJECT/global/networks/NETWORK. Cannot be changed."
          >
            <Input
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              list={`${id}-networks`}
              {...fieldAria('peering-network', errors.network?.message, 'hint')}
              {...form.register('network')}
            />
            <datalist id={`${id}-networks`}>
              {(networks.data ?? [])
                .filter((n) => n.name !== r.network)
                .map((n) => (
                  <option key={n.name} value={n.name} />
                ))}
            </datalist>
          </Field>
        </div>
        <QueryStatus query={networks} />
        <SettingsFields form={form} prefix="peering" />
      </ResourceEditor>
    </Card>
  )
}

// ---- edit ----

export function EditPeering() {
  const r = useNetworkRef()
  const { peering = '' } = useParams()
  const query = useQuery(networkQuery(r))
  const p = query.data?.peerings?.find((x) => x.name === peering)
  if (!query.data) return <QueryStatus query={query} />
  if (!p) {
    return (
      <Card role="status">
        <p className="text-sm text-muted-foreground">
          Network <span className="font-mono">{r.network}</span> has no peering{' '}
          <span className="font-mono">{peering}</span>.
        </p>
      </Card>
    )
  }
  return <EditPeeringForm r={r} peering={p} />
}

function EditPeeringForm({ r, peering }: { r: NetworkRef; peering: NetworkPeering }) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<EditValues>({
    resolver: zodResolver(editSchema),
    defaultValues: readSettings(peering, { ...BLANK_SETTINGS }),
  })
  const toBody = (v: EditValues, base: Body): Body => {
    const p: Body = { ...peeringOf(base), name: peering.name }
    putSettings(p, v)
    return { ...base, networkPeering: p }
  }
  const fromBody = (body: Body, prev: EditValues) => readSettings(peeringOf(body), prev)
  const back = () => navigate(networkPage(r.network))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: (body: Body) =>
      withFieldError(
        form,
        {},
        () => updatePeering(r, peeringOf(body) as NetworkPeering),
        peeringField,
      ),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(`Updated peering ${peering.name}.`)
      back()
    },
  })
  return (
    <Card>
      <h1 className="text-lg font-semibold">
        Edit peering <span className="font-mono">{peering.name}</span>{' '}
        <span className="text-sm font-normal text-muted-foreground">
          of {r.network} to {networkValue(peering.network, r.project)}
        </span>
      </h1>
      <ResourceEditor
        form={form}
        initialBody={toBody(form.getValues(), {})}
        toBody={toBody}
        fromBody={fromBody}
        fields={['stackType']}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">networks.updatePeering</span>, which replaces the
            route exchange settings: leaving one out turns it off.
          </>
        }
      >
        <SettingsFields form={form} prefix="edit-peering" />
      </ResourceEditor>
    </Card>
  )
}
