import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm, type UseFormReturn } from 'react-hook-form'
import { z } from 'zod'

import { useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Field, fieldAria } from '@/components/resource/Field'
import { ResourceEditor, type Body } from '@/components/resource/ResourceEditor'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Textarea } from '@/components/ui/textarea'
import { toast } from '@/lib/toast'

import {
  accountIdMessage,
  createServiceAccount,
  patchServiceAccount,
  serviceAccountQuery,
  validAccountId,
  type ServiceAccount,
} from './api'
import { accountPath, ChooseProject, useAccount, useProject } from './IamLayout'

// Create and edit forms for service accounts (FR-IAM-001, FR-UI-011):
// create sends serviceAccounts.create's request; edit sends
// serviceAccounts.patch with the fields that changed and an updateMask
// naming them.

type Obj = Record<string, unknown>
const obj = (v: unknown): Obj =>
  typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Obj) : {}
const str = (v: unknown) => (typeof v === 'string' ? v : '')

const fields = {
  displayName: z.string().max(100, 'Display name must be at most 100 characters.'),
  description: z.string().max(256, 'Description must be at most 256 characters.'),
}

const createSchema = z.object({
  accountId: z.string().superRefine((v, ctx) => {
    if (!validAccountId(v)) ctx.addIssue({ code: 'custom', message: accountIdMessage(v) })
  }),
  ...fields,
})
export type CreateValues = z.infer<typeof createSchema>

const editSchema = z.object(fields)
export type EditValues = z.infer<typeof editSchema>

/** createValues reads the create form's values out of a create request. */
export function createValues(body: Body): CreateValues {
  const sa = obj(body.serviceAccount)
  return {
    accountId: str(body.accountId),
    displayName: str(sa.displayName),
    description: str(sa.description),
  }
}

/** createBody writes the create form's values onto a create request. */
export function createBody(v: CreateValues, base: Body): Body {
  const sa: Obj = { ...obj(base.serviceAccount) }
  for (const k of ['displayName', 'description'] as const) {
    if (v[k]) sa[k] = v[k]
    else delete sa[k]
  }
  return { ...base, accountId: v.accountId, serviceAccount: sa }
}

/**
 * patchBody writes what the edit form changed onto a patch request, with
 * the updateMask naming those fields.
 */
export function patchBody(sa: ServiceAccount, v: EditValues, base: Body): Body {
  const out: Obj = { ...obj(base.serviceAccount) }
  const mask: string[] = []
  for (const k of ['displayName', 'description'] as const) {
    if (v[k] !== (sa[k] ?? '')) {
      out[k] = v[k]
      mask.push(k)
    } else {
      delete out[k]
    }
  }
  return { ...base, serviceAccount: out, updateMask: mask.join(',') }
}

/** patchValues reads a patch request back into the edit form, defaulting to sa. */
export const patchValues =
  (sa: ServiceAccount) =>
  (body: Body): EditValues => {
    const v = obj(body.serviceAccount)
    return {
      displayName: 'displayName' in v ? str(v.displayName) : (sa.displayName ?? ''),
      description: 'description' in v ? str(v.description) : (sa.description ?? ''),
    }
  }

function AccountFields({
  form,
  prefix,
}: {
  form: UseFormReturn<EditValues> | UseFormReturn<CreateValues>
  prefix: string
}) {
  const f = form as UseFormReturn<EditValues>
  const errors = f.formState.errors
  return (
    <>
      <Field id={`${prefix}-display`} label="Display name" error={errors.displayName?.message}>
        <Input
          autoComplete="off"
          {...fieldAria(`${prefix}-display`, errors.displayName?.message)}
          {...f.register('displayName')}
        />
      </Field>
      <Field id={`${prefix}-description`} label="Description" error={errors.description?.message}>
        <Textarea
          rows={2}
          {...fieldAria(`${prefix}-description`, errors.description?.message)}
          {...f.register('description')}
        />
      </Field>
    </>
  )
}

// ---- create ----

export function CreateServiceAccount() {
  const project = useProject()
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<CreateValues>({
    resolver: zodResolver(createSchema),
    defaultValues: { accountId: '', displayName: '', description: '' },
  })
  const errors = form.formState.errors
  const create = useMutation({
    meta: { toast: false },
    mutationFn: (body: Body) => createServiceAccount(project, body),
    onSuccess: (sa) => {
      void qc.invalidateQueries({ queryKey: ['iam'] })
      toast.success(`Created service account ${sa.email}.`)
      navigate(accountPath(sa.email))
    },
  })
  if (!project) {
    return <ChooseProject title="Create service account" what="create a service account in it" />
  }
  return (
    <Card>
      <h1 className="text-lg font-semibold">Create service account</h1>
      <ResourceEditor
        form={form}
        initialBody={createBody(form.getValues(), {})}
        toBody={createBody}
        fromBody={createValues}
        fields={['accountId']}
        onSubmit={(body) => create.mutateAsync(body)}
        submitLabel="Create"
        onCancel={() => navigate('/iam/serviceAccounts')}
        jsonHint={
          <>
            Sent to <span className="font-mono">serviceAccounts.create</span> in{' '}
            <span className="font-mono">projects/{project}</span>.
          </>
        }
      >
        <Field
          id="sa-id"
          label="Service account ID"
          error={errors.accountId?.message}
          hint={`The email is ID@${project}.iam.gserviceaccount.com. Cannot be changed.`}
        >
          <Input
            autoComplete="off"
            spellCheck={false}
            className="font-mono"
            {...fieldAria('sa-id', errors.accountId?.message, 'The')}
            {...form.register('accountId')}
          />
        </Field>
        <AccountFields form={form} prefix="sa" />
      </ResourceEditor>
    </Card>
  )
}

// ---- edit ----

export function EditServiceAccount() {
  const { project, email } = useAccount()
  const query = useQuery({ ...serviceAccountQuery(project, email), enabled: !!project })
  if (!project)
    return <ChooseProject title="Edit service account" what="edit its service accounts" />
  if (!query.data) return <QueryStatus query={query} />
  return <EditForm sa={query.data} project={project} />
}

function EditForm({ sa, project }: { sa: ServiceAccount; project: string }) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<EditValues>({
    resolver: zodResolver(editSchema),
    defaultValues: { displayName: sa.displayName ?? '', description: sa.description ?? '' },
  })
  const back = () => navigate(accountPath(sa.email))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => {
      if (!str(body.updateMask)) return false
      await patchServiceAccount(project, sa.email, body)
      return true
    },
    onSuccess: (changed) => {
      void qc.invalidateQueries({ queryKey: ['iam'] })
      toast.success(changed ? `Updated service account ${sa.email}.` : 'Nothing to change.')
      back()
    },
  })
  return (
    <Card>
      <h2 className="text-lg font-semibold">
        Edit service account <span className="font-mono">{sa.email}</span>
      </h2>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={(v, base) => patchBody(sa, v, base)}
        fromBody={patchValues(sa)}
        fields={['displayName', 'description']}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">serviceAccounts.patch</span>: updateMask names the
            fields of serviceAccount to change.
          </>
        }
      >
        <AccountFields form={form} prefix="edit-sa" />
      </ResourceEditor>
    </Card>
  )
}
