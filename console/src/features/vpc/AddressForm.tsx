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
import { Textarea } from '@/components/ui/textarea'
import { formatLabels, labelsError, parseLabels } from '@/lib/labels'
import { toast } from '@/lib/toast'
import { useViewState } from '@/lib/viewState'

import { lastSeg, relPath } from '../lb/api'
import { invalid, nameMessage, requiredMessage } from '../lb/spec'
import { insertAddress, networksQuery, subnetsQuery } from './api'
import { isIPv4, NAME, networkRef, networkValue, str, withFieldError, type Obj } from './form'
import { addressPage } from './links'

// The create form for addresses (FR-UI-011), which sends addresses.insert
// in a region or globalAddresses.insert. An address cannot be changed
// once it is reserved, but for its labels (setLabels, on its page).

const PURPOSES = ['VPC_PEERING', 'PRIVATE_SERVICE_CONNECT'] as const

const schema = z
  .object({
    name: z.string(),
    scope: z.enum(['regional', 'global']),
    region: z.string(),
    addressType: z.enum(['EXTERNAL', 'INTERNAL']),
    networkTier: z.string(),
    subnetwork: z.string(),
    purpose: z.enum(PURPOSES),
    network: z.string(),
    prefixLength: z.string(),
    address: z.string(),
    description: z.string(),
    labels: z.string(),
  })
  .superRefine((v, ctx) => {
    const add = (path: string, message: string) =>
      ctx.addIssue({ code: 'custom', path: [path], message })
    if (!NAME.test(v.name)) add('name', nameMessage(v.name))
    if (v.scope === 'regional' && !v.region.trim()) add('region', 'Choose a region.')
    const peering = v.scope === 'global' && v.addressType === 'INTERNAL'
    const ip = v.address.trim()
    // A Private Service Connect address names its IP.
    if ((ip || (peering && v.purpose === 'PRIVATE_SERVICE_CONNECT')) && !isIPv4(ip))
      add('address', invalid('address', ip, 'Must be a valid IPv4 address.'))
    if (peering) {
      if (!v.network.trim()) add('network', requiredMessage('network'))
      if (v.purpose === 'VPC_PEERING') {
        const p = v.prefixLength.trim()
        if (!p) add('prefixLength', requiredMessage('prefixLength'))
        else if (!/^\d+$/.test(p) || Number(p) < 8 || Number(p) > 29)
          add('prefixLength', invalid('prefixLength', p, 'Must be between 8 and 29.'))
      }
    }
    const labels = labelsError(v.labels)
    if (labels) add('labels', labels)
  })
export type Values = z.infer<typeof schema>

const FIELD: Record<string, string> = {
  name: 'name',
  address: 'address',
  subnetwork: 'subnetwork',
  network: 'network',
  purpose: 'purpose',
  prefixLength: 'prefixLength',
  networkTier: 'networkTier',
  description: 'description',
  labels: 'labels',
}

/** subnetRef writes a subnetwork field: a bare name is in project and region. */
const subnetRef = (v: string, project: string, region: string) =>
  NAME.test(v.trim()) ? `projects/${project}/regions/${region}/subnetworks/${v.trim()}` : v.trim()

export function CreateAddress() {
  const [view] = useViewState()
  const [params] = useSearchParams()
  const project = view.project ?? ''
  const id = useId()
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const networks = useQuery(networksQuery(project))
  const subnets = useQuery(subnetsQuery(project))
  const peering = params.get('purpose') === 'VPC_PEERING'
  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: {
      name: '',
      // ?purpose=VPC_PEERING is a private services access range.
      scope: peering ? 'global' : 'regional',
      region: 'us-central1',
      addressType: peering ? 'INTERNAL' : 'EXTERNAL',
      networkTier: '',
      subnetwork: '',
      purpose: 'VPC_PEERING',
      network: params.get('network') ?? '',
      prefixLength: '',
      address: '',
      description: '',
      labels: '',
    },
  })
  const errors = form.formState.errors
  const [scope, type, purpose, region] = useWatch({
    control: form.control,
    name: ['scope', 'addressType', 'purpose', 'region'],
  })
  const global = scope === 'global'
  const internal = type === 'INTERNAL'
  const toBody = (v: Values, base: Body): Body => {
    const out: Body = { ...base, name: v.name, addressType: v.addressType }
    const put = (k: string, x: unknown) => {
      if (x === '' || x === undefined) delete out[k]
      else out[k] = x
    }
    const isGlobal = v.scope === 'global'
    const isInternal = v.addressType === 'INTERNAL'
    const peering = isGlobal && isInternal
    put('address', isInternal ? v.address.trim() : '')
    put(
      'subnetwork',
      !isGlobal && isInternal && v.subnetwork.trim()
        ? subnetRef(v.subnetwork, project, v.region.trim())
        : '',
    )
    put('purpose', peering ? v.purpose : '')
    put('network', peering ? networkRef(v.network, project) : '')
    put(
      'prefixLength',
      peering && v.purpose === 'VPC_PEERING' && v.prefixLength.trim() ? Number(v.prefixLength) : '',
    )
    // Only a regional external address has a network tier.
    put('networkTier', !isGlobal && !isInternal ? v.networkTier : '')
    put('description', v.description)
    const labels = parseLabels(v.labels)
    put('labels', Object.keys(labels).length ? labels : '')
    return out
  }
  const fromBody = (body: Body, prev: Values): Values => {
    const internalBody = body.addressType === 'INTERNAL'
    const purposeBody = str(body.purpose)
    return {
      ...prev,
      name: str(body.name),
      addressType: internalBody ? 'INTERNAL' : 'EXTERNAL',
      networkTier: str(body.networkTier),
      subnetwork: (() => {
        const p = relPath(str(body.subnetwork))
        return p.startsWith(`projects/${project}/regions/${prev.region.trim()}/subnetworks/`)
          ? lastSeg(p)
          : p
      })(),
      purpose: (PURPOSES as readonly string[]).includes(purposeBody)
        ? (purposeBody as Values['purpose'])
        : prev.purpose,
      network: networkValue(body.network, project),
      prefixLength: str(body.prefixLength),
      address: str(body.address),
      description: str(body.description),
      labels: formatLabels(
        typeof body.labels === 'object' && body.labels
          ? (body.labels as Obj as Record<string, string>)
          : {},
      ),
    }
  }
  const create = useMutation({
    meta: { toast: false },
    mutationFn: ({ body, where }: { body: Body; where: string }) =>
      withFieldError(form, FIELD, () => insertAddress(project, where, body)),
    onSuccess: (_, { body, where }) => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(`Reserved address ${str(body.name)}.`)
      navigate(addressPage({ region: where, address: str(body.name) }))
    },
  })
  const regionSubnets = (subnets.data ?? []).filter((s) => lastSeg(s.region) === region.trim())
  return (
    <Card>
      <h1 className="text-lg font-semibold">Reserve address</h1>
      <ResourceEditor
        form={form}
        initialBody={toBody(form.getValues(), {})}
        toBody={toBody}
        fromBody={fromBody}
        fields={[
          'name',
          'addressType',
          'networkTier',
          'subnetwork',
          'purpose',
          'network',
          'prefixLength',
          'address',
          'description',
          'labels',
        ]}
        onSubmit={(body, values) =>
          create.mutateAsync({
            body,
            where: values.scope === 'global' ? '' : values.region.trim(),
          })
        }
        submitLabel="Reserve"
        onCancel={() => navigate('/compute/addresses')}
        jsonHint={
          <>
            Sent to <span className="font-mono">addresses.insert</span> in the region the form
            names, or <span className="font-mono">globalAddresses.insert</span>.
          </>
        }
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field
            id="address-name"
            label="Name"
            error={errors.name?.message}
            hint="Lowercase letters, digits and dashes."
          >
            <Input
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              {...fieldAria('address-name', errors.name?.message, 'hint')}
              {...form.register('name')}
            />
          </Field>
          <Field id="address-type" label="Type">
            <NativeSelect
              {...fieldAria('address-type', undefined)}
              {...form.register('addressType')}
            >
              <option value="EXTERNAL">External</option>
              <option value="INTERNAL">Internal</option>
            </NativeSelect>
          </Field>
          <Field id="address-scope" label="Scope">
            <NativeSelect {...fieldAria('address-scope', undefined)} {...form.register('scope')}>
              <option value="regional">Regional</option>
              <option value="global">Global</option>
            </NativeSelect>
          </Field>
          {!global && (
            <Field id="address-region" label="Region" error={errors.region?.message}>
              <Input
                autoComplete="off"
                spellCheck={false}
                className="font-mono"
                {...fieldAria('address-region', errors.region?.message)}
                {...form.register('region')}
              />
            </Field>
          )}
          {!internal && !global && (
            <Field
              id="address-tier"
              label="Network tier"
              error={errors.networkTier?.message}
              hint="Empty is PREMIUM."
            >
              <NativeSelect
                {...fieldAria('address-tier', errors.networkTier?.message, 'hint')}
                {...form.register('networkTier')}
              >
                <option value="">Default</option>
                <option value="PREMIUM">PREMIUM</option>
                <option value="STANDARD">STANDARD</option>
              </NativeSelect>
            </Field>
          )}
          {internal && !global && (
            <Field
              id="address-subnetwork"
              label="Subnetwork"
              error={errors.subnetwork?.message}
              hint="A subnetwork of the region, or its path. Empty is default."
            >
              <Input
                autoComplete="off"
                spellCheck={false}
                className="font-mono"
                list={`${id}-subnets`}
                {...fieldAria('address-subnetwork', errors.subnetwork?.message, 'hint')}
                {...form.register('subnetwork')}
              />
              <datalist id={`${id}-subnets`}>
                {regionSubnets.map((s) => (
                  <option key={s.name} value={s.name} />
                ))}
              </datalist>
            </Field>
          )}
          {internal && global && (
            <>
              <Field id="address-purpose" label="Purpose" error={errors.purpose?.message}>
                <NativeSelect
                  {...fieldAria('address-purpose', errors.purpose?.message)}
                  {...form.register('purpose')}
                >
                  <option value="VPC_PEERING">VPC_PEERING (private services access)</option>
                  <option value="PRIVATE_SERVICE_CONNECT">PRIVATE_SERVICE_CONNECT</option>
                </NativeSelect>
              </Field>
              <Field
                id="address-network"
                label="Network"
                error={errors.network?.message}
                hint="A VPC network of this Project, or projects/PROJECT/global/networks/NETWORK."
              >
                <Input
                  autoComplete="off"
                  spellCheck={false}
                  className="font-mono"
                  list={`${id}-networks`}
                  {...fieldAria('address-network', errors.network?.message, 'hint')}
                  {...form.register('network')}
                />
                <datalist id={`${id}-networks`}>
                  {(networks.data ?? []).map((n) => (
                    <option key={n.name} value={n.name} />
                  ))}
                </datalist>
              </Field>
              {purpose === 'VPC_PEERING' && (
                <Field
                  id="address-prefix"
                  label="Prefix length"
                  error={errors.prefixLength?.message}
                  hint="8 to 29: the size of the range, e.g. 16 for a /16."
                >
                  <Input
                    inputMode="numeric"
                    className="font-mono"
                    {...fieldAria('address-prefix', errors.prefixLength?.message, 'hint')}
                    {...form.register('prefixLength')}
                  />
                </Field>
              )}
            </>
          )}
          {internal && (
            <Field
              id="address-ip"
              label={global && purpose === 'VPC_PEERING' ? 'First address' : 'IP address'}
              error={errors.address?.message}
              hint={
                global && purpose === 'VPC_PEERING'
                  ? 'The first address of the range. Empty lets the API choose a free one.'
                  : global
                    ? 'The IPv4 address to reserve.'
                    : 'An address in the subnetwork. Empty lets the API choose a free one.'
              }
            >
              <Input
                autoComplete="off"
                spellCheck={false}
                className="font-mono"
                {...fieldAria('address-ip', errors.address?.message, 'hint')}
                {...form.register('address')}
              />
            </Field>
          )}
        </div>
        <QueryStatus query={internal ? (global ? networks : subnets) : networks} />
        <Field id="address-description" label="Description" error={errors.description?.message}>
          <Input
            {...fieldAria('address-description', errors.description?.message)}
            {...form.register('description')}
          />
        </Field>
        <Field
          id="address-labels"
          label="Labels"
          error={errors.labels?.message}
          hint="key=value, one per line."
        >
          <Textarea
            rows={2}
            spellCheck={false}
            className="font-mono"
            {...fieldAria('address-labels', errors.labels?.message, 'hint')}
            {...form.register('labels')}
          />
        </Field>
      </ResourceEditor>
    </Card>
  )
}
