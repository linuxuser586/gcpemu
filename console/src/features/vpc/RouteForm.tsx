import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useId } from 'react'
import { useForm, useWatch } from 'react-hook-form'
import { useSearchParams } from 'react-router'
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

import { invalid, nameMessage, requiredMessage } from '../lb/spec'
import { insertRoute, networksQuery } from './api'
import {
  formatList,
  NAME,
  networkRef,
  networkValue,
  parseCidr,
  parseList,
  priorityError,
  str,
  withFieldError,
} from './form'
import { routePage } from './links'
import { RecordedNotice } from './RecordedNotice'

// The create form for routes (FR-UI-011), which sends routes.insert.
// Routes are immutable: there is no edit form.

const HOPS = {
  nextHopGateway: ['Gateway', 'Next hop gateway', 'default-internet-gateway, the only gateway.'],
  nextHopIp: ['IP address', 'Next hop IP address', 'A VM instance’s internal IPv4 address.'],
  nextHopInstance: [
    'VM instance',
    'Next hop VM instance',
    'projects/PROJECT/zones/ZONE/instances/INSTANCE.',
  ],
  nextHopIlb: [
    'Internal load balancer',
    'Next hop internal load balancer',
    'An internal passthrough forwarding rule’s path, or its IP address.',
  ],
  nextHopVpnTunnel: [
    'VPN tunnel',
    'Next hop VPN tunnel',
    'projects/PROJECT/regions/REGION/vpnTunnels/TUNNEL.',
  ],
} as const
type Hop = keyof typeof HOPS
const HOP_KEYS = Object.keys(HOPS) as Hop[]

const GATEWAY = 'default-internet-gateway'

const schema = z
  .object({
    name: z.string(),
    network: z.string(),
    destRange: z.string(),
    priority: z.string(),
    hopKind: z.enum(HOP_KEYS as [Hop, ...Hop[]]),
    hop: z.string(),
    tags: z.string(),
    description: z.string(),
  })
  .superRefine((v, ctx) => {
    const add = (path: string, message: string) =>
      ctx.addIssue({ code: 'custom', path: [path], message })
    if (!NAME.test(v.name)) add('name', nameMessage(v.name))
    if (!v.network.trim()) add('network', requiredMessage('network'))
    if (!v.destRange.trim()) add('destRange', requiredMessage('destRange'))
    else if (!parseCidr(v.destRange))
      add(
        'destRange',
        invalid('destRange', v.destRange.trim(), 'Must be a valid IPv4 CIDR address range.'),
      )
    const priority = priorityError(v.priority)
    if (priority) add('priority', priority)
    if (!v.hop.trim()) add('hop', requiredMessage(v.hopKind))
  })
export type Values = z.infer<typeof schema>

const FIELD: Record<string, string> = {
  name: 'name',
  network: 'network',
  destRange: 'destRange',
  priority: 'priority',
  tags: 'tags',
  description: 'description',
  ...Object.fromEntries(HOP_KEYS.map((k) => [k, 'hop'])),
}

/** hopRef writes a next hop: a bare gateway name is in project. */
const hopRef = (kind: Hop, v: string, project: string) =>
  kind === 'nextHopGateway' && NAME.test(v.trim())
    ? `projects/${project}/global/gateways/${v.trim()}`
    : v.trim()

export function CreateRoute() {
  const [view] = useViewState()
  const [params] = useSearchParams()
  const project = view.project ?? ''
  const id = useId()
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const networks = useQuery(networksQuery(project))
  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: {
      name: '',
      network: params.get('network') ?? '',
      destRange: '',
      priority: '',
      hopKind: 'nextHopGateway',
      hop: GATEWAY,
      tags: '',
      description: '',
    },
  })
  const errors = form.formState.errors
  const kind = useWatch({ control: form.control, name: 'hopKind' })
  const toBody = (v: Values, base: Body): Body => {
    const out: Body = {
      ...base,
      name: v.name,
      network: networkRef(v.network, project),
      destRange: v.destRange.trim(),
    }
    if (v.priority.trim()) out.priority = Number(v.priority)
    else delete out.priority
    for (const k of HOP_KEYS) delete out[k]
    if (v.hop.trim()) out[v.hopKind] = hopRef(v.hopKind, v.hop, project)
    const tags = parseList(v.tags)
    if (tags.length) out.tags = tags
    else delete out.tags
    if (v.description) out.description = v.description
    else delete out.description
    return out
  }
  const fromBody = (body: Body, prev: Values): Values => {
    const hopKind = HOP_KEYS.find((k) => str(body[k])) ?? prev.hopKind
    const hop = str(body[hopKind])
    return {
      name: str(body.name),
      network: networkValue(body.network, project),
      destRange: str(body.destRange),
      priority: str(body.priority),
      hopKind,
      hop:
        hopKind === 'nextHopGateway' && hop.endsWith(`/global/gateways/${GATEWAY}`) ? GATEWAY : hop,
      tags: formatList(Array.isArray(body.tags) ? body.tags.map(str) : []),
      description: str(body.description),
    }
  }
  const create = useMutation({
    meta: { toast: false },
    mutationFn: (body: Body) => withFieldError(form, FIELD, () => insertRoute(project, body)),
    onSuccess: (_, body) => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(`Created route ${str(body.name)}.`)
      navigate(routePage(str(body.name)))
    },
  })
  const hopField = form.register('hopKind')
  return (
    <Card>
      <h1 className="text-lg font-semibold">Create route</h1>
      <RecordedNotice />
      <ResourceEditor
        form={form}
        initialBody={toBody(form.getValues(), {})}
        toBody={toBody}
        fromBody={fromBody}
        fields={['name', 'network', 'destRange', 'priority', 'hop', 'tags', 'description']}
        onSubmit={(body) => create.mutateAsync(body)}
        submitLabel="Create"
        onCancel={() => navigate('/compute/routes')}
        jsonHint={
          <>
            Sent to <span className="font-mono">routes.insert</span>. A route cannot be changed once
            it is made.
          </>
        }
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field
            id="route-name"
            label="Name"
            error={errors.name?.message}
            hint="Lowercase letters, digits and dashes."
          >
            <Input
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              {...fieldAria('route-name', errors.name?.message, 'hint')}
              {...form.register('name')}
            />
          </Field>
          <Field
            id="route-network"
            label="Network"
            error={errors.network?.message}
            hint="A VPC network of this Project, or projects/PROJECT/global/networks/NETWORK."
          >
            <Input
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              list={`${id}-networks`}
              {...fieldAria('route-network', errors.network?.message, 'hint')}
              {...form.register('network')}
            />
            <datalist id={`${id}-networks`}>
              {(networks.data ?? []).map((n) => (
                <option key={n.name} value={n.name} />
              ))}
            </datalist>
          </Field>
          <Field
            id="route-dest"
            label="Destination range"
            error={errors.destRange?.message}
            hint="The IPv4 CIDR range the route sends, e.g. 10.50.0.0/16 or 0.0.0.0/0."
          >
            <Input
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              {...fieldAria('route-dest', errors.destRange?.message, 'hint')}
              {...form.register('destRange')}
            />
          </Field>
          <Field
            id="route-priority"
            label="Priority"
            error={errors.priority?.message}
            hint="0 to 65535; among routes as specific, the lowest number wins. Empty is 1000."
          >
            <Input
              inputMode="numeric"
              className="font-mono"
              {...fieldAria('route-priority', errors.priority?.message, 'hint')}
              {...form.register('priority')}
            />
          </Field>
          <Field id="route-hop-kind" label="Next hop">
            <NativeSelect
              {...fieldAria('route-hop-kind', undefined)}
              {...hopField}
              onChange={(e) => {
                void hopField.onChange(e)
                form.setValue('hop', e.target.value === 'nextHopGateway' ? GATEWAY : '')
              }}
            >
              {HOP_KEYS.map((k) => (
                <option key={k} value={k}>
                  {HOPS[k][0]}
                </option>
              ))}
            </NativeSelect>
          </Field>
          <Field
            id="route-hop"
            label={HOPS[kind][1]}
            error={errors.hop?.message}
            hint={HOPS[kind][2]}
          >
            <Input
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              {...fieldAria('route-hop', errors.hop?.message, 'hint')}
              {...form.register('hop')}
            />
          </Field>
        </div>
        <QueryStatus query={networks} />
        <Field
          id="route-tags"
          label="Network tags"
          error={errors.tags?.message}
          hint="Network tags of the VM instances the route applies to, separated by commas. Empty is all."
        >
          <Input
            autoComplete="off"
            spellCheck={false}
            className="font-mono"
            {...fieldAria('route-tags', errors.tags?.message, 'hint')}
            {...form.register('tags')}
          />
        </Field>
        <Field id="route-description" label="Description" error={errors.description?.message}>
          <Input
            {...fieldAria('route-description', errors.description?.message)}
            {...form.register('description')}
          />
        </Field>
      </ResourceEditor>
    </Card>
  )
}
