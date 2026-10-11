import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm, useWatch, type UseFormReturn } from 'react-hook-form'
import { useParams } from 'react-router'
import { z } from 'zod'

import { errorMessage } from '@/api/errors'
import { useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Field, fieldAria } from '@/components/resource/Field'
import { ResourceEditor, type Body } from '@/components/resource/ResourceEditor'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import { toast } from '@/lib/toast'

import {
  apiField,
  applyChange,
  inZone,
  invalid,
  isDnsName,
  required,
  rrsetKey,
  rrsetsQuery,
  TYPES,
  type ManagedZone,
  type ResourceRecordSet,
  type ZoneRef,
} from './api'
import { useZoneRef, zonePath } from './DnsLayout'
import { useZone } from './ZonePage'

// Add and edit forms for record sets (FR-DNS-002, FR-UI-011). Both send
// changes.create: adding is a change with the record set as its addition;
// editing deletes the record set as it is and adds it as edited, in one
// change, as Cloud DNS replaces record sets. Routing policies are in the
// JSON editor.

const str = (v: unknown) => (typeof v === 'string' ? v : '')
const lines = (text: string) =>
  text
    .split('\n')
    .map((s) => s.trim())
    .filter(Boolean)

const A = 'entity.change.additions[0]'
const MAX_TTL = 2 ** 31 - 1

/** fullName is the record set's DNS name: a name in the zone, or the zone's own. */
export function fullName(name: string, zone: ManagedZone): string {
  const n = name.trim()
  if (n === '' || n === '@') return zone.dnsName
  if (n.endsWith('.')) return n
  return `${n}.${zone.dnsName}`
}

/** shortName is the part of a DNS name the form shows before the zone's. */
export function shortName(name: string, zone: ManagedZone): string {
  if (name.toLowerCase() === zone.dnsName.toLowerCase()) return ''
  const suffix = `.${zone.dnsName}`
  return name.toLowerCase().endsWith(suffix.toLowerCase()) ? name.slice(0, -suffix.length) : name
}

export interface Values {
  name: string
  type: string
  ttl: string
  rrdatas: string
  /** routed: the record set answers through a routing policy, not rrdatas. */
  routed: boolean
}

/**
 * schema checks what the API checks of an addition, with its messages;
 * the data of each type is checked by the API.
 */
const schema = (zone: ManagedZone) =>
  z
    .object({
      name: z.string(),
      type: z.string(),
      ttl: z.string(),
      rrdatas: z.string(),
      routed: z.boolean(),
    })
    .superRefine((v, ctx) => {
      const issue = (path: keyof Values, message: string) =>
        ctx.addIssue({ code: 'custom', path: [path], message })
      const name = fullName(v.name, zone)
      if (!isDnsName(name) || !inZone(name, zone.dnsName)) issue('name', invalid(`${A}.name`, name))
      if (!/^\d+$/.test(v.ttl.trim()) || Number(v.ttl) > MAX_TTL)
        issue('ttl', invalid(`${A}.ttl`, v.ttl))
      const data = lines(v.rrdatas)
      const apex = name.toLowerCase() === zone.dnsName.toLowerCase()
      if (v.type === 'CNAME' && apex) {
        issue(
          'name',
          `The resource '${A}' named '${name} (CNAME)' cannot be created because a CNAME record set is not permitted at the zone apex.`,
        )
      }
      if (v.type === 'SOA' && !apex) issue('name', invalid(`${A}.name`, name))
      if (v.routed) return
      if (data.length === 0) issue('rrdatas', required(`${A}.rrdata`))
      else if ((v.type === 'CNAME' || v.type === 'SOA') && data.length !== 1)
        issue('rrdatas', invalid(`${A}.rrdata`, data.join(',')))
    })

/** API fields of a change the form shows errors on. */
const FIELDS = [
  [/^entity\.change\.(additions|deletions)\[\d+\]$/, 'name'],
  [/^entity\.change\.additions\[\d+\]\.name$/, 'name'],
  [/^entity\.change\.additions\[\d+\]\.type$/, 'type'],
  [/^entity\.change\.additions\[\d+\]\.ttl$/, 'ttl'],
  [/^entity\.change\.additions\[\d+\]\.rrdata/, 'rrdatas'],
] as const

/** recordValues reads the form's values out of a ResourceRecordSet body. */
export const recordValues =
  (zone: ManagedZone) =>
  (body: Body): Values => ({
    name: shortName(str(body.name), zone),
    type: str(body.type) || 'A',
    ttl: typeof body.ttl === 'number' ? String(body.ttl) : str(body.ttl),
    rrdatas: (Array.isArray(body.rrdatas) ? body.rrdatas.map(str) : []).join('\n'),
    routed: !!body.routingPolicy,
  })

/** recordBody writes the form's values onto a record set (a change's addition). */
export const recordBody =
  (zone: ManagedZone) =>
  (v: Values, base: Body): Body => {
    const out: Body = { ...base, name: fullName(v.name, zone), type: v.type, ttl: Number(v.ttl) }
    delete out.kind
    if (base.routingPolicy) delete out.rrdatas
    else out.rrdatas = lines(v.rrdatas)
    return out
  }

/** RecordFields are the record set form's fields. */
function RecordFields({ form, zone }: { form: UseFormReturn<Values>; zone: ManagedZone }) {
  const errors = form.formState.errors
  const type = useWatch({ control: form.control, name: 'type' })
  const routed = useWatch({ control: form.control, name: 'routed' })
  return (
    <>
      <Field
        id="rrset-name"
        label="DNS name"
        error={errors.name?.message}
        hint={
          <>
            Before <span className="font-mono">{zone.dnsName}</span>; empty for the zone itself, or
            a full name ending with a dot.
          </>
        }
      >
        <div className="flex items-center gap-2">
          <Input
            autoComplete="off"
            spellCheck={false}
            className="font-mono"
            {...fieldAria('rrset-name', errors.name?.message, 'Before')}
            {...form.register('name')}
          />
          <span className="font-mono text-sm whitespace-nowrap text-muted-foreground">
            .{zone.dnsName}
          </span>
        </div>
      </Field>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id="rrset-type" label="Type" error={errors.type?.message}>
          <NativeSelect
            {...fieldAria('rrset-type', errors.type?.message)}
            {...form.register('type')}
          >
            {TYPES.map((t) => (
              <option key={t} value={t}>
                {t}
              </option>
            ))}
          </NativeSelect>
        </Field>
        <Field id="rrset-ttl" label="TTL (seconds)" error={errors.ttl?.message}>
          <Input
            inputMode="numeric"
            {...fieldAria('rrset-ttl', errors.ttl?.message)}
            {...form.register('ttl')}
          />
        </Field>
      </div>
      {routed ? (
        <p className="text-sm text-muted-foreground">
          This record set answers through a routing policy, which the JSON tab edits.
        </p>
      ) : (
        <Field
          id="rrset-rrdatas"
          label="Data"
          error={errors.rrdatas?.message}
          hint={dataHint(type)}
        >
          <Textarea
            rows={4}
            spellCheck={false}
            className="font-mono"
            {...fieldAria('rrset-rrdatas', errors.rrdatas?.message, 'hint')}
            {...form.register('rrdatas')}
          />
        </Field>
      )}
    </>
  )
}

const EXAMPLES: Record<string, string> = {
  A: '192.0.2.10',
  AAAA: '2001:db8::10',
  CNAME: 'www.example.com.',
  MX: '10 mail.example.com.',
  TXT: '"v=spf1 -all"',
  SRV: '0 5 443 svc.example.com.',
  NS: 'ns1.example.com.',
  CAA: '0 issue "letsencrypt.org"',
  PTR: 'host.example.com.',
}

const dataHint = (type: string) =>
  `One record per line${EXAMPLES[type] ? `, e.g. ${EXAMPLES[type]}` : ''}.`

/**
 * RecordSetEditor sends the form's record set as a change: an addition,
 * and with was, the deletion of the record set it replaces.
 */
function RecordSetEditor({
  zone,
  r,
  was,
}: {
  zone: ManagedZone
  r: ZoneRef
  was?: ResourceRecordSet
}) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const initial: Body = was
    ? (structuredClone(was) as unknown as Body)
    : { name: zone.dnsName, type: 'A', ttl: 300, rrdatas: [] }
  delete initial.kind
  const form = useForm<Values>({
    resolver: zodResolver(schema(zone)),
    defaultValues: was
      ? recordValues(zone)(initial)
      : { name: '', type: 'A', ttl: '300', rrdatas: '', routed: false },
  })
  const back = () => navigate(zonePath(r.zone))
  const send = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => {
      if (was && JSON.stringify(body) === JSON.stringify(initial)) return undefined
      try {
        return await applyChange(r, {
          additions: [body as unknown as ResourceRecordSet],
          ...(was ? { deletions: [was] } : {}),
        })
      } catch (e) {
        const f = apiField(errorMessage(e), FIELDS)
        if (f) form.setError(f, { type: 'server', message: errorMessage(e) })
        throw e
      }
    },
    onSuccess: (c, body) => {
      void qc.invalidateQueries({ queryKey: ['dns'] })
      const label = `${str(body.name)} (${str(body.type)})`
      toast.success(
        !c
          ? 'Nothing to change.'
          : was
            ? `Updated record set ${label} in change ${c.id}.`
            : `Added record set ${label} in change ${c.id}.`,
      )
      back()
    },
  })
  return (
    <ResourceEditor
      form={form}
      initialBody={initial}
      toBody={recordBody(zone)}
      fromBody={recordValues(zone)}
      fields={['name', 'type', 'ttl', 'rrdatas']}
      onSubmit={(body) => send.mutateAsync(body)}
      submitLabel={was ? 'Save' : 'Add'}
      onCancel={back}
      jsonHint={
        <>
          The record set, sent to <span className="font-mono">changes.create</span> as the
          change&apos;s addition
          {was && ', with the record set as it is as its deletion'}.
        </>
      }
    >
      <RecordFields form={form} zone={zone} />
    </ResourceEditor>
  )
}

export function CreateRecordSet() {
  const zone = useZone()
  const r = useZoneRef()
  return (
    <Card>
      <h2 className="text-lg font-semibold">Add record set</h2>
      <RecordSetEditor zone={zone} r={r} />
    </Card>
  )
}

export function EditRecordSet() {
  const zone = useZone()
  const r = useZoneRef()
  const { name = '', type = '' } = useParams()
  const query = useQuery(rrsetsQuery(r))
  if (!query.data) return <QueryStatus query={query} />
  const was = query.data.find((rs) => rrsetKey(rs) === rrsetKey({ name, type }))
  if (!was) {
    return (
      <Card role="alert">
        <p className="text-sm">
          Record set <span className="font-mono">{name}</span> ({type}) is not in zone{' '}
          <span className="font-mono">{r.zone}</span>.
        </p>
      </Card>
    )
  }
  return (
    <Card>
      <h2 className="text-lg font-semibold">
        Edit record set <span className="font-mono">{was.name}</span> ({was.type})
      </h2>
      <RecordSetEditor key={rrsetKey(was)} zone={zone} r={r} was={was} />
    </Card>
  )
}
