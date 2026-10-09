import { useMutation, useQueries, useQuery, useQueryClient } from '@tanstack/react-query'
import { Plus, Trash2 } from 'lucide-react'
import {
  Controller,
  useForm,
  useWatch,
  type FieldErrors,
  type Resolver,
  type UseFormReturn,
} from 'react-hook-form'
import { useParams } from 'react-router'

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
import { cn } from '@/lib/utils'
import { useViewState } from '@/lib/viewState'

import {
  insertResource,
  kindInfo,
  kindQuery,
  lastSeg,
  negsQuery,
  patchResource,
  relPath,
  resourceQuery,
  text,
  type KindInfo,
  type Ref,
  type Resource,
} from './api'
import { lbPath, listPath, useLbRef } from './LbLayout'
import {
  createBody,
  initialValues,
  patchBody,
  patchValues,
  readValues,
  SPECS,
  short,
  validate,
  type FieldSpec,
  type KindSpec,
  type Matcher,
  type Target,
  type Values,
} from './spec'

// Create and edit forms of every load-balancing kind (FR-UI-011), built
// from the kind's field specs (spec.ts). Writes are compute Operations,
// which the forms wait for. Create sends insert; edit sends patch, a JSON
// merge patch of what changed.

/** apiMethod names the API method a request goes to, e.g. regionUrlMaps.patch. */
export function apiMethod(k: KindInfo, region: string, verb: string) {
  if (!region) return `${k.api}.${verb}`
  if (k.coll === 'forwardingRules') return `forwardingRules.${verb}`
  return `region${k.api[0]!.toUpperCase()}${k.api.slice(1)}.${verb}`
}

function resolver(spec: KindSpec, edit: boolean): Resolver<Values> {
  return (values) => {
    const errs = validate(spec, values, edit)
    if (Object.keys(errs).length === 0) return { values, errors: {} }
    const errors: FieldErrors<Values> = {}
    for (const [k, message] of Object.entries(errs)) errors[k] = { type: 'validate', message }
    return { values: {}, errors }
  }
}

interface Option {
  value: string
  label: string
}

/**
 * useRefOptions lists the resources a reference field offers: those of its
 * collections in the form's scope (any zone for NEGs).
 */
function useRefOptions(project: string, region: string, targets: readonly Target[]): Option[] {
  const results = useQueries({
    queries: targets.map((t) =>
      t === 'networkEndpointGroups' ? negsQuery(project) : kindQuery(project, t),
    ),
  })
  const scope = region ? `/regions/${region}/` : '/global/'
  return targets.flatMap((t, i) =>
    (results[i]?.data ?? [])
      .filter((r) => t === 'networkEndpointGroups' || (r.selfLink ?? '').includes(scope))
      .map((r) => ({
        value: relPath(r.selfLink),
        label:
          t === 'networkEndpointGroups'
            ? `${r.name} (${lastSeg(r.zone as string | undefined) || lastSeg(r.region) || 'global'})`
            : targets.length > 1
              ? `${r.name} (${kindInfo(t)?.singular ?? t})`
              : r.name,
      }))
      .sort((a, b) => a.label.localeCompare(b.label)),
  )
}

/** withCurrent adds values the options lack (e.g. in another Project). */
const withCurrent = (opts: Option[], cur: string[]) => [
  ...opts,
  ...cur
    .filter((c) => c && !opts.some((o) => o.value === c))
    .map((c) => ({ value: c, label: short(c) })),
]

function SpecField({
  f,
  form,
  values,
  prefix,
  project,
  region,
}: {
  f: FieldSpec
  form: UseFormReturn<Values>
  values: Values
  prefix: string
  project: string
  region: string
}) {
  const id = `${prefix}-${f.key}`
  const error = form.formState.errors[f.key]?.message
  const reg = form.register(f.key)
  const options = useRefOptions(project, region, f.targets ?? [])
  const mono = f.mono ? 'font-mono' : undefined
  switch (f.type) {
    case 'checkbox':
      return (
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" className="size-4" {...reg} />
          {f.label}
        </label>
      )
    case 'routing':
      return (
        <Controller
          control={form.control}
          name={f.key}
          render={({ field }) => (
            <RoutingEditor
              id={id}
              label={f.label}
              error={error}
              value={field.value as Matcher[]}
              onChange={field.onChange}
              options={withCurrent(
                options,
                (field.value as Matcher[]).map((m) => m.defaultService),
              )}
            />
          )}
        />
      )
  }
  let control
  switch (f.type) {
    case 'select':
      control = (
        <NativeSelect className={mono} {...fieldAria(id, error, f.hint)} {...reg}>
          {f.emptyOption !== undefined && <option value="">{f.emptyOption}</option>}
          {f.options?.map((o) => (
            <option key={o} value={o}>
              {o}
            </option>
          ))}
        </NativeSelect>
      )
      break
    case 'ref':
      control = (
        <NativeSelect {...fieldAria(id, error, f.hint)} {...reg}>
          <option value="">{f.emptyOption ?? 'Choose…'}</option>
          {withCurrent(options, [values[f.key] as string]).map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </NativeSelect>
      )
      break
    case 'refs': {
      const opts = withCurrent(options, values[f.key] as string[])
      control =
        opts.length === 0 ? (
          <p id={id} className="text-sm text-muted-foreground">
            None to choose from yet.
          </p>
        ) : (
          <select
            multiple
            size={Math.min(6, Math.max(2, opts.length))}
            className="rounded-md border bg-transparent p-1 text-sm outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50"
            {...fieldAria(id, error, f.hint)}
            {...reg}
          >
            {opts.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
        )
      break
    }
    case 'textarea':
    case 'lines':
    case 'labels':
      control = (
        <Textarea
          rows={f.type === 'textarea' ? 6 : 3}
          spellCheck={false}
          className={cn(mono, f.type === 'textarea' && 'text-xs')}
          {...fieldAria(id, error, f.hint)}
          {...reg}
        />
      )
      break
    case 'number':
      control = (
        <Input type="number" inputMode="numeric" {...fieldAria(id, error, f.hint)} {...reg} />
      )
      break
    default:
      control = (
        <Input
          autoComplete="off"
          spellCheck={false}
          className={mono}
          {...fieldAria(id, error, f.hint)}
          {...reg}
        />
      )
  }
  return (
    <Field id={id} label={f.label} error={error} hint={f.hint}>
      {control}
    </Field>
  )
}

/**
 * RoutingEditor edits a URL map's path matchers, each with the hosts
 * routed to it, its default backend and its path rules.
 */
function RoutingEditor({
  id,
  label,
  error,
  value,
  onChange,
  options,
}: {
  id: string
  label: string
  error?: string
  value: Matcher[]
  onChange: (v: Matcher[]) => void
  options: Option[]
}) {
  const set = (i: number, patch: Partial<Matcher>) =>
    onChange(value.map((m, j) => (j === i ? { ...m, ...patch } : m)))
  return (
    <fieldset className="flex flex-col gap-3" aria-describedby={error ? `${id}-error` : undefined}>
      <legend className="mb-1 text-sm font-medium">{label}</legend>
      <p className="text-xs text-muted-foreground">
        Each path matcher serves the hosts routed to it. Route rules, and path rules that redirect
        or carry a route action, are kept as they are: edit them in JSON.
      </p>
      {value.map((m, i) => (
        <div
          key={i}
          role="group"
          aria-label={`Path matcher ${i + 1}`}
          className="flex flex-col gap-3 rounded-md border p-3"
        >
          <div className="grid gap-3 sm:grid-cols-2">
            <Field id={`${id}-${i}-name`} label="Path matcher name">
              <Input
                id={`${id}-${i}-name`}
                className="font-mono"
                spellCheck={false}
                value={m.name}
                onChange={(e) => set(i, { name: e.target.value.trim() })}
              />
            </Field>
            <Field
              id={`${id}-${i}-hosts`}
              label="Hosts"
              hint="Comma-separated; *.example.com and * match many."
            >
              <Input
                className="font-mono"
                spellCheck={false}
                {...fieldAria(`${id}-${i}-hosts`, undefined, 'hint')}
                value={m.hosts}
                onChange={(e) => set(i, { hosts: e.target.value })}
              />
            </Field>
          </div>
          <Field id={`${id}-${i}-default`} label="Default backend of the matcher">
            <NativeSelect
              id={`${id}-${i}-default`}
              value={m.defaultService}
              onChange={(e) => set(i, { defaultService: e.target.value })}
            >
              <option value="">None</option>
              {options.map((o) => (
                <option key={o.value} value={o.value}>
                  {o.label}
                </option>
              ))}
            </NativeSelect>
          </Field>
          <Field
            id={`${id}-${i}-rules`}
            label="Path rules"
            hint="paths = backend, one per line, e.g. /api/*, /v1/* = global/backendServices/api. The longest matching path wins."
          >
            <Textarea
              rows={3}
              spellCheck={false}
              className="font-mono"
              {...fieldAria(`${id}-${i}-rules`, undefined, 'hint')}
              value={m.pathRules}
              onChange={(e) => set(i, { pathRules: e.target.value })}
            />
          </Field>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="w-fit"
            onClick={() => onChange(value.filter((_, j) => j !== i))}
          >
            <Trash2 aria-hidden />
            Remove path matcher {m.name}
          </Button>
        </div>
      ))}
      <Button
        type="button"
        variant="outline"
        size="sm"
        className="w-fit"
        onClick={() =>
          onChange([
            ...value,
            { name: `matcher-${value.length + 1}`, hosts: '', defaultService: '', pathRules: '' },
          ])
        }
      >
        <Plus aria-hidden />
        Add path matcher
      </Button>
      {error && (
        <p id={`${id}-error`} role="alert" className="text-sm text-destructive">
          {error}
        </p>
      )}
    </fieldset>
  )
}

/** SpecFields are a kind's form fields; edit leaves out the create-only ones. */
function SpecFields({
  spec,
  form,
  edit,
  project,
  region,
}: {
  spec: KindSpec
  form: UseFormReturn<Values>
  edit: boolean
  project: string
  region?: string
}) {
  const values = useWatch({ control: form.control }) as Values
  const scope = region ?? text(values.region)
  return (
    <>
      {spec.fields
        .filter((f) => !(edit && f.createOnly) && (!f.show || f.show(values)))
        .map((f) => (
          <SpecField
            key={f.key}
            f={f}
            form={form}
            values={values}
            prefix={edit ? 'edit' : 'create'}
            project={project}
            region={scope.trim()}
          />
        ))}
    </>
  )
}

const keysOf = (spec: KindSpec) => spec.fields.map((f) => f.key)

export function CreateResource() {
  const { coll: param = '' } = useParams()
  const k = kindInfo(param)
  if (!k) {
    return (
      <Card role="status">
        <p className="text-sm text-muted-foreground">No resource type {param}.</p>
      </Card>
    )
  }
  return <CreateForm key={k.coll} k={k} />
}

function CreateForm({ k }: { k: KindInfo }) {
  const [view] = useViewState()
  const project = view.project ?? ''
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const spec = SPECS[k.coll]
  const loc = k.regional && view.location && view.location !== 'global' ? view.location : ''
  const defaults = initialValues(spec, loc)
  const form = useForm<Values>({ resolver: resolver(spec, false), defaultValues: defaults })
  const region = text(useWatch({ control: form.control, name: 'region' })).trim()
  const create = useMutation({
    meta: { toast: false },
    mutationFn: ({ body, region }: { body: Body; region: string }) =>
      insertResource(project, region, k.coll, body),
    onSuccess: (_op, { body, region }) => {
      void qc.invalidateQueries({ queryKey: ['lb'] })
      const name = String(body.name)
      toast.success(`Created ${k.singular} ${name}.`)
      navigate(lbPath({ project, region, coll: k.coll, name }))
    },
  })
  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-2xl font-semibold">Create {k.singular}</h1>
        <p className="text-sm text-muted-foreground">
          In Project <span className="font-mono">{project}</span>.
        </p>
      </div>
      <Card>
        <ResourceEditor
          form={form}
          initialBody={createBody(spec)(defaults, {})}
          toBody={createBody(spec)}
          fromBody={(body, prev) => readValues(spec, body, prev)}
          fields={keysOf(spec)}
          onSubmit={(body, values) =>
            create.mutateAsync({ body, region: text(values.region).trim() })
          }
          submitLabel="Create"
          onCancel={() => navigate(listPath(k.coll))}
          jsonHint={
            <>
              Sent to <span className="font-mono">{apiMethod(k, region, 'insert')}</span>
              {k.regional && ' (the Region field picks the method)'}.
            </>
          }
        >
          <SpecFields spec={spec} form={form} edit={false} project={project} />
        </ResourceEditor>
      </Card>
    </div>
  )
}

export function EditResource() {
  const ref = useLbRef()
  const k = kindInfo(ref.coll)
  const query = useQuery(resourceQuery(ref))
  return (
    <div className="flex flex-col gap-6">
      <h1 className="text-2xl font-semibold">
        Edit {k?.singular ?? ref.coll} <span className="font-mono">{ref.name}</span>
      </h1>
      <QueryStatus query={query} />
      {k && !k.editable && (
        <Card role="status">
          <p className="text-sm text-muted-foreground">
            {k.title} cannot be changed: create a new one and point the proxies at it.
          </p>
        </Card>
      )}
      {k?.editable && query.data && <EditForm k={k} r={ref} cur={query.data} />}
    </div>
  )
}

function EditForm({ k, r, cur }: { k: KindInfo; r: Ref; cur: Resource }) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const spec = SPECS[k.coll]
  const form = useForm<Values>({
    resolver: resolver(spec, true),
    defaultValues: readValues(spec, cur, { region: r.region }),
  })
  const back = () => navigate(lbPath(r))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => {
      if (Object.keys(body).filter((key) => key !== 'fingerprint').length === 0) return false
      await patchResource(r, body)
      return true
    },
    onSuccess: (changed) => {
      void qc.invalidateQueries({ queryKey: ['lb'] })
      toast.success(changed ? `Saved ${k.singular} ${r.name}.` : 'Nothing to change.')
      back()
    },
  })
  return (
    <Card>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={patchBody(spec, cur)}
        fromBody={patchValues(spec, cur)}
        fields={keysOf(spec)}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">{apiMethod(k, r.region, 'patch')}</span>: objects
            merge, lists replace and null removes a field.
          </>
        }
      >
        <p className="text-sm text-muted-foreground">
          {r.region ? (
            <>
              In region <span className="font-mono">{r.region}</span>.
            </>
          ) : (
            'Global.'
          )}
        </p>
        <SpecFields spec={spec} form={form} edit project={r.project} region={r.region} />
      </ResourceEditor>
    </Card>
  )
}
