import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useId } from 'react'
import { useForm, useWatch, type UseFormReturn } from 'react-hook-form'
import { useParams, useSearchParams } from 'react-router'
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

import { isObj, lastSeg } from '../lb/api'
import { nameMessage, requiredMessage } from '../lb/spec'
import {
  firewallQuery,
  insertFirewall,
  networksQuery,
  patchFirewall,
  type Firewall,
  type FirewallRule,
} from './api'
import {
  formatList,
  formatRules,
  NAME,
  networkRef,
  networkValue,
  parseList,
  parseRules,
  priorityError,
  rulesError,
  str,
  withFieldError,
} from './form'
import { firewallPage } from './links'
import { RecordedNotice } from './RecordedNotice'

// Create and edit forms for firewall rules (FR-UI-011): create sends
// firewalls.insert; edit sends firewalls.patch with the fields that
// changed. A rule's name, network and direction cannot be changed.

type Direction = 'INGRESS' | 'EGRESS'

const editShape = {
  action: z.enum(['ALLOW', 'DENY']),
  rules: z.string(),
  priority: z.string(),
  sourceRanges: z.string(),
  destinationRanges: z.string(),
  sourceTags: z.string(),
  sourceServiceAccounts: z.string(),
  targetTags: z.string(),
  targetServiceAccounts: z.string(),
  description: z.string(),
  disabled: z.boolean(),
  logs: z.boolean(),
}

/** check adds the API's messages for the rules and the priority. */
function check(
  v: { action: 'ALLOW' | 'DENY'; rules: string; priority: string },
  ctx: z.RefinementCtx,
) {
  const rules = rulesError(v.rules, v.action === 'DENY' ? 'denied' : 'allowed')
  if (rules) ctx.addIssue({ code: 'custom', path: ['rules'], message: rules })
  const priority = priorityError(v.priority)
  if (priority) ctx.addIssue({ code: 'custom', path: ['priority'], message: priority })
}

const editSchema = z.object(editShape).superRefine(check)
export type EditValues = z.infer<typeof editSchema>

const createSchema = z
  .object({
    ...editShape,
    name: z.string(),
    network: z.string(),
    direction: z.enum(['INGRESS', 'EGRESS']),
  })
  .superRefine((v, ctx) => {
    if (!NAME.test(v.name))
      ctx.addIssue({ code: 'custom', path: ['name'], message: nameMessage(v.name) })
    if (!v.network.trim()) {
      ctx.addIssue({ code: 'custom', path: ['network'], message: requiredMessage('network') })
    }
    check(v, ctx)
  })
export type CreateValues = z.infer<typeof createSchema>

const FIELD: Record<string, string> = {
  name: 'name',
  network: 'network',
  direction: 'direction',
  priority: 'priority',
  allowed: 'rules',
  denied: 'rules',
  sourceRanges: 'sourceRanges',
  destinationRanges: 'destinationRanges',
  sourceTags: 'sourceTags',
  sourceServiceAccounts: 'sourceServiceAccounts',
  targetTags: 'targetTags',
  targetServiceAccounts: 'targetServiceAccounts',
  description: 'description',
}

const LISTS = [
  'sourceRanges',
  'destinationRanges',
  'sourceTags',
  'sourceServiceAccounts',
  'targetTags',
  'targetServiceAccounts',
] as const

const listOf = (v: unknown) => (Array.isArray(v) ? v.map(str) : [])
const rulesOf = (v: unknown) => (Array.isArray(v) ? (v as FirewallRule[]) : [])

/** shared reads the fields create and edit share out of a rule or a request body. */
function shared(body: Body): EditValues {
  const deny = rulesOf(body.denied).length > 0
  return {
    action: deny ? 'DENY' : 'ALLOW',
    rules: formatRules(rulesOf(deny ? body.denied : body.allowed)),
    priority: str(body.priority),
    sourceRanges: formatList(listOf(body.sourceRanges)),
    destinationRanges: formatList(listOf(body.destinationRanges)),
    sourceTags: formatList(listOf(body.sourceTags)),
    sourceServiceAccounts: formatList(listOf(body.sourceServiceAccounts)),
    targetTags: formatList(listOf(body.targetTags)),
    targetServiceAccounts: formatList(listOf(body.targetServiceAccounts)),
    description: str(body.description),
    disabled: body.disabled === true,
    logs: isObj(body.logConfig) && body.logConfig.enable === true,
  }
}

/** shown are the list fields a rule of direction d has. */
const SOURCES: readonly string[] = ['sourceRanges', 'sourceTags', 'sourceServiceAccounts']
const shown = (d: Direction) =>
  LISTS.filter((k) => (d === 'EGRESS' ? !SOURCES.includes(k) : k !== 'destinationRanges'))

// ---- create ----

export function CreateFirewall() {
  const [view] = useViewState()
  const [params] = useSearchParams()
  const project = view.project ?? ''
  const id = useId()
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const networks = useQuery(networksQuery(project))
  const form = useForm<CreateValues>({
    resolver: zodResolver(createSchema),
    defaultValues: {
      name: '',
      network: params.get('network') ?? '',
      direction: 'INGRESS',
      ...shared({}),
    },
  })
  const errors = form.formState.errors
  const direction = useWatch({ control: form.control, name: 'direction' })
  const toBody = (v: CreateValues, base: Body): Body => {
    const out: Body = { ...base, name: v.name, network: networkRef(v.network, project) }
    out.direction = v.direction
    const [set, unset] = v.action === 'DENY' ? ['denied', 'allowed'] : ['allowed', 'denied']
    out[set] = parseRules(v.rules)
    delete out[unset]
    if (v.priority.trim()) out.priority = Number(v.priority)
    else delete out.priority
    for (const k of LISTS) {
      const l = parseList(v[k])
      if (l.length && shown(v.direction).includes(k)) out[k] = l
      else delete out[k]
    }
    if (v.description) out.description = v.description
    else delete out.description
    if (v.disabled) out.disabled = true
    else delete out.disabled
    if (v.logs) out.logConfig = { ...(isObj(base.logConfig) ? base.logConfig : {}), enable: true }
    else delete out.logConfig
    return out
  }
  const fromBody = (body: Body): CreateValues => ({
    ...shared(body),
    name: str(body.name),
    network: networkValue(body.network, project),
    direction: body.direction === 'EGRESS' ? 'EGRESS' : 'INGRESS',
  })
  const create = useMutation({
    meta: { toast: false },
    mutationFn: (body: Body) => withFieldError(form, FIELD, () => insertFirewall(project, body)),
    onSuccess: (_, body) => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(`Created firewall rule ${str(body.name)}.`)
      navigate(firewallPage(str(body.name)))
    },
  })
  return (
    <Card>
      <h1 className="text-lg font-semibold">Create firewall rule</h1>
      <RecordedNotice />
      <ResourceEditor
        form={form}
        initialBody={toBody(form.getValues(), {})}
        toBody={toBody}
        fromBody={fromBody}
        fields={[
          'name',
          'network',
          'direction',
          ...(Object.values(FIELD) as (keyof CreateValues)[]),
        ]}
        onSubmit={(body) => create.mutateAsync(body)}
        submitLabel="Create"
        onCancel={() => navigate('/compute/firewalls')}
        jsonHint={
          <>
            Sent to <span className="font-mono">firewalls.insert</span>.
          </>
        }
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field
            id="firewall-name"
            label="Name"
            error={errors.name?.message}
            hint="Lowercase letters, digits and dashes. Cannot be changed."
          >
            <Input
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              {...fieldAria('firewall-name', errors.name?.message, 'hint')}
              {...form.register('name')}
            />
          </Field>
          <Field
            id="firewall-network"
            label="Network"
            error={errors.network?.message}
            hint="A VPC network of this Project, or projects/PROJECT/global/networks/NETWORK. Cannot be changed."
          >
            <Input
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              list={`${id}-networks`}
              {...fieldAria('firewall-network', errors.network?.message, 'hint')}
              {...form.register('network')}
            />
            <datalist id={`${id}-networks`}>
              {(networks.data ?? []).map((n) => (
                <option key={n.name} value={n.name} />
              ))}
            </datalist>
          </Field>
          <Field
            id="firewall-direction"
            label="Direction"
            hint="Ingress matches traffic to the targets, egress traffic from them. Cannot be changed."
          >
            <NativeSelect
              {...fieldAria('firewall-direction', undefined, 'hint')}
              {...form.register('direction')}
            >
              <option value="INGRESS">INGRESS</option>
              <option value="EGRESS">EGRESS</option>
            </NativeSelect>
          </Field>
        </div>
        <QueryStatus query={networks} />
        <SharedFields form={form} prefix="firewall" direction={direction} />
      </ResourceEditor>
    </Card>
  )
}

const LIST_FIELDS: Record<(typeof LISTS)[number], [label: string, hint: string]> = {
  sourceRanges: [
    'Source ranges',
    'IPv4 CIDR ranges, separated by commas. Empty with no source tags is 0.0.0.0/0.',
  ],
  destinationRanges: [
    'Destination ranges',
    'IPv4 CIDR ranges, separated by commas. Empty is 0.0.0.0/0.',
  ],
  sourceTags: ['Source tags', 'Network tags of the VM instances the traffic comes from.'],
  sourceServiceAccounts: [
    'Source service accounts',
    'Service account emails of the VM instances the traffic comes from, instead of tags.',
  ],
  targetTags: [
    'Target tags',
    'Network tags of the VM instances the rule applies to. Empty is all.',
  ],
  targetServiceAccounts: [
    'Target service accounts',
    'Service account emails of the VM instances the rule applies to, instead of tags.',
  ],
}

/** SharedFields are the fields create and edit share. */
function SharedFields({
  form,
  prefix,
  direction,
}: {
  form: UseFormReturn<CreateValues> | UseFormReturn<EditValues>
  prefix: string
  direction: Direction
}) {
  const f = form as UseFormReturn<EditValues>
  const errors = f.formState.errors
  return (
    <>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field
          id={`${prefix}-action`}
          label="Action"
          hint="Whether matching traffic is allowed or denied."
        >
          <NativeSelect
            {...fieldAria(`${prefix}-action`, undefined, 'hint')}
            {...f.register('action')}
          >
            <option value="ALLOW">ALLOW</option>
            <option value="DENY">DENY</option>
          </NativeSelect>
        </Field>
        <Field
          id={`${prefix}-rules`}
          label="Protocols and ports"
          error={errors.rules?.message}
          hint="As gcloud's --rules: tcp:22,tcp:8000-8080,udp,icmp, or all."
        >
          <Input
            autoComplete="off"
            spellCheck={false}
            className="font-mono"
            {...fieldAria(`${prefix}-rules`, errors.rules?.message, 'hint')}
            {...f.register('rules')}
          />
        </Field>
        <Field
          id={`${prefix}-priority`}
          label="Priority"
          error={errors.priority?.message}
          hint="0 to 65535; the lowest number wins. Empty is 1000."
        >
          <Input
            inputMode="numeric"
            className="font-mono"
            {...fieldAria(`${prefix}-priority`, errors.priority?.message, 'hint')}
            {...f.register('priority')}
          />
        </Field>
        {shown(direction).map((k) => (
          <Field
            key={k}
            id={`${prefix}-${k}`}
            label={LIST_FIELDS[k][0]}
            error={errors[k]?.message}
            hint={LIST_FIELDS[k][1]}
          >
            <Input
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              {...fieldAria(`${prefix}-${k}`, errors[k]?.message, 'hint')}
              {...f.register(k)}
            />
          </Field>
        ))}
      </div>
      <Field id={`${prefix}-description`} label="Description" error={errors.description?.message}>
        <Input
          {...fieldAria(`${prefix}-description`, errors.description?.message)}
          {...f.register('description')}
        />
      </Field>
      <div className="flex flex-wrap gap-6">
        <label className="flex items-center gap-2 text-sm font-medium">
          <input type="checkbox" className="size-4" {...f.register('disabled')} />
          Disabled
        </label>
        <label className="flex items-center gap-2 text-sm font-medium">
          <input type="checkbox" className="size-4" {...f.register('logs')} />
          Logs
        </label>
      </div>
    </>
  )
}

// ---- edit ----

export function EditFirewall() {
  const { firewall: name = '' } = useParams()
  const [view] = useViewState()
  const project = view.project ?? ''
  const query = useQuery(firewallQuery(project, name))
  if (!query.data) return <QueryStatus query={query} />
  return <EditFirewallForm firewall={query.data} project={project} />
}

function EditFirewallForm({ firewall, project }: { firewall: Firewall; project: string }) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const name = firewall.name
  const direction: Direction = firewall.direction === 'EGRESS' ? 'EGRESS' : 'INGRESS'
  const was = shared(firewall)
  const form = useForm<EditValues>({ resolver: zodResolver(editSchema), defaultValues: was })
  const toBody = (v: EditValues, base: Body): Body => {
    const out: Body = { ...base }
    delete out.allowed
    delete out.denied
    // A patch with allowed clears denied, and the other way round.
    if (v.action !== was.action || v.rules !== was.rules) {
      out[v.action === 'DENY' ? 'denied' : 'allowed'] = parseRules(v.rules)
    }
    // A merge patch: null resets the priority to 1000.
    if (v.priority !== was.priority) out.priority = v.priority.trim() ? Number(v.priority) : null
    else delete out.priority
    // Lists replace: an empty one clears them.
    for (const k of shown(direction)) {
      if (v[k] !== was[k]) out[k] = parseList(v[k])
      else delete out[k]
    }
    if (v.description !== was.description) out.description = v.description
    else delete out.description
    if (v.disabled !== was.disabled) out.disabled = v.disabled
    else delete out.disabled
    if (v.logs !== was.logs) out.logConfig = { enable: v.logs }
    else delete out.logConfig
    return out
  }
  const fromBody = (body: Body): EditValues => {
    const read = shared(body)
    const rules = 'allowed' in body || 'denied' in body
    const keep = <K extends keyof EditValues>(k: K, field: string = k) =>
      field in body ? read[k] : was[k]
    return {
      action: rules ? read.action : was.action,
      rules: rules ? read.rules : was.rules,
      priority: keep('priority'),
      sourceRanges: keep('sourceRanges'),
      destinationRanges: keep('destinationRanges'),
      sourceTags: keep('sourceTags'),
      sourceServiceAccounts: keep('sourceServiceAccounts'),
      targetTags: keep('targetTags'),
      targetServiceAccounts: keep('targetServiceAccounts'),
      description: keep('description'),
      disabled: keep('disabled'),
      logs: keep('logs', 'logConfig'),
    }
  }
  const back = () => navigate(firewallPage(name))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => {
      if (Object.keys(body).length === 0) return false
      await withFieldError(form, FIELD, () => patchFirewall(project, name, body))
      return true
    },
    onSuccess: (changed) => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(changed ? `Updated firewall rule ${name}.` : 'Nothing to change.')
      back()
    },
  })
  return (
    <Card>
      <h1 className="text-lg font-semibold">
        Edit firewall rule <span className="font-mono">{name}</span>{' '}
        <span className="text-sm font-normal text-muted-foreground">
          {direction}, network {lastSeg(firewall.network)}
        </span>
      </h1>
      <RecordedNotice />
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={toBody}
        fromBody={fromBody}
        fields={Object.keys(editShape) as (keyof EditValues)[]}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">firewalls.patch</span>: objects merge, lists
            replace, and <span className="font-mono">allowed</span> replaces{' '}
            <span className="font-mono">denied</span> (and the other way round).
          </>
        }
      >
        <SharedFields form={form} prefix="edit-firewall" direction={direction} />
      </ResourceEditor>
    </Card>
  )
}
