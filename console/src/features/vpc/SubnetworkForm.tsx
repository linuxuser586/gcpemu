import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useId } from 'react'
import { useForm, type UseFormReturn } from 'react-hook-form'
import { useSearchParams } from 'react-router'
import { z } from 'zod'

import { useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Field, fieldAria } from '@/components/resource/Field'
import { ResourceEditor, type Body } from '@/components/resource/ResourceEditor'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Textarea } from '@/components/ui/textarea'
import { toast } from '@/lib/toast'
import { useViewState } from '@/lib/viewState'

import { lastSeg, relPath } from '../lb/api'
import { nameMessage, requiredMessage } from '../lb/spec'
import {
  insertSubnetwork,
  networksQuery,
  patchSubnetwork,
  subnetQuery,
  type SecondaryRange,
  type Subnetwork,
  type SubnetRef,
} from './api'
import {
  formatRanges,
  NAME,
  parseRanges,
  primaryRangeError,
  rangesError,
  str,
  withFieldError,
} from './form'
import { subnetPage, useSubnetRef } from './VpcLayout'

// Create and edit forms for subnetworks (FR-UI-011): create sends
// subnetworks.insert; edit sends subnetworks.patch with the fields that
// changed and the fingerprint the API requires. Expanding the primary
// range and Private Google Access are their own methods, on the
// subnetwork's page.

const required = (message: string) =>
  z.string().superRefine((v, ctx) => {
    if (!v.trim()) ctx.addIssue({ code: 'custom', message })
  })

const rangesField = z.string().superRefine((v, ctx) => {
  const message = rangesError(v)
  if (message) ctx.addIssue({ code: 'custom', message })
})

const editSchema = z.object({ description: z.string(), secondary: rangesField })
export type EditValues = z.infer<typeof editSchema>

const createSchema = editSchema.extend({
  name: z.string().superRefine((v, ctx) => {
    if (!NAME.test(v)) ctx.addIssue({ code: 'custom', message: nameMessage(v) })
  }),
  region: required('Choose a region.'),
  network: required(requiredMessage('network')),
  ipCidrRange: z.string().superRefine((v, ctx) => {
    const message = primaryRangeError(v)
    if (message) ctx.addIssue({ code: 'custom', message })
  }),
  privateIpGoogleAccess: z.boolean(),
})
export type CreateValues = z.infer<typeof createSchema>

const FIELD: Record<string, string> = {
  name: 'name',
  network: 'network',
  ipCidrRange: 'ipCidrRange',
  description: 'description',
  secondaryIpRanges: 'secondary',
}

/** networkRef writes the network field: a bare name is in project. */
export const networkRef = (v: string, project: string) =>
  NAME.test(v.trim()) ? `projects/${project}/global/networks/${v.trim()}` : v.trim()

/** networkValue reads the network field: a name for project's networks. */
export function networkValue(link: unknown, project: string) {
  const p = relPath(str(link))
  return p.startsWith(`projects/${project}/global/networks/`) ? lastSeg(p) : p
}

const rangesOf = (body: Body) =>
  Array.isArray(body.secondaryIpRanges) ? (body.secondaryIpRanges as SecondaryRange[]) : []

/** setRanges writes the secondary ranges text onto body. */
function setRanges(out: Body, text: string) {
  const ranges = parseRanges(text)
  if (ranges.length) out.secondaryIpRanges = ranges
  else delete out.secondaryIpRanges
}

// ---- create ----

export function CreateSubnetwork() {
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
      region: 'us-central1',
      network: params.get('network') ?? '',
      ipCidrRange: '',
      description: '',
      secondary: '',
      privateIpGoogleAccess: false,
    },
  })
  const errors = form.formState.errors
  const toBody = (v: CreateValues, base: Body): Body => {
    const out: Body = {
      ...base,
      name: v.name,
      network: networkRef(v.network, project),
      ipCidrRange: v.ipCidrRange.trim(),
    }
    if (v.description) out.description = v.description
    else delete out.description
    if (v.privateIpGoogleAccess) out.privateIpGoogleAccess = true
    else delete out.privateIpGoogleAccess
    setRanges(out, v.secondary)
    return out
  }
  const fromBody = (body: Body, prev: CreateValues): CreateValues => ({
    ...prev,
    name: str(body.name),
    network: networkValue(body.network, project),
    ipCidrRange: str(body.ipCidrRange),
    description: str(body.description),
    secondary: formatRanges(rangesOf(body)),
    privateIpGoogleAccess: body.privateIpGoogleAccess === true,
  })
  const create = useMutation({
    meta: { toast: false },
    mutationFn: ({ body, region }: { body: Body; region: string }) =>
      withFieldError(form, FIELD, () => insertSubnetwork(project, region.trim(), body)),
    onSuccess: (_, { body, region }) => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(`Created subnetwork ${str(body.name)}.`)
      navigate(subnetPage({ region: region.trim(), subnetwork: str(body.name) }))
    },
  })
  return (
    <Card>
      <h1 className="text-lg font-semibold">Create subnetwork</h1>
      <ResourceEditor
        form={form}
        initialBody={toBody(form.getValues(), {})}
        toBody={toBody}
        fromBody={fromBody}
        fields={['name', 'network', 'ipCidrRange', 'description', 'secondary']}
        onSubmit={(body, values) => create.mutateAsync({ body, region: values.region })}
        submitLabel="Create"
        onCancel={() => navigate('/compute/subnetworks')}
        jsonHint={
          <>
            Sent to <span className="font-mono">subnetworks.insert</span> in the region the form
            names.
          </>
        }
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field
            id="subnet-name"
            label="Name"
            error={errors.name?.message}
            hint="Lowercase letters, digits and dashes. Cannot be changed."
          >
            <Input
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              {...fieldAria('subnet-name', errors.name?.message, 'hint')}
              {...form.register('name')}
            />
          </Field>
          <Field
            id="subnet-region"
            label="Region"
            error={errors.region?.message}
            hint="Cannot be changed."
          >
            <Input
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              {...fieldAria('subnet-region', errors.region?.message, 'hint')}
              {...form.register('region')}
            />
          </Field>
          <Field
            id="subnet-network"
            label="Network"
            error={errors.network?.message}
            hint="A VPC network of this Project, or projects/PROJECT/global/networks/NETWORK. Cannot be changed."
          >
            <Input
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              list={`${id}-networks`}
              {...fieldAria('subnet-network', errors.network?.message, 'hint')}
              {...form.register('network')}
            />
            <datalist id={`${id}-networks`}>
              {(networks.data ?? []).map((n) => (
                <option key={n.name} value={n.name} />
              ))}
            </datalist>
          </Field>
          <Field
            id="subnet-range"
            label="Primary range"
            error={errors.ipCidrRange?.message}
            hint="An IPv4 CIDR range, /8 to /29, e.g. 10.0.0.0/24. It can be expanded later, not shrunk."
          >
            <Input
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              {...fieldAria('subnet-range', errors.ipCidrRange?.message, 'hint')}
              {...form.register('ipCidrRange')}
            />
          </Field>
        </div>
        <QueryStatus query={networks} />
        <SharedFields form={form} prefix="subnet" />
        <label className="flex items-center gap-2 text-sm font-medium">
          <input type="checkbox" className="size-4" {...form.register('privateIpGoogleAccess')} />
          Private Google Access
        </label>
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
      <Field
        id={`${prefix}-secondary`}
        label="Secondary ranges"
        error={errors.secondary?.message}
        hint="One per line: a range name and its CIDR range, e.g. pods 10.4.0.0/14. GKE takes its Pod and Service ranges from these."
      >
        <Textarea
          rows={3}
          spellCheck={false}
          className="font-mono"
          {...fieldAria(`${prefix}-secondary`, errors.secondary?.message, 'hint')}
          {...f.register('secondary')}
        />
      </Field>
    </>
  )
}

// ---- edit ----

export function EditSubnetwork() {
  const ref = useSubnetRef()
  const query = useQuery(subnetQuery(ref))
  if (!query.data) return <QueryStatus query={query} />
  return <EditSubnetworkForm subnet={query.data} r={ref} />
}

const editValues = (s: Subnetwork): EditValues => ({
  description: s.description ?? '',
  secondary: formatRanges(s.secondaryIpRanges),
})

function EditSubnetworkForm({ subnet, r }: { subnet: Subnetwork; r: SubnetRef }) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<EditValues>({
    resolver: zodResolver(editSchema),
    defaultValues: editValues(subnet),
  })
  const was = editValues(subnet)
  const toBody = (v: EditValues, base: Body): Body => {
    const out: Body = { ...base, fingerprint: base.fingerprint ?? subnet.fingerprint }
    if (v.description !== was.description) out.description = v.description
    else delete out.description
    // Lists replace: an empty list removes every secondary range.
    if (v.secondary !== was.secondary) out.secondaryIpRanges = parseRanges(v.secondary)
    else delete out.secondaryIpRanges
    return out
  }
  const fromBody = (body: Body): EditValues => ({
    description: 'description' in body ? str(body.description) : was.description,
    secondary: 'secondaryIpRanges' in body ? formatRanges(rangesOf(body)) : was.secondary,
  })
  const back = () => navigate(subnetPage(r))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => {
      if (Object.keys(body).filter((k) => k !== 'fingerprint').length === 0) return false
      await withFieldError(form, FIELD, () => patchSubnetwork(r, body))
      return true
    },
    onSuccess: (changed) => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(changed ? `Updated subnetwork ${r.subnetwork}.` : 'Nothing to change.')
      back()
    },
  })
  return (
    <Card>
      <h1 className="text-lg font-semibold">
        Edit subnetwork <span className="font-mono">{r.subnetwork}</span>{' '}
        <span className="text-sm font-normal text-muted-foreground">
          {r.region}, {subnet.ipCidrRange}, network {lastSeg(subnet.network)}
        </span>
      </h1>
      <ResourceEditor
        form={form}
        initialBody={{ fingerprint: subnet.fingerprint }}
        toBody={toBody}
        fromBody={fromBody}
        fields={['description', 'secondary']}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">subnetworks.patch</span>, which needs the
            subnetwork&apos;s current <span className="font-mono">fingerprint</span>: objects merge,
            lists (such as <span className="font-mono">secondaryIpRanges</span>) replace.
          </>
        }
      >
        <SharedFields form={form} prefix="edit-subnet" />
      </ResourceEditor>
    </Card>
  )
}
