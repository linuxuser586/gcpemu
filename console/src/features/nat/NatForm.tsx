import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Plus } from 'lucide-react'
import { useParams } from 'react-router'
import { useForm, useWatch, type UseFormReturn } from 'react-hook-form'
import { z } from 'zod'

import { errorMessage } from '@/api/errors'
import { useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Field, fieldAria } from '@/components/resource/Field'
import { ResourceEditor, type Body } from '@/components/resource/ResourceEditor'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import { toast } from '@/lib/toast'

import { lastSeg, relPath } from '../lb/api'
import {
  addressesQuery,
  patchRouter,
  regionSubnets,
  resourceField,
  routerPath,
  routerQuery,
  subnetsQuery,
  withNat,
  type LogFilter,
  type RangesToNat,
  type Router,
  type RouterNat,
  type RouterNatSubnetwork,
  type RouterRef,
} from './api'
import { natPage, routerPage, useRouterRef } from './NatLayout'

// The NAT gateway forms (FR-NAT-001, FR-UI-011). A NAT is an entry of its
// router's nats[]: adding or editing one sends routers.patch with the
// whole list, the router's other NATs unchanged. The JSON editor holds the
// NAT config itself; timeouts, drain IPs and rules are edited there.
// Messages are the API's, for the NAT's index in nats[].

type Obj = Record<string, unknown>
const str = (v: unknown) => (typeof v === 'string' ? v : typeof v === 'number' ? String(v) : '')
const lines = (s: string) =>
  s
    .split(/[\n,]/)
    .map((l) => l.trim())
    .filter(Boolean)

const NAME = /^[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?$/

/** A subnetwork's choice: its ranges, or "custom" for a secondary range list kept as is. */
export type SubnetChoice = 'ALL_IP_RANGES' | 'PRIMARY_IP_RANGE' | 'custom'

export const RANGES: Record<RangesToNat, string> = {
  ALL_SUBNETWORKS_ALL_IP_RANGES: 'All subnetworks, all ranges',
  ALL_SUBNETWORKS_ALL_PRIMARY_IP_RANGES: 'All subnetworks, primary ranges only',
  LIST_OF_SUBNETWORKS: 'Chosen subnetworks',
}

const values = z.object({
  name: z.string(),
  alloc: z.enum(['AUTO_ONLY', 'MANUAL_ONLY']),
  natIps: z.string(),
  ranges: z.enum([
    'ALL_SUBNETWORKS_ALL_IP_RANGES',
    'ALL_SUBNETWORKS_ALL_PRIMARY_IP_RANGES',
    'LIST_OF_SUBNETWORKS',
  ]),
  subnets: z.array(
    z.object({ name: z.string(), ranges: z.enum(['ALL_IP_RANGES', 'PRIMARY_IP_RANGE', 'custom']) }),
  ),
  logEnable: z.boolean(),
  logFilter: z.enum(['ALL', 'ERRORS_ONLY', 'TRANSLATIONS_ONLY']),
  minPorts: z.string(),
  dynamic: z.boolean(),
  maxPorts: z.string(),
  eim: z.boolean(),
})
export type NatValues = z.infer<typeof values>

const msg = (i: number, field: string, value: unknown, why: string) =>
  `Invalid value for field 'resource.nats[${i}].${field}': '${str(value)}'. ${why}`

/**
 * natSchema checks what routers.patch checks of the NAT at index i, with
 * its messages; taken are the router's other NATs' names.
 */
export const natSchema = (i: number, taken: readonly string[]) =>
  values.superRefine((v, ctx) => {
    const add = (path: keyof NatValues, message: string) =>
      ctx.addIssue({ code: 'custom', path: [path], message })
    const why = "Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'"
    if (!NAME.test(v.name)) add('name', msg(i, 'name', v.name, why))
    else if (taken.includes(v.name))
      add('name', msg(i, 'name', v.name, 'NAT names must be unique within a router.'))
    if (v.alloc === 'MANUAL_ONLY' && lines(v.natIps).length === 0)
      add(
        'natIps',
        msg(i, 'natIps', '', 'At least one NAT IP is required with MANUAL_ONLY allocation.'),
      )
    if (v.ranges === 'LIST_OF_SUBNETWORKS' && v.subnets.length === 0)
      add(
        'subnets',
        msg(i, 'subnetworks', '', 'At least one subnetwork is required with LIST_OF_SUBNETWORKS.'),
      )
    const min = v.minPorts.trim()
    if (min) {
      const m = Number(min)
      if (!/^\d+$/.test(min) || m < 2 || m > 65536)
        add('minPorts', msg(i, 'minPortsPerVm', min, 'Must be between 2 and 65536.'))
      else if (v.dynamic && (m & (m - 1)) !== 0)
        add(
          'minPorts',
          msg(
            i,
            'minPortsPerVm',
            min,
            'Must be a power of two when dynamic port allocation is enabled.',
          ),
        )
    }
    const max = v.maxPorts.trim()
    if (max && v.dynamic && !/^\d+$/.test(max))
      add('maxPorts', msg(i, 'maxPortsPerVm', max, 'Must be a number.'))
  })

/** natValues reads the form's values out of a NAT config. */
export function natValues(n: Obj): NatValues {
  const log = (n.logConfig ?? {}) as { enable?: boolean; filter?: LogFilter }
  const subs = (Array.isArray(n.subnetworks) ? n.subnetworks : []) as RouterNatSubnetwork[]
  return {
    name: str(n.name),
    alloc: n.natIpAllocateOption === 'MANUAL_ONLY' ? 'MANUAL_ONLY' : 'AUTO_ONLY',
    natIps: (Array.isArray(n.natIps) ? (n.natIps as string[]) : []).map(addressValue).join('\n'),
    ranges: (n.sourceSubnetworkIpRangesToNat as RangesToNat) ?? 'ALL_SUBNETWORKS_ALL_IP_RANGES',
    subnets: subs.map((s) => ({ name: lastSeg(s.name), ranges: choiceOf(s) })),
    logEnable: !!log.enable,
    logFilter: log.filter ?? 'ALL',
    minPorts: str(n.minPortsPerVm),
    dynamic: !!n.enableDynamicPortAllocation,
    maxPorts: str(n.maxPortsPerVm),
    eim: !!n.enableEndpointIndependentMapping,
  }
}

/** addressValue shows a NAT IP's address by name when it is in the router's region. */
const addressValue = (link: string) => {
  const m = /regions\/[^/]+\/addresses\/([^/]+)$/.exec(relPath(link))
  return m ? m[1]! : link
}

function choiceOf(s: RouterNatSubnetwork): SubnetChoice {
  const k = s.sourceIpRangesToNat ?? []
  if (k.length === 1 && k[0] === 'ALL_IP_RANGES') return 'ALL_IP_RANGES'
  if (k.length === 1 && k[0] === 'PRIMARY_IP_RANGE') return 'PRIMARY_IP_RANGE'
  return 'custom'
}

/** natBody writes the form's values onto a NAT config, keeping what it does not edit. */
export function natBody(v: NatValues, base: Body): Body {
  const out: Body = {
    ...base,
    name: v.name,
    natIpAllocateOption: v.alloc,
    sourceSubnetworkIpRangesToNat: v.ranges,
  }
  const was = natValues(base)
  if (v.alloc === 'MANUAL_ONLY') {
    // Unchanged NAT IPs keep the API's links.
    out.natIps = v.natIps === was.natIps ? base.natIps : lines(v.natIps)
  } else delete out.natIps
  if (v.ranges === 'LIST_OF_SUBNETWORKS') {
    const old = (Array.isArray(base.subnetworks) ? base.subnetworks : []) as RouterNatSubnetwork[]
    out.subnetworks = v.subnets.map((s) => {
      const kept = old.find((o) => lastSeg(o.name) === s.name)
      if (kept && (s.ranges === 'custom' || choiceOf(kept) === s.ranges)) return kept
      return { name: s.name, sourceIpRangesToNat: [s.ranges] }
    })
  } else delete out.subnetworks
  if (v.logEnable) out.logConfig = { enable: true, filter: v.logFilter }
  else if (base.logConfig) out.logConfig = { ...(base.logConfig as Obj), enable: false }
  else delete out.logConfig
  if (v.minPorts.trim()) out.minPortsPerVm = Number(v.minPorts)
  else delete out.minPortsPerVm
  if (v.dynamic) out.enableDynamicPortAllocation = true
  else if ('enableDynamicPortAllocation' in base) out.enableDynamicPortAllocation = false
  if (v.dynamic && v.maxPorts.trim()) out.maxPortsPerVm = Number(v.maxPorts)
  else delete out.maxPortsPerVm
  if (v.eim) out.enableEndpointIndependentMapping = true
  else if ('enableEndpointIndependentMapping' in base) out.enableEndpointIndependentMapping = false
  return out
}

/** API fields of a NAT, by the form field that edits them. */
const FIELD: Record<string, keyof NatValues> = {
  name: 'name',
  natIpAllocateOption: 'alloc',
  natIps: 'natIps',
  sourceSubnetworkIpRangesToNat: 'ranges',
  subnetworks: 'subnets',
  logConfig: 'logFilter',
  minPortsPerVm: 'minPorts',
  maxPortsPerVm: 'maxPorts',
}

/** withFieldError shows an API error about NAT i on the field it names. */
async function withFieldError(
  form: UseFormReturn<NatValues>,
  i: number,
  send: () => Promise<unknown>,
) {
  try {
    return await send()
  } catch (e) {
    const m = /^nats\[(\d+)\]\.(\w+)/.exec(resourceField(errorMessage(e)) ?? '')
    const f = m && Number(m[1]) === i ? FIELD[m[2]!] : undefined
    if (f) form.setError(f, { type: 'server', message: errorMessage(e) })
    throw e
  }
}

const NEW_NAT: Body = {
  name: '',
  natIpAllocateOption: 'AUTO_ONLY',
  sourceSubnetworkIpRangesToNat: 'ALL_SUBNETWORKS_ALL_IP_RANGES',
}

export function AddNat() {
  const ref = useRouterRef()
  const query = useQuery(routerQuery(ref))
  if (!query.data) return <QueryStatus query={query} />
  return <NatForm r={ref} router={query.data} />
}

export function EditNat() {
  const ref = useRouterRef()
  const { nat: name = '' } = useParams()
  const query = useQuery(routerQuery(ref))
  if (!query.data) return <QueryStatus query={query} />
  const nat = query.data.nats?.find((n) => n.name === name)
  if (!nat) {
    return (
      <Card role="status">
        <p className="text-sm text-muted-foreground">
          Router <span className="font-mono">{ref.router}</span> has no NAT gateway named{' '}
          <span className="font-mono">{name}</span>.
        </p>
      </Card>
    )
  }
  return <NatForm r={ref} router={query.data} nat={nat} />
}

function NatForm({ r, router, nat }: { r: RouterRef; router: Router; nat?: RouterNat }) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const nats = router.nats ?? []
  const index = nat ? nats.findIndex((n) => n.name === nat.name) : nats.length
  const taken = nats.filter((n) => n.name !== nat?.name).map((n) => n.name)
  const base: Body = nat ?? NEW_NAT
  const form = useForm<NatValues>({
    resolver: zodResolver(natSchema(index, taken)),
    defaultValues: natValues(base),
  })
  const back = () => navigate(nat ? natPage(r, nat.name) : routerPage(r))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: (body: Body) =>
      withFieldError(form, index, () =>
        patchRouter(r, {
          nats: withNat(router, body as RouterNat, nat?.name),
          fingerprint: router.fingerprint,
        }),
      ),
    onSuccess: (_, body) => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(
        nat ? `Updated NAT gateway ${str(body.name)}.` : `Added NAT gateway ${str(body.name)}.`,
      )
      navigate(natPage(r, str(body.name)))
    },
  })
  return (
    <Card>
      <h1 className="text-lg font-semibold">
        {nat ? (
          <>
            Edit NAT gateway <span className="font-mono">{nat.name}</span>
          </>
        ) : (
          'Add NAT gateway'
        )}{' '}
        <span className="text-sm font-normal text-muted-foreground">
          router {r.router}, {r.region}, network {lastSeg(router.network)}
        </span>
      </h1>
      <ResourceEditor
        form={form}
        initialBody={natBody(form.getValues(), base)}
        toBody={natBody}
        fromBody={(body) => natValues(body)}
        fields={Object.values(FIELD)}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel={nat ? 'Save' : 'Add'}
        onCancel={back}
        jsonHint={
          <>
            The NAT config, sent to <span className="font-mono">routers.patch</span> of{' '}
            <span className="font-mono">{routerPath(r)}</span> as{' '}
            <span className="font-mono">nats[{index}]</span>; the router&apos;s other NATs are kept.
          </>
        }
      >
        <NatFields form={form} r={r} router={router} editing={!!nat} />
      </ResourceEditor>
    </Card>
  )
}

function NatFields({
  form,
  r,
  router,
  editing,
}: {
  form: UseFormReturn<NatValues>
  r: RouterRef
  router: Router
  editing: boolean
}) {
  const errors = form.formState.errors
  const [alloc, ranges, logEnable, dynamic] = useWatch({
    control: form.control,
    name: ['alloc', 'ranges', 'logEnable', 'dynamic'],
  })
  return (
    <>
      <Field
        id="nat-name"
        label="Name"
        error={errors.name?.message}
        hint={editing ? 'Cannot be changed here.' : 'Lowercase letters, digits and dashes.'}
      >
        <Input
          autoComplete="off"
          spellCheck={false}
          className="font-mono"
          readOnly={editing}
          {...fieldAria('nat-name', errors.name?.message, 'hint')}
          {...form.register('name')}
        />
      </Field>
      <Field
        id="nat-ranges"
        label="Source ranges"
        error={errors.ranges?.message}
        hint={`Which ranges of ${lastSeg(router.network)}'s subnetworks in ${r.region} reach the internet through this NAT.`}
      >
        <NativeSelect
          {...fieldAria('nat-ranges', errors.ranges?.message, 'hint')}
          {...form.register('ranges')}
        >
          {Object.entries(RANGES).map(([k, label]) => (
            <option key={k} value={k}>
              {label} ({k})
            </option>
          ))}
        </NativeSelect>
      </Field>
      {ranges === 'LIST_OF_SUBNETWORKS' && <SubnetsField form={form} router={router} r={r} />}
      <Field
        id="nat-alloc"
        label="NAT IP allocation"
        error={errors.alloc?.message}
        hint="Automatic allocates NAT IPs; manual uses reserved external addresses."
      >
        <NativeSelect
          {...fieldAria('nat-alloc', errors.alloc?.message, 'hint')}
          {...form.register('alloc')}
        >
          <option value="AUTO_ONLY">Automatic (AUTO_ONLY)</option>
          <option value="MANUAL_ONLY">Manual (MANUAL_ONLY)</option>
        </NativeSelect>
      </Field>
      {alloc === 'MANUAL_ONLY' && <NatIpsField form={form} r={r} />}
      <div className="grid gap-4 sm:grid-cols-2">
        <Field
          id="nat-min-ports"
          label="Minimum ports per VM"
          error={errors.minPorts?.message}
          hint={`Empty for the default (${dynamic ? 32 : 64}).`}
        >
          <Input
            inputMode="numeric"
            className="font-mono"
            {...fieldAria('nat-min-ports', errors.minPorts?.message, 'hint')}
            {...form.register('minPorts')}
          />
        </Field>
        {dynamic && (
          <Field
            id="nat-max-ports"
            label="Maximum ports per VM"
            error={errors.maxPorts?.message}
            hint="Empty for the default."
          >
            <Input
              inputMode="numeric"
              className="font-mono"
              {...fieldAria('nat-max-ports', errors.maxPorts?.message, 'hint')}
              {...form.register('maxPorts')}
            />
          </Field>
        )}
      </div>
      <Check form={form} name="dynamic" label="Dynamic port allocation" />
      <Check form={form} name="eim" label="Endpoint-independent mapping" />
      <Check form={form} name="logEnable" label="Logging" />
      {logEnable && (
        <Field
          id="nat-log-filter"
          label="Log"
          error={errors.logFilter?.message}
          hint="Logged in Cloud Logging JSON shape to the Instance's log (FR-NAT-004)."
        >
          <NativeSelect
            {...fieldAria('nat-log-filter', errors.logFilter?.message, 'hint')}
            {...form.register('logFilter')}
          >
            <option value="ALL">Translations and errors (ALL)</option>
            <option value="TRANSLATIONS_ONLY">Translations only (TRANSLATIONS_ONLY)</option>
            <option value="ERRORS_ONLY">Errors only (ERRORS_ONLY)</option>
          </NativeSelect>
        </Field>
      )}
    </>
  )
}

function Check({
  form,
  name,
  label,
}: {
  form: UseFormReturn<NatValues>
  name: 'dynamic' | 'eim' | 'logEnable'
  label: string
}) {
  return (
    <label className="flex items-center gap-2 text-sm font-medium">
      <input type="checkbox" className="size-4" {...form.register(name)} />
      {label}
    </label>
  )
}

const CHOICES: Record<SubnetChoice | '', string> = {
  '': 'Not translated',
  ALL_IP_RANGES: 'All ranges',
  PRIMARY_IP_RANGE: 'Primary range only',
  custom: 'Secondary ranges (JSON)',
}

/** SubnetsField chooses the ranges of each subnetwork of the router's region and network. */
function SubnetsField({
  form,
  router,
  r,
}: {
  form: UseFormReturn<NatValues>
  router: Router
  r: RouterRef
}) {
  const subnets = useQuery(subnetsQuery(r.project))
  const chosen = useWatch({ control: form.control, name: 'subnets' }) ?? []
  const error = form.formState.errors.subnets?.message
  const names = [
    ...new Set([
      ...regionSubnets(router, subnets.data ?? []).map((s) => s.name),
      ...chosen.map((s) => s.name),
    ]),
  ]
  const set = (name: string, v: SubnetChoice | '') => {
    const next = !v
      ? chosen.filter((s) => s.name !== name)
      : chosen.some((s) => s.name === name)
        ? chosen.map((s) => (s.name === name ? { name, ranges: v } : s))
        : [...chosen, { name, ranges: v }]
    form.setValue('subnets', next, {
      shouldDirty: true,
      shouldValidate: form.formState.isSubmitted,
    })
  }
  return (
    <fieldset
      className="flex flex-col gap-2"
      aria-describedby={error ? 'nat-subnets-error' : 'nat-subnets-hint'}
    >
      <legend className="text-sm font-medium">Subnetworks</legend>
      <QueryStatus query={subnets} />
      {subnets.data && names.length === 0 && (
        <p className="text-sm text-muted-foreground">
          {lastSeg(router.network)} has no subnetworks in {r.region}.
        </p>
      )}
      {names.map((name) => {
        const cur = chosen.find((s) => s.name === name)?.ranges ?? ''
        return (
          <div key={name} className="flex flex-wrap items-center gap-3">
            <span className="min-w-40 font-mono text-sm">{name}</span>
            <NativeSelect
              aria-label={`Ranges of ${name}`}
              value={cur}
              onChange={(e) => set(name, e.target.value as SubnetChoice | '')}
            >
              {Object.entries(CHOICES)
                .filter(([k]) => k !== 'custom' || cur === 'custom')
                .map(([k, label]) => (
                  <option key={k} value={k}>
                    {label}
                  </option>
                ))}
            </NativeSelect>
          </div>
        )
      })}
      <p id="nat-subnets-hint" className="text-xs text-muted-foreground">
        Lists of secondary ranges are edited in the JSON editor.
      </p>
      {error && (
        <p id="nat-subnets-error" role="alert" className="text-sm text-destructive">
          {error}
        </p>
      )}
    </fieldset>
  )
}

/** NatIpsField lists the reserved addresses of a manual NAT, offering the region's free ones. */
function NatIpsField({ form, r }: { form: UseFormReturn<NatValues>; r: RouterRef }) {
  const error = form.formState.errors.natIps?.message
  const text = useWatch({ control: form.control, name: 'natIps' }) ?? ''
  const addresses = useQuery(addressesQuery(r.project, r.region))
  const used = new Set(lines(text))
  const self = routerPath(r)
  const free = (addresses.data ?? []).filter(
    (a) => !used.has(a.name) && (a.users ?? []).every((u) => relPath(u) === self),
  )
  const hint = `External addresses reserved in ${r.region}: a name, or projects/PROJECT/regions/REGION/addresses/NAME, one per line.`
  return (
    <Field id="nat-ips" label="NAT IPs" error={error} hint={hint}>
      <Textarea
        rows={2}
        spellCheck={false}
        className="font-mono"
        {...fieldAria('nat-ips', error, hint)}
        {...form.register('natIps')}
      />
      {free.length > 0 && (
        <div className="flex flex-wrap gap-2">
          {free.map((a) => (
            <Button
              key={a.name}
              type="button"
              variant="outline"
              size="sm"
              aria-label={`Use address ${a.name}`}
              onClick={() =>
                form.setValue('natIps', [...lines(text), a.name].join('\n'), { shouldDirty: true })
              }
            >
              <Plus aria-hidden />
              {a.name} <span className="text-muted-foreground">{a.address}</span>
            </Button>
          ))}
        </div>
      )}
    </Field>
  )
}
