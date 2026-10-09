import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm, useWatch, type Resolver, type UseFormReturn } from 'react-hook-form'
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

import {
  DATABASE_VERSIONS,
  flagsQuery,
  insertInstance,
  instanceQuery,
  NAME,
  nameMessage,
  patchInstance,
  type AclEntry,
  type DatabaseInstance,
  type Flag,
  type InstanceRef,
  type Settings,
} from './api'
import {
  flagsError,
  formatFlags,
  formatNetworks,
  networksError,
  parseFlags,
  parseNetworks,
} from './settings'
import { instancePath, useInstanceRef } from './SqlLayout'

// Create and edit forms for instances (FR-SQL-001, FR-SQL-003, FR-UI-011).
// Create sends instances.insert's DatabaseInstance; edit sends
// instances.patch with the settings that changed.

type Obj = Record<string, unknown>
const obj = (v: unknown): Obj =>
  typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Obj) : {}
const str = (v: unknown) => (typeof v === 'string' ? v : '')

/** put sets key on o, or removes it when value is undefined. */
function put(o: Obj, key: string, value: unknown) {
  if (value === undefined) delete o[key]
  else o[key] = value
}

const DEFAULT_REGION = 'us-central1'
export const SSL_MODES = [
  'ALLOW_UNENCRYPTED_AND_ENCRYPTED',
  'ENCRYPTED_ONLY',
  'TRUSTED_CLIENT_CERTIFICATE_REQUIRED',
] as const

/** settingsSchema checks the settings fields, flags against known when loaded. */
function settingsSchema(known?: Flag[]) {
  return z.object({
    tier: z.string().trim(),
    edition: z.string(),
    availabilityType: z.string(),
    diskSizeGb: z
      .number({ error: 'The disk size must be a whole number of GB.' })
      .int('The disk size must be a whole number of GB.')
      .min(10, 'Invalid request: The data disk size must be at least 10 GB.'),
    publicIp: z.boolean(),
    authorizedNetworks: z.string().superRefine((v, ctx) => {
      const err = networksError(v)
      if (err) ctx.addIssue({ code: 'custom', message: err })
    }),
    sslMode: z.string(),
    dataApi: z.boolean(),
    deletionProtection: z.boolean(),
    flags: z.string().superRefine((v, ctx) => {
      const err = flagsError(v, known)
      if (err) ctx.addIssue({ code: 'custom', message: err })
    }),
    labels: z.string().superRefine((v, ctx) => {
      const err = labelsError(v)
      if (err) ctx.addIssue({ code: 'custom', message: err })
    }),
  })
}
type SettingsValues = z.infer<ReturnType<typeof settingsSchema>>

function createSchema(known?: Flag[]) {
  return settingsSchema(known).extend({
    name: z.string().superRefine((v, ctx) => {
      if (!NAME.test(v)) ctx.addIssue({ code: 'custom', message: nameMessage(v) })
    }),
    databaseVersion: z.string(),
    region: z.string().trim().min(1, 'Invalid request: Invalid region ().'),
    rootPassword: z.string(),
  })
}
type CreateValues = z.infer<ReturnType<typeof createSchema>>

/** forVersion keeps the flags a database version allows. */
const forVersion = (flags: Flag[] | undefined, version: string) =>
  flags?.filter((f) => !f.appliesTo || f.appliesTo.includes(version))

/**
 * flagsResolver validates with the flags the form's version allows, once
 * the list is loaded. useForm takes the resolver of every render.
 */
function flagsResolver<V extends SettingsValues>(
  schema: (known?: Flag[]) => z.ZodType<V, V>,
  flags: Flag[] | undefined,
  version: (values: V) => string,
): Resolver<V> {
  return (values, ctx, opts) =>
    zodResolver(schema(forVersion(flags, version(values))))(values, ctx, opts)
}

/** settingsValues reads the form's settings out of an instance's settings. */
function settingsValues(s: Settings = {}): SettingsValues {
  return {
    tier: s.tier ?? '',
    edition: s.edition ?? '',
    availabilityType: s.availabilityType ?? 'ZONAL',
    diskSizeGb: Number(s.dataDiskSizeGb ?? 10),
    publicIp: s.ipConfiguration?.ipv4Enabled ?? true,
    authorizedNetworks: formatNetworks(s.ipConfiguration?.authorizedNetworks),
    sslMode: s.ipConfiguration?.sslMode ?? 'ALLOW_UNENCRYPTED_AND_ENCRYPTED',
    dataApi: s.dataApiAccess === 'ALLOW_DATA_API',
    deletionProtection: !!s.deletionProtectionEnabled,
    flags: formatFlags(s.databaseFlags),
    labels: formatLabels(s.userLabels),
  }
}

/** writeSettings writes the form's settings onto a settings object. */
function writeSettings(st: Obj, v: SettingsValues) {
  put(st, 'tier', v.tier || undefined)
  put(st, 'edition', v.edition || undefined)
  st.availabilityType = v.availabilityType
  st.dataDiskSizeGb = String(v.diskSizeGb)
  const ip = { ...obj(st.ipConfiguration) }
  ip.ipv4Enabled = v.publicIp
  const acl = parseNetworks(v.authorizedNetworks)
  put(ip, 'authorizedNetworks', acl.length ? acl : undefined)
  ip.sslMode = v.sslMode
  st.ipConfiguration = ip
  st.dataApiAccess = v.dataApi ? 'ALLOW_DATA_API' : 'DISALLOW_DATA_API'
  st.deletionProtectionEnabled = v.deletionProtection
  const flags = parseFlags(v.flags)
  put(st, 'databaseFlags', flags.length ? flags : undefined)
  const labels = parseLabels(v.labels)
  put(st, 'userLabels', Object.keys(labels).length ? labels : undefined)
}

/** readSettings reads the form's settings back out of a settings object. */
function readSettings(st: Obj, prev: SettingsValues): SettingsValues {
  const ip = obj(st.ipConfiguration)
  return {
    tier: str(st.tier),
    edition: str(st.edition),
    availabilityType: str(st.availabilityType) || prev.availabilityType,
    diskSizeGb: st.dataDiskSizeGb === undefined ? prev.diskSizeGb : Number(st.dataDiskSizeGb),
    publicIp: typeof ip.ipv4Enabled === 'boolean' ? ip.ipv4Enabled : prev.publicIp,
    authorizedNetworks: formatNetworks(ip.authorizedNetworks as AclEntry[] | undefined),
    sslMode: str(ip.sslMode) || prev.sslMode,
    dataApi: st.dataApiAccess === undefined ? prev.dataApi : st.dataApiAccess === 'ALLOW_DATA_API',
    deletionProtection:
      typeof st.deletionProtectionEnabled === 'boolean'
        ? st.deletionProtectionEnabled
        : prev.deletionProtection,
    flags: formatFlags(st.databaseFlags as Settings['databaseFlags']),
    labels: formatLabels(obj(st.userLabels) as Record<string, string>),
  }
}

// ---- create ----

export function createBody(v: CreateValues, base: Body): Body {
  const b: Body = { ...base, name: v.name, databaseVersion: v.databaseVersion, region: v.region }
  put(b, 'rootPassword', v.rootPassword || undefined)
  const st = { ...obj(base.settings) }
  writeSettings(st, v)
  b.settings = st
  return b
}

export function createValues(body: Body, prev: CreateValues): CreateValues {
  return {
    ...readSettings(obj(body.settings), prev),
    name: str(body.name),
    databaseVersion: str(body.databaseVersion) || prev.databaseVersion,
    region: str(body.region),
    rootPassword: str(body.rootPassword),
  }
}

export function CreateInstance() {
  const [view] = useViewState()
  const project = view.project ?? ''
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const defaults: CreateValues = {
    ...settingsValues(),
    name: '',
    databaseVersion: DATABASE_VERSIONS[0],
    region: view.location || DEFAULT_REGION,
    rootPassword: '',
  }
  const flags = useQuery(flagsQuery())
  const form = useForm<CreateValues>({
    resolver: flagsResolver(createSchema, flags.data, (v) => v.databaseVersion),
    defaultValues: defaults,
  })
  const version = useWatch({ control: form.control, name: 'databaseVersion' })
  const errors = form.formState.errors
  const create = useMutation({
    meta: { toast: false },
    mutationFn: (body: Body) => insertInstance(project, body),
    onSuccess: (_op, body) => {
      void qc.invalidateQueries({ queryKey: ['sql'] })
      toast.success(`Creating instance ${str(body.name)}. Follow it in Operations.`)
      navigate(instancePath(str(body.name)))
    },
  })

  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-2xl font-semibold">Create instance</h1>
        <p className="text-sm text-muted-foreground">
          A Cloud SQL for PostgreSQL instance in Project{' '}
          <span className="font-mono">{project}</span>, backed by a real PostgreSQL server in a
          container.
        </p>
      </div>
      <Card>
        <ResourceEditor
          form={form}
          initialBody={createBody(defaults, { settings: {} })}
          toBody={createBody}
          fromBody={createValues}
          fields={['name', 'region', 'flags', 'authorizedNetworks']}
          onSubmit={(body) => create.mutateAsync(body)}
          submitLabel="Create"
          onCancel={() => navigate('/sql')}
          jsonHint={
            <>
              Sent to <span className="font-mono">instances.insert</span>.
            </>
          }
        >
          <Field id="instance-name" label="Instance ID" error={errors.name?.message}>
            <Input
              autoComplete="off"
              spellCheck={false}
              {...fieldAria('instance-name', errors.name?.message)}
              {...form.register('name')}
            />
          </Field>
          <div className="grid gap-4 sm:grid-cols-2">
            <Field id="instance-version" label="Database version">
              <NativeSelect id="instance-version" {...form.register('databaseVersion')}>
                {DATABASE_VERSIONS.map((v) => (
                  <option key={v} value={v}>
                    {v}
                  </option>
                ))}
              </NativeSelect>
            </Field>
            <Field id="instance-region" label="Region" error={errors.region?.message}>
              <Input
                spellCheck={false}
                {...fieldAria('instance-region', errors.region?.message)}
                {...form.register('region')}
              />
            </Field>
          </div>
          <Field
            id="instance-password"
            label="Password of the postgres user"
            hint="Empty leaves the postgres user without a password."
          >
            <Input
              type="password"
              autoComplete="new-password"
              {...fieldAria('instance-password', undefined, 'Empty')}
              {...form.register('rootPassword')}
            />
          </Field>
          <SettingsFields form={form} version={version} prefix="instance" />
        </ResourceEditor>
      </Card>
    </div>
  )
}

/**
 * SettingsFields are the settings both forms edit. Tier, edition and disk
 * size are stored and reported, not enforced.
 */
function SettingsFields<V extends SettingsValues>({
  form,
  version,
  prefix,
}: {
  form: UseFormReturn<V>
  version: string
  prefix: string
}) {
  // The fields are SettingsValues' own; the cast lets both forms share them.
  const f = form as unknown as UseFormReturn<SettingsValues>
  const errors = f.formState.errors
  const flags = forVersion(useQuery(flagsQuery()).data, version)
  const id = (name: string) => `${prefix}-${name}`
  return (
    <>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id={id('edition')} label="Edition">
          <NativeSelect id={id('edition')} {...f.register('edition')}>
            <option value="">Default (by version and tier)</option>
            <option value="ENTERPRISE">ENTERPRISE</option>
            <option value="ENTERPRISE_PLUS">ENTERPRISE_PLUS</option>
          </NativeSelect>
        </Field>
        <Field
          id={id('tier')}
          label="Machine tier"
          hint="Empty for the edition's default; Enterprise Plus takes db-perf-optimized-* tiers."
        >
          <Input
            spellCheck={false}
            {...fieldAria(id('tier'), undefined, 'Empty')}
            {...f.register('tier')}
          />
        </Field>
      </div>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id={id('availability')} label="Availability">
          <NativeSelect id={id('availability')} {...f.register('availabilityType')}>
            <option value="ZONAL">ZONAL</option>
            <option value="REGIONAL">REGIONAL (stored)</option>
          </NativeSelect>
        </Field>
        <Field id={id('disk')} label="Disk size (GB)" error={errors.diskSizeGb?.message}>
          <Input
            type="number"
            min={10}
            {...fieldAria(id('disk'), errors.diskSizeGb?.message)}
            {...f.register('diskSizeGb', { valueAsNumber: true })}
          />
        </Field>
      </div>
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" className="size-4" {...f.register('publicIp')} />
        Public IP
      </label>
      <Field
        id={id('networks')}
        label="Authorized networks"
        error={errors.authorizedNetworks?.message}
        hint="CIDR ranges or addresses allowed on the public IP, one per line, optionally name=range."
      >
        <Textarea
          rows={2}
          spellCheck={false}
          className="font-mono"
          {...fieldAria(id('networks'), errors.authorizedNetworks?.message, 'CIDR')}
          {...f.register('authorizedNetworks')}
        />
      </Field>
      <Field id={id('ssl')} label="SSL mode">
        <NativeSelect id={id('ssl')} {...f.register('sslMode')}>
          {SSL_MODES.map((m) => (
            <option key={m} value={m}>
              {m}
            </option>
          ))}
        </NativeSelect>
      </Field>
      <Field
        id={id('flags')}
        label="Database flags"
        error={errors.flags?.message}
        hint={
          <>
            name=value, one per line; {flags?.length ?? 'the allowed'} flags apply to {version}.
            Restart-required flags restart the instance.
          </>
        }
      >
        <Textarea
          rows={3}
          spellCheck={false}
          className="font-mono"
          {...fieldAria(id('flags'), errors.flags?.message, 'name=value')}
          {...f.register('flags')}
        />
      </Field>
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" className="size-4" {...f.register('dataApi')} />
        Data API access (instances.executeSql, used by the query runner)
      </label>
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" className="size-4" {...f.register('deletionProtection')} />
        Deletion protection
      </label>
      <Field
        id={id('labels')}
        label="Labels"
        error={errors.labels?.message}
        hint="key=value, one per line"
      >
        <Textarea
          rows={2}
          spellCheck={false}
          className="font-mono"
          {...fieldAria(id('labels'), errors.labels?.message, 'key=value')}
          {...f.register('labels')}
        />
      </Field>
    </>
  )
}

// ---- edit ----

const same = (a: unknown, b: unknown) => JSON.stringify(a ?? null) === JSON.stringify(b ?? null)

/**
 * patchBody writes what the edit form changed onto an instances.patch
 * body: changed settings only, with removed labels set to null.
 */
export function patchBody(i: DatabaseInstance, v: SettingsValues, base: Body): Body {
  const cur = obj(i.settings)
  const next: Obj = { ...cur }
  writeSettings(next, v)
  const st = { ...obj(base.settings) }
  for (const key of [
    'tier',
    'edition',
    'availabilityType',
    'dataDiskSizeGb',
    'dataApiAccess',
    'deletionProtectionEnabled',
  ]) {
    // An emptied tier or edition keeps the instance's.
    if (next[key] !== undefined && !same(next[key], cur[key])) st[key] = next[key]
    else delete st[key]
  }
  const ipCur = obj(cur.ipConfiguration)
  const ipNext = obj(next.ipConfiguration)
  const ip = { ...obj(st.ipConfiguration) }
  for (const key of ['ipv4Enabled', 'sslMode']) {
    put(ip, key, same(ipNext[key], ipCur[key]) ? undefined : ipNext[key])
  }
  put(
    ip,
    'authorizedNetworks',
    same(ipNext.authorizedNetworks ?? [], ipCur.authorizedNetworks ?? [])
      ? undefined
      : (ipNext.authorizedNetworks ?? []),
  )
  put(st, 'ipConfiguration', Object.keys(ip).length ? ip : undefined)
  put(
    st,
    'databaseFlags',
    same(next.databaseFlags ?? [], cur.databaseFlags ?? [])
      ? undefined
      : (next.databaseFlags ?? []),
  )
  const was = obj(cur.userLabels) as Record<string, string>
  const want = obj(next.userLabels) as Record<string, string>
  if (!same(Object.entries(was).sort(), Object.entries(want).sort())) {
    const labels: Record<string, string | null> = { ...want }
    for (const k of Object.keys(was)) if (!(k in want)) labels[k] = null
    st.userLabels = labels
  } else delete st.userLabels
  return { ...base, settings: st }
}

export function patchValues(i: DatabaseInstance) {
  return (body: Body, prev: SettingsValues): SettingsValues => {
    const was = settingsValues(i.settings)
    const st = obj(body.settings)
    const ip = obj(st.ipConfiguration)
    const labels = st.userLabels === undefined ? undefined : obj(st.userLabels)
    return {
      ...readSettings(
        {
          ...obj(i.settings),
          ...st,
          ipConfiguration: { ...obj(i.settings?.ipConfiguration), ...ip },
        },
        was,
      ),
      labels:
        labels === undefined
          ? prev.labels
          : formatLabels(
              Object.fromEntries(
                Object.entries(labels).filter((e): e is [string, string] => e[1] !== null),
              ),
            ),
    }
  }
}

export function EditInstance() {
  const ref = useInstanceRef()
  const query = useQuery(instanceQuery(ref))
  return (
    <div className="flex flex-col gap-6">
      <h1 className="text-2xl font-semibold">
        Edit instance <span className="font-mono">{ref.instance}</span>
      </h1>
      <QueryStatus query={query} />
      {query.data && <EditInstanceForm instance={query.data} instanceRef={ref} />}
    </div>
  )
}

function EditInstanceForm({
  instance,
  instanceRef,
}: {
  instance: DatabaseInstance
  instanceRef: InstanceRef
}) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const version = instance.databaseVersion ?? DATABASE_VERSIONS[0]
  const flags = useQuery(flagsQuery())
  const form = useForm<SettingsValues>({
    resolver: flagsResolver(settingsSchema, flags.data, () => version),
    defaultValues: settingsValues(instance.settings),
  })
  const back = () => navigate(instancePath(instanceRef.instance))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => {
      if (Object.keys(obj(body.settings)).length === 0 && Object.keys(body).length === 1)
        return false
      await patchInstance(instanceRef, body)
      return true
    },
    onSuccess: (changed) => {
      void qc.invalidateQueries({ queryKey: ['sql'] })
      toast.success(changed ? `Updating instance ${instance.name}.` : 'Nothing to change.')
      back()
    },
  })
  return (
    <Card>
      <ResourceEditor
        form={form}
        initialBody={{ settings: {} }}
        toBody={(v, base) => patchBody(instance, v, base)}
        fromBody={patchValues(instance)}
        fields={['flags', 'authorizedNetworks']}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">instances.patch</span>: objects merge, lists replace
            and null removes a field.
          </>
        }
      >
        <p className="text-sm text-muted-foreground">
          <span className="font-mono">{version}</span> in{' '}
          <span className="font-mono">{instance.region}</span>. Start and stop the instance from its
          page.
        </p>
        <SettingsFields form={form} version={version} prefix="edit" />
      </ResourceEditor>
    </Card>
  )
}
