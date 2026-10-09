import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm, useWatch, type UseFormReturn } from 'react-hook-form'
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
  createRepository,
  lastSegment,
  locationName,
  locationsQuery,
  patchRepository,
  REPO_ID,
  repoIdMessage,
  repositoryQuery,
  type RepoRef,
  type Repository,
} from './api'
import { ChooseProject, repoPath, useRepoRef } from './ArLayout'

// Create and edit forms for repositories (FR-AR-001, FR-UI-011): create
// sends repositories.create's Repository (format DOCKER) and waits for its
// Operation; edit sends repositories.patch with the fields that changed
// and an updateMask naming them. Cleanup policies and the rest are in the
// JSON editor.

type Obj = Record<string, unknown>
const obj = (v: unknown): Obj =>
  typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Obj) : {}
const str = (v: unknown) => (typeof v === 'string' ? v : '')
const lines = (text: string) =>
  text
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter(Boolean)

/** UPSTREAM is one virtual repository member: REPOSITORY_ID=PRIORITY. */
const UPSTREAM = /^([a-z]([a-z0-9-]*[a-z0-9])?)\s*=\s*(\d+)$/

const fieldsSchema = {
  description: z.string(),
  labels: z.string().superRefine((v, ctx) => {
    const err = labelsError(v)
    if (err) ctx.addIssue({ code: 'custom', message: err })
  }),
  immutableTags: z.boolean(),
  upstreams: z.string().superRefine((v, ctx) => {
    const bad = lines(v).find((e) => !UPSTREAM.test(e))
    if (bad)
      ctx.addIssue({
        code: 'custom',
        message: `Invalid upstream "${bad}": use repository-id=priority.`,
      })
  }),
}

const createSchema = z
  .object({
    id: z.string().superRefine((v, ctx) => {
      if (!REPO_ID.test(v) || v.length > 63)
        ctx.addIssue({ code: 'custom', message: repoIdMessage(v) })
    }),
    location: z.string().min(1, 'Choose a location.'),
    mode: z.enum(['STANDARD_REPOSITORY', 'REMOTE_REPOSITORY', 'VIRTUAL_REPOSITORY']),
    upstream: z.enum(['DOCKER_HUB', 'custom']),
    customUri: z.string().trim(),
    ...fieldsSchema,
  })
  .superRefine((v, ctx) => {
    if (v.mode === 'REMOTE_REPOSITORY' && v.upstream === 'custom' && !v.customUri) {
      ctx.addIssue({
        code: 'custom',
        path: ['customUri'],
        message:
          'remote_repository_config.docker_repository must set public_repository or custom_repository.',
      })
    }
  })
export type CreateValues = z.infer<typeof createSchema>

const editSchema = z.object(fieldsSchema)
export type EditValues = z.infer<typeof editSchema>

/** upstreamText reads a virtual repository's members as repository-id=priority lines. */
function upstreamText(body: Body): string {
  const vc = obj(body.virtualRepositoryConfig)
  const policies = Array.isArray(vc.upstreamPolicies) ? vc.upstreamPolicies : []
  return policies
    .map((p) => `${lastSegment(str(obj(p).repository))}=${Number(obj(p).priority ?? 0)}`)
    .join('\n')
}

/** upstreamPolicies writes repository-id=priority lines as upstream policies. */
function upstreamPolicies(text: string, project: string, location: string) {
  return lines(text).map((e) => {
    const [, id = '', , priority = '0'] = UPSTREAM.exec(e) ?? []
    return {
      id,
      repository: `${locationName(project, location)}/repositories/${id}`,
      priority: Number(priority),
    }
  })
}

function fieldValues(body: Body): EditValues {
  return {
    description: str(body.description),
    labels: formatLabels(obj(body.labels) as Record<string, string>),
    immutableTags: obj(body.dockerConfig).immutableTags === true,
    upstreams: upstreamText(body),
  }
}

/** repositoryValues reads the create form's values out of a Repository body. */
export function repositoryValues(body: Body, prev: CreateValues): CreateValues {
  const dr = obj(obj(body.remoteRepositoryConfig).dockerRepository)
  const custom = str(obj(dr.customRepository).uri)
  return {
    ...prev,
    mode: (str(body.mode) || 'STANDARD_REPOSITORY') as CreateValues['mode'],
    upstream: custom ? 'custom' : 'DOCKER_HUB',
    customUri: custom,
    ...fieldValues(body),
  }
}

/** repositoryBody writes the create form's values onto a repositories.create body. */
export function repositoryBody(project: string) {
  return (v: CreateValues, base: Body): Body => {
    const out: Body = { ...base, format: 'DOCKER', mode: v.mode }
    const set = (k: string, value: unknown, keep: boolean) => {
      if (keep) out[k] = value
      else delete out[k]
    }
    set('description', v.description, !!v.description)
    const labels = parseLabels(v.labels)
    set('labels', labels, Object.keys(labels).length > 0)
    const dc = { ...obj(base.dockerConfig) }
    if (v.immutableTags) dc.immutableTags = true
    else delete dc.immutableTags
    set('dockerConfig', dc, Object.keys(dc).length > 0)
    const rc = obj(base.remoteRepositoryConfig)
    set(
      'remoteRepositoryConfig',
      {
        ...rc,
        dockerRepository:
          v.upstream === 'custom'
            ? { customRepository: { uri: v.customUri } }
            : { publicRepository: 'DOCKER_HUB' },
      },
      v.mode === 'REMOTE_REPOSITORY',
    )
    set(
      'virtualRepositoryConfig',
      { upstreamPolicies: upstreamPolicies(v.upstreams, project, v.location) },
      v.mode === 'VIRTUAL_REPOSITORY',
    )
    return out
  }
}

/** editValues are the edit form's values for r. */
export const editValues = (r: Repository) => fieldValues(r as unknown as Body)

/**
 * patchBody writes what the edit form changed onto a repositories.patch
 * body; updateMask then names exactly those fields.
 */
export function patchBody(r: Repository, ref: RepoRef, v: EditValues, base: Body): Body {
  const was = editValues(r)
  const out: Body = { ...base }
  const set = (k: string, changed: boolean, value: unknown) => {
    if (changed) out[k] = value
    else delete out[k]
  }
  set('description', v.description !== was.description, v.description)
  set('labels', v.labels !== was.labels, parseLabels(v.labels))
  set('dockerConfig', v.immutableTags !== was.immutableTags, {
    ...r.dockerConfig,
    immutableTags: v.immutableTags,
  })
  set('virtualRepositoryConfig', v.upstreams !== was.upstreams, {
    upstreamPolicies: upstreamPolicies(v.upstreams, ref.project, ref.location),
  })
  return out
}

/** patchValues reads an edit body back into the form, defaulting to r. */
export function patchValues(r: Repository) {
  return (body: Body): EditValues => {
    const was = editValues(r)
    const now = fieldValues(body)
    const has = (k: string) => k in body
    return {
      description: has('description') ? now.description : was.description,
      labels: has('labels') ? now.labels : was.labels,
      immutableTags: has('dockerConfig') ? now.immutableTags : was.immutableTags,
      upstreams: has('virtualRepositoryConfig') ? now.upstreams : was.upstreams,
    }
  }
}

/** updateMask names the body's fields in the API's snake_case. */
export const updateMask = (body: Body) =>
  Object.keys(body).map((k) => k.replace(/[A-Z]/g, (c) => `_${c.toLowerCase()}`))

/** RepositoryFields are the fields create and edit share. */
function RepositoryFields({
  form,
  prefix,
  virtual,
}: {
  form: UseFormReturn<EditValues> | UseFormReturn<CreateValues>
  prefix: string
  virtual: boolean
}) {
  const f = form as UseFormReturn<EditValues>
  const errors = f.formState.errors
  return (
    <>
      <Field id={`${prefix}-description`} label="Description">
        <Input {...fieldAria(`${prefix}-description`)} {...f.register('description')} />
      </Field>
      <Field
        id={`${prefix}-labels`}
        label="Labels"
        error={errors.labels?.message}
        hint="key=value, one per line"
      >
        <Textarea
          rows={2}
          spellCheck={false}
          className="font-mono"
          {...fieldAria(`${prefix}-labels`, errors.labels?.message, 'key')}
          {...f.register('labels')}
        />
      </Field>
      {virtual && (
        <Field
          id={`${prefix}-upstreams`}
          label="Upstream repositories"
          error={errors.upstreams?.message}
          hint="repository-id=priority, one per line: repositories in the same location, the highest priority pulled from first."
        >
          <Textarea
            rows={3}
            spellCheck={false}
            className="font-mono"
            {...fieldAria(`${prefix}-upstreams`, errors.upstreams?.message, 'repository')}
            {...f.register('upstreams')}
          />
        </Field>
      )}
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" className="size-4" {...f.register('immutableTags')} />
        Immutable tags (a tag cannot be moved to another digest)
      </label>
    </>
  )
}

// ---- create ----

const createDefaults: CreateValues = {
  id: '',
  location: '',
  mode: 'STANDARD_REPOSITORY',
  upstream: 'DOCKER_HUB',
  customUri: '',
  description: '',
  labels: '',
  immutableTags: false,
  upstreams: '',
}

export function CreateRepository() {
  const [view] = useViewState()
  const project = view.project ?? ''
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const locations = useQuery({ ...locationsQuery(project), enabled: !!project })
  const form = useForm<CreateValues>({
    resolver: zodResolver(createSchema),
    defaultValues: { ...createDefaults, location: view.location ?? 'us-central1' },
  })
  const errors = form.formState.errors
  const location = useWatch({ control: form.control, name: 'location' })
  const mode = useWatch({ control: form.control, name: 'mode' })
  const upstream = useWatch({ control: form.control, name: 'upstream' })
  const create = useMutation({
    meta: { toast: false },
    mutationFn: ({ body, values }: { body: Body; values: CreateValues }) =>
      createRepository(project, values.location, values.id, body),
    onSuccess: (r) => {
      void qc.invalidateQueries({ queryKey: ['ar'] })
      const id = lastSegment(r.name)
      toast.success(`Created repository ${id}.`)
      navigate(repoPath({ location: r.name.split('/')[3] ?? '', repo: id }))
    },
  })
  if (!project) return <ChooseProject what="create a repository in it" />
  const toBody = repositoryBody(project)
  return (
    <Card>
      <h1 className="text-lg font-semibold">Create repository</h1>
      <ResourceEditor
        form={form}
        initialBody={toBody(form.getValues(), {})}
        toBody={toBody}
        fromBody={repositoryValues}
        fields={['id', 'location', 'customUri', 'labels', 'upstreams']}
        onSubmit={(body, values) => create.mutateAsync({ body, values })}
        submitLabel="Create"
        onCancel={() => navigate('/ar')}
        jsonHint={
          <>
            Sent to <span className="font-mono">repositories.create</span> in{' '}
            <span className="font-mono">{locationName(project, location)}</span> with the ID of the
            form.
          </>
        }
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id="repo-id" label="Name" error={errors.id?.message} hint="Cannot be changed.">
            <Input
              autoComplete="off"
              spellCheck={false}
              {...fieldAria('repo-id', errors.id?.message, 'Cannot')}
              {...form.register('id')}
            />
          </Field>
          <Field
            id="repo-location"
            label="Location"
            error={errors.location?.message}
            hint="Images are pushed to LOCATION-docker.pkg.dev."
          >
            <NativeSelect
              {...fieldAria('repo-location', errors.location?.message, 'Images')}
              {...form.register('location')}
            >
              {location && !locations.data?.some((l) => l.locationId === location) && (
                <option value={location}>{location}</option>
              )}
              {(locations.data ?? []).map((l) => (
                <option key={l.locationId} value={l.locationId}>
                  {l.locationId}
                  {l.displayName ? ` (${l.displayName})` : ''}
                </option>
              ))}
            </NativeSelect>
          </Field>
        </div>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id="repo-format" label="Format" hint="The emulator serves Docker repositories.">
            <NativeSelect disabled {...fieldAria('repo-format', undefined, 'The')}>
              <option>Docker</option>
            </NativeSelect>
          </Field>
          <Field id="repo-mode" label="Mode">
            <NativeSelect {...fieldAria('repo-mode')} {...form.register('mode')}>
              <option value="STANDARD_REPOSITORY">Standard</option>
              <option value="REMOTE_REPOSITORY">Remote (pull-through cache)</option>
              <option value="VIRTUAL_REPOSITORY">Virtual</option>
            </NativeSelect>
          </Field>
        </div>
        {mode === 'REMOTE_REPOSITORY' && (
          <div className="grid gap-4 sm:grid-cols-2">
            <Field id="repo-upstream" label="Remote source" hint="Cannot be changed.">
              <NativeSelect
                {...fieldAria('repo-upstream', undefined, 'Cannot')}
                {...form.register('upstream')}
              >
                <option value="DOCKER_HUB">Docker Hub</option>
                <option value="custom">Custom registry</option>
              </NativeSelect>
            </Field>
            {upstream === 'custom' && (
              <Field
                id="repo-custom-uri"
                label="Registry URL"
                error={errors.customUri?.message}
                hint="e.g. https://registry.example.com"
              >
                <Input
                  spellCheck={false}
                  {...fieldAria('repo-custom-uri', errors.customUri?.message, 'e.g.')}
                  {...form.register('customUri')}
                />
              </Field>
            )}
          </div>
        )}
        <RepositoryFields form={form} prefix="repo" virtual={mode === 'VIRTUAL_REPOSITORY'} />
      </ResourceEditor>
    </Card>
  )
}

// ---- edit ----

export function EditRepository() {
  const ref = useRepoRef()
  const query = useQuery({ ...repositoryQuery(ref), enabled: !!ref.project })
  if (!ref.project) return <ChooseProject what="edit its repositories" />
  if (!query.data) return <QueryStatus query={query} />
  return <EditRepositoryForm r={query.data} repoRef={ref} />
}

function EditRepositoryForm({ r, repoRef }: { r: Repository; repoRef: RepoRef }) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<EditValues>({
    resolver: zodResolver(editSchema),
    defaultValues: editValues(r),
  })
  const back = () => navigate(repoPath(repoRef, 'overview'))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => {
      const mask = updateMask(body)
      if (mask.length === 0) return false
      await patchRepository(repoRef, body, mask)
      return true
    },
    onSuccess: (changed) => {
      void qc.invalidateQueries({ queryKey: ['ar'] })
      toast.success(changed ? `Updated repository ${repoRef.repo}.` : 'Nothing to change.')
      back()
    },
  })
  return (
    <Card>
      <h2 className="text-lg font-semibold">
        Edit repository <span className="font-mono">{repoRef.repo}</span>{' '}
        <span className="text-sm font-normal text-muted-foreground">({repoRef.location})</span>
      </h2>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={(v, base) => patchBody(r, repoRef, v, base)}
        fromBody={patchValues(r)}
        fields={['labels', 'upstreams']}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">repositories.patch</span> with an updateMask of the
            body&apos;s fields: a field in the mask but not the body is cleared.
          </>
        }
      >
        <RepositoryFields
          form={form}
          prefix="edit-repo"
          virtual={r.mode === 'VIRTUAL_REPOSITORY'}
        />
      </ResourceEditor>
    </Card>
  )
}
