import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useId } from 'react'
import { useForm, type UseFormReturn } from 'react-hook-form'
import { useParams } from 'react-router'
import { z } from 'zod'

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
  createRole,
  customRoleQuery,
  patchRole,
  PERMISSION,
  predefinedRolesQuery,
  ROLE_ID,
  roleIdMessage,
  type Role,
} from './api'
import { ChooseProject, rolePath, useProject } from './IamLayout'

// Create and edit forms for custom roles (FR-IAM-002, FR-UI-011): create
// sends roles.create's request; edit sends roles.patch with the fields
// that changed, an updateMask naming them and the etag read.

type Obj = Record<string, unknown>
const obj = (v: unknown): Obj =>
  typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Obj) : {}
const str = (v: unknown) => (typeof v === 'string' ? v : '')
const lines = (text: string) =>
  text
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter(Boolean)

export const STAGES = ['ALPHA', 'BETA', 'GA', 'DEPRECATED', 'DISABLED', 'EAP'] as const

const fields = {
  title: z.string().max(100, 'Role title must be at most 100 characters.'),
  description: z.string().max(300, 'Role description must be at most 300 characters.'),
  stage: z.enum(STAGES),
  permissions: z.string().superRefine((v, ctx) => {
    const bad = lines(v).find((p) => !PERMISSION.test(p))
    if (bad) ctx.addIssue({ code: 'custom', message: `Permission ${bad} is not valid.` })
  }),
}

const createSchema = z.object({
  roleId: z.string().superRefine((v, ctx) => {
    if (!ROLE_ID.test(v)) ctx.addIssue({ code: 'custom', message: roleIdMessage(v) })
  }),
  ...fields,
})
export type CreateValues = z.infer<typeof createSchema>

const editSchema = z.object(fields)
export type EditValues = z.infer<typeof editSchema>

const stageOf = (v: unknown): EditValues['stage'] =>
  (STAGES as readonly string[]).includes(str(v)) ? (str(v) as EditValues['stage']) : 'ALPHA'

/** roleFields reads the shared fields out of a Role. */
function roleFields(role: Obj): EditValues {
  return {
    title: str(role.title),
    description: str(role.description),
    stage: stageOf(role.stage),
    permissions: (Array.isArray(role.includedPermissions) ? role.includedPermissions : [])
      .map(str)
      .join('\n'),
  }
}

/** writeRole writes the shared fields onto a Role (unset text fields are dropped). */
function writeRole(v: EditValues, base: Obj): Obj {
  const out: Obj = { ...base, stage: v.stage, includedPermissions: lines(v.permissions) }
  for (const k of ['title', 'description'] as const) {
    if (v[k]) out[k] = v[k]
    else delete out[k]
  }
  return out
}

/** createValues reads the create form's values out of a create request. */
export const createValues = (body: Body): CreateValues => ({
  roleId: str(body.roleId),
  ...roleFields(obj(body.role)),
})

/** createBody writes the create form's values onto a create request. */
export const createBody = (v: CreateValues, base: Body): Body => ({
  ...base,
  roleId: v.roleId,
  role: writeRole(v, obj(base.role)),
})

/** editValues are the edit form's values for role. */
export const editValues = (role: Role) => roleFields(role as unknown as Obj)

/** roleMask names a patch body's fields, all but its etag. */
export const roleMask = (body: Body) => Object.keys(body).filter((k) => k !== 'etag')

/** patchBody writes what the edit form changed onto a roles.patch body, with the etag. */
export function patchBody(role: Role, v: EditValues, base: Body): Body {
  const was = editValues(role)
  const now = writeRole(v, {})
  const out: Body = { ...base, etag: role.etag }
  const set = (k: string, changed: boolean, value: unknown) => {
    if (changed) out[k] = value
    else delete out[k]
  }
  set('title', v.title !== was.title, now.title ?? '')
  set('description', v.description !== was.description, now.description ?? '')
  set('stage', v.stage !== was.stage, now.stage)
  set('includedPermissions', v.permissions !== was.permissions, now.includedPermissions)
  return out
}

/** patchValues reads a patch body back into the edit form, defaulting to role. */
export const patchValues =
  (role: Role) =>
  (body: Body): EditValues => {
    const was = editValues(role)
    const now = roleFields(body)
    const has = (k: string) => k in body
    return {
      title: has('title') ? now.title : was.title,
      description: has('description') ? now.description : was.description,
      stage: has('stage') ? now.stage : was.stage,
      permissions: has('includedPermissions') ? now.permissions : was.permissions,
    }
  }

function RoleFields({
  form,
  prefix,
}: {
  form: UseFormReturn<EditValues> | UseFormReturn<CreateValues>
  prefix: string
}) {
  const f = form as UseFormReturn<EditValues>
  const errors = f.formState.errors
  const id = useId()
  const roles = useQuery(predefinedRolesQuery())
  const copy = (name: string) => {
    const r = roles.data?.find((x) => x.name === name)
    if (!r) return
    const have = lines(f.getValues('permissions'))
    const add = (r.includedPermissions ?? []).filter((p) => !have.includes(p))
    f.setValue('permissions', [...have, ...add].join('\n'), { shouldDirty: true })
  }
  return (
    <>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id={`${prefix}-title`} label="Title" error={errors.title?.message}>
          <Input
            autoComplete="off"
            {...fieldAria(`${prefix}-title`, errors.title?.message)}
            {...f.register('title')}
          />
        </Field>
        <Field id={`${prefix}-stage`} label="Launch stage">
          <NativeSelect {...fieldAria(`${prefix}-stage`)} {...f.register('stage')}>
            {STAGES.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </NativeSelect>
        </Field>
      </div>
      <Field id={`${prefix}-description`} label="Description" error={errors.description?.message}>
        <Textarea
          rows={2}
          {...fieldAria(`${prefix}-description`, errors.description?.message)}
          {...f.register('description')}
        />
      </Field>
      <Field
        id={`${prefix}-permissions`}
        label="Permissions"
        error={errors.permissions?.message}
        hint="SERVICE.RESOURCE.VERB, one per line, e.g. storage.objects.get"
      >
        <Textarea
          rows={8}
          spellCheck={false}
          className="font-mono text-xs"
          {...fieldAria(`${prefix}-permissions`, errors.permissions?.message, 'SERVICE')}
          {...f.register('permissions')}
        />
      </Field>
      <Field
        id={`${id}-copy`}
        label="Copy a predefined role"
        hint="Its permissions are appended to the list above."
      >
        <NativeSelect
          {...fieldAria(`${id}-copy`, undefined, 'Its')}
          value=""
          onChange={(e) => copy(e.target.value)}
        >
          <option value="">Choose a role…</option>
          {(roles.data ?? []).map((r) => (
            <option key={r.name} value={r.name}>
              {r.title ? `${r.title} (${r.name})` : r.name}
            </option>
          ))}
        </NativeSelect>
      </Field>
    </>
  )
}

// ---- create ----

const createDefaults: CreateValues = {
  roleId: '',
  title: '',
  description: '',
  stage: 'GA',
  permissions: '',
}

export function CreateRole() {
  const project = useProject()
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<CreateValues>({
    resolver: zodResolver(createSchema),
    defaultValues: createDefaults,
  })
  const errors = form.formState.errors
  const create = useMutation({
    meta: { toast: false },
    mutationFn: (body: Body) => createRole(project, str(body.roleId), obj(body.role)),
    onSuccess: (r) => {
      void qc.invalidateQueries({ queryKey: ['iam'] })
      toast.success(`Created role ${r.name}.`)
      navigate(rolePath(r.name))
    },
  })
  if (!project) return <ChooseProject title="Create role" what="create a custom role in it" />
  return (
    <Card>
      <h1 className="text-lg font-semibold">Create role</h1>
      <ResourceEditor
        form={form}
        initialBody={createBody(createDefaults, {})}
        toBody={createBody}
        fromBody={createValues}
        fields={['roleId', 'permissions']}
        onSubmit={(body) => create.mutateAsync(body)}
        submitLabel="Create"
        onCancel={() => navigate('/iam/roles')}
        jsonHint={
          <>
            Sent to <span className="font-mono">roles.create</span> in{' '}
            <span className="font-mono">projects/{project}</span>.
          </>
        }
      >
        <Field
          id="role-id"
          label="Role ID"
          error={errors.roleId?.message}
          hint={`The role is projects/${project}/roles/ID. Cannot be changed.`}
        >
          <Input
            autoComplete="off"
            spellCheck={false}
            className="font-mono"
            {...fieldAria('role-id', errors.roleId?.message, 'The')}
            {...form.register('roleId')}
          />
        </Field>
        <RoleFields form={form} prefix="role" />
      </ResourceEditor>
    </Card>
  )
}

// ---- edit ----

export function EditRole() {
  const project = useProject()
  const { role = '' } = useParams()
  const query = useQuery({ ...customRoleQuery(project, role), enabled: !!project })
  if (!project) return <ChooseProject title="Edit role" what="edit its custom roles" />
  if (!query.data) return <QueryStatus query={query} />
  return <EditForm role={query.data} project={project} id={role} />
}

function EditForm({ role, project, id }: { role: Role; project: string; id: string }) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<EditValues>({
    resolver: zodResolver(editSchema),
    defaultValues: editValues(role),
  })
  const back = () => navigate(rolePath(role.name))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => {
      const mask = roleMask(body)
      if (mask.length === 0) return false
      await patchRole(project, id, body, mask)
      return true
    },
    onSuccess: (changed) => {
      void qc.invalidateQueries({ queryKey: ['iam'] })
      toast.success(changed ? `Updated role ${id}.` : 'Nothing to change.')
      back()
    },
  })
  return (
    <Card>
      <h2 className="text-lg font-semibold">
        Edit role <span className="font-mono">{role.name}</span>
      </h2>
      <ResourceEditor
        form={form}
        initialBody={{ etag: role.etag }}
        toBody={(v, base) => patchBody(role, v, base)}
        fromBody={patchValues(role)}
        fields={['permissions']}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">roles.patch</span> with an updateMask of the
            body&apos;s fields; the etag makes it fail if the role changed since it was read.
          </>
        }
      >
        <RoleFields form={form} prefix="edit-role" />
      </ResourceEditor>
    </Card>
  )
}
