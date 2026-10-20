import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useId } from 'react'
import { useForm, useWatch, type Path, type UseFormReturn } from 'react-hook-form'
import { z } from 'zod'

import { errorMessage } from '@/api/errors'
import { useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Field, fieldAria } from '@/components/resource/Field'
import { ResourceEditor, type Body } from '@/components/resource/ResourceEditor'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { toast } from '@/lib/toast'
import { useViewState } from '@/lib/viewState'

import { lastSeg, relPath } from '../lb/api'
import { invalid, nameMessage, requiredMessage } from '../lb/spec'
import { networksQuery, resourceField, subnetsQuery } from '../vpc/api'
import { insertRouter, patchRouter, routerQuery, type Router, type RouterRef } from './api'
import { routerPage, useRouterRef } from './NatLayout'

// Create and edit forms for Cloud Routers (FR-NAT-001, FR-UI-011):
// create sends routers.insert; edit sends routers.patch with the fields
// that changed. A router's NAT gateways have their own forms; BGP peers
// and interfaces are in the JSON editor.

type Obj = Record<string, unknown>
const obj = (v: unknown): Obj =>
  typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Obj) : {}
const str = (v: unknown) => (typeof v === 'string' ? v : typeof v === 'number' ? String(v) : '')

const NAME = /^[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?$/

/** privateAsn is the API's rule for a router's BGP ASN. */
const privateAsn = (a: number) =>
  (a >= 64512 && a <= 65534) || (a >= 4200000000 && a <= 4294967294) || a === 16550

const asnField = z.string().superRefine((v, ctx) => {
  if (v.trim() && (!/^\d+$/.test(v.trim()) || !privateAsn(Number(v)))) {
    ctx.addIssue({
      code: 'custom',
      message: invalid(
        'bgp.asn',
        v,
        'Must be a private ASN (64512-65534, 4200000000-4294967294) or 16550.',
      ),
    })
  }
})

const createSchema = z.object({
  name: z.string().superRefine((v, ctx) => {
    if (!NAME.test(v)) ctx.addIssue({ code: 'custom', message: nameMessage(v) })
  }),
  region: z.string().superRefine((v, ctx) => {
    if (!v.trim()) ctx.addIssue({ code: 'custom', message: 'Choose a region.' })
  }),
  network: z.string().superRefine((v, ctx) => {
    if (!v.trim()) ctx.addIssue({ code: 'custom', message: requiredMessage('network') })
  }),
  description: z.string(),
  asn: asnField,
})
export type CreateValues = z.infer<typeof createSchema>

const editSchema = z.object({ description: z.string(), asn: asnField })
export type EditValues = z.infer<typeof editSchema>

/** networkRef writes the network field: a bare name is in project. */
export const networkRef = (v: string, project: string) =>
  NAME.test(v.trim()) ? `projects/${project}/global/networks/${v.trim()}` : v.trim()

/** networkValue reads the network field: a name for project's networks. */
export function networkValue(link: unknown, project: string) {
  const p = relPath(str(link))
  return p.startsWith(`projects/${project}/global/networks/`) ? lastSeg(p) : p
}

const FIELD: Record<string, string> = { name: 'name', network: 'network', bgp: 'asn' }

/**
 * withFieldError shows an API error on the field it names, then rethrows
 * it for the editor to show above the buttons.
 */
async function withFieldError<V extends Obj>(form: UseFormReturn<V>, send: () => Promise<unknown>) {
  try {
    return await send()
  } catch (e) {
    const head = resourceField(errorMessage(e))?.split(/[.[]/)[0]
    const f = head && FIELD[head]
    if (f && f in form.getValues()) {
      form.setError(f as Path<V>, { type: 'server', message: errorMessage(e) })
    }
    throw e
  }
}

/** setBgp writes the ASN onto body's bgp, keeping its other fields. */
function setBgp(out: Body, base: Body, asn: string) {
  if (asn.trim()) out.bgp = { ...obj(base.bgp), asn: Number(asn) }
  else if (Object.keys(obj(base.bgp)).some((k) => k !== 'asn'))
    out.bgp = Object.fromEntries(Object.entries(obj(base.bgp)).filter(([k]) => k !== 'asn'))
  else delete out.bgp
}

// ---- create ----

export function CreateRouter() {
  const [view] = useViewState()
  const project = view.project ?? ''
  const id = useId()
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const networks = useQuery(networksQuery(project))
  const subnets = useQuery(subnetsQuery(project))
  const form = useForm<CreateValues>({
    resolver: zodResolver(createSchema),
    defaultValues: { name: '', region: 'us-central1', network: '', description: '', asn: '' },
  })
  const errors = form.formState.errors
  const network = useWatch({ control: form.control, name: 'network' })
  // Regions where the chosen network has subnetworks: a NAT there covers them.
  const regions = [
    ...new Set(
      (subnets.data ?? [])
        .filter((s) => relPath(s.network) === networkRef(network ?? '', project))
        .map((s) => lastSeg(s.region)),
    ),
  ].sort()
  const toBody = (v: CreateValues, base: Body): Body => {
    const out: Body = { ...base, name: v.name, network: networkRef(v.network, project) }
    if (v.description) out.description = v.description
    else delete out.description
    setBgp(out, base, v.asn)
    return out
  }
  const fromBody = (body: Body, prev: CreateValues): CreateValues => ({
    ...prev,
    name: str(body.name),
    network: networkValue(body.network, project),
    description: str(body.description),
    asn: str(obj(body.bgp).asn),
  })
  const create = useMutation({
    meta: { toast: false },
    mutationFn: ({ body, region }: { body: Body; region: string }) =>
      withFieldError(form, () => insertRouter(project, region.trim(), body)),
    onSuccess: (_, { body, region }) => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(`Created router ${str(body.name)}.`)
      navigate(routerPage({ region: region.trim(), router: str(body.name) }))
    },
  })
  return (
    <Card>
      <h1 className="text-lg font-semibold">Create router</h1>
      <ResourceEditor
        form={form}
        initialBody={toBody(form.getValues(), {})}
        toBody={toBody}
        fromBody={fromBody}
        fields={['name', 'network', 'description', 'asn']}
        onSubmit={(body, values) => create.mutateAsync({ body, region: values.region })}
        submitLabel="Create"
        onCancel={() => navigate('/nat')}
        jsonHint={
          <>
            Sent to <span className="font-mono">routers.insert</span> in the region the form names.
            NAT gateways can be added here as <span className="font-mono">nats</span>, or after
            create.
          </>
        }
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field
            id="router-name"
            label="Name"
            error={errors.name?.message}
            hint="Lowercase letters, digits and dashes. Cannot be changed."
          >
            <Input
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              {...fieldAria('router-name', errors.name?.message, 'hint')}
              {...form.register('name')}
            />
          </Field>
          <Field
            id="router-network"
            label="Network"
            error={errors.network?.message}
            hint="A VPC network of this Project, or projects/PROJECT/global/networks/NETWORK. Cannot be changed."
          >
            <Input
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              list={`${id}-networks`}
              {...fieldAria('router-network', errors.network?.message, 'hint')}
              {...form.register('network')}
            />
            <datalist id={`${id}-networks`}>
              {(networks.data ?? []).map((n) => (
                <option key={n.name} value={n.name} />
              ))}
            </datalist>
          </Field>
        </div>
        <QueryStatus query={networks} />
        <Field
          id="router-region"
          label="Region"
          error={errors.region?.message}
          hint={
            regions.length
              ? `The network has subnetworks in ${regions.join(', ')}. Cannot be changed.`
              : 'A NAT gateway on the router covers the network’s subnetworks in this region. Cannot be changed.'
          }
        >
          <Input
            autoComplete="off"
            spellCheck={false}
            className="font-mono"
            list={`${id}-regions`}
            {...fieldAria('router-region', errors.region?.message, 'hint')}
            {...form.register('region')}
          />
          <datalist id={`${id}-regions`}>
            {regions.map((r) => (
              <option key={r} value={r} />
            ))}
          </datalist>
        </Field>
        <SharedFields form={form} prefix="router" />
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
        id={`${prefix}-asn`}
        label="BGP ASN"
        error={errors.asn?.message}
        hint="Optional: a private ASN. Cloud NAT needs none."
      >
        <Input
          inputMode="numeric"
          className="font-mono"
          {...fieldAria(`${prefix}-asn`, errors.asn?.message, 'hint')}
          {...f.register('asn')}
        />
      </Field>
    </>
  )
}

// ---- edit ----

export function EditRouter() {
  const ref = useRouterRef()
  const query = useQuery(routerQuery(ref))
  if (!query.data) return <QueryStatus query={query} />
  return <EditRouterForm router={query.data} r={ref} />
}

const editValues = (r: Router): EditValues => ({
  description: r.description ?? '',
  asn: str(r.bgp?.asn),
})

function EditRouterForm({ router, r }: { router: Router; r: RouterRef }) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<EditValues>({
    resolver: zodResolver(editSchema),
    defaultValues: editValues(router),
  })
  const was = editValues(router)
  const toBody = (v: EditValues, base: Body): Body => {
    const out: Body = { ...base }
    if (v.description !== was.description) out.description = v.description
    else delete out.description
    // A merge patch: bgp's other fields stay, null removes the ASN.
    if (v.asn !== was.asn) out.bgp = { asn: v.asn.trim() ? Number(v.asn) : null }
    else delete out.bgp
    return out
  }
  const fromBody = (body: Body): EditValues => ({
    description: 'description' in body ? str(body.description) : was.description,
    asn: 'bgp' in body ? str(obj(body.bgp).asn) : was.asn,
  })
  const back = () => navigate(routerPage(r))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => {
      if (Object.keys(body).filter((k) => k !== 'fingerprint').length === 0) return false
      await withFieldError(form, () => patchRouter(r, body))
      return true
    },
    onSuccess: (changed) => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(changed ? `Updated router ${r.router}.` : 'Nothing to change.')
      back()
    },
  })
  return (
    <Card>
      <h1 className="text-lg font-semibold">
        Edit router <span className="font-mono">{r.router}</span>{' '}
        <span className="text-sm font-normal text-muted-foreground">
          {r.region}, network {lastSeg(router.network)}
        </span>
      </h1>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={toBody}
        fromBody={fromBody}
        fields={['description', 'asn']}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">routers.patch</span>: objects merge, lists (such as{' '}
            <span className="font-mono">nats</span>) replace.
          </>
        }
      >
        <SharedFields form={form} prefix="edit-router" />
      </ResourceEditor>
    </Card>
  )
}
