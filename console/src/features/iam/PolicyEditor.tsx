import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { FlaskConical, Pencil, Plus, Trash2 } from 'lucide-react'
import { useId, useState } from 'react'
import { useForm } from 'react-hook-form'
import { z } from 'zod'

import { errorMessage } from '@/api/errors'
import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { JsonView } from '@/components/resource/DetailList'
import { Field, fieldAria } from '@/components/resource/Field'
import { parseBody, ResourceEditor, type Body } from '@/components/resource/ResourceEditor'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { Textarea } from '@/components/ui/textarea'
import { toast } from '@/lib/toast'

import {
  customRolesQuery,
  kindOf,
  MEMBER,
  policyQuery,
  predefinedRolesQuery,
  setPolicy,
  type Binding,
  type Policy,
  type ResourceRef,
} from './api'
import { rolePath, simulatorPath } from './IamLayout'

// Bindings of any resource's IAM policy (FR-IAM-003, FR-UI-011): every
// change is a read-modify-write of the whole policy through the
// resource's setIamPolicy, carrying the etag read, so a concurrent change
// fails with the API's ABORTED rather than being overwritten.

type Obj = Record<string, unknown>
const obj = (v: unknown): Obj =>
  typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Obj) : {}
const str = (v: unknown) => (typeof v === 'string' ? v : '')
const lines = (text: string) =>
  text
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter(Boolean)

const schema = z
  .object({
    role: z.string().trim().min(1, 'Role must be specified in a binding.'),
    members: z.string().superRefine((v, ctx) => {
      const all = lines(v)
      if (all.length === 0) {
        ctx.addIssue({ code: 'custom', message: 'Add at least one member.' })
        return
      }
      const bad = all.find((m) => !MEMBER.test(m))
      if (bad) ctx.addIssue({ code: 'custom', message: `Invalid member: ${bad}` })
    }),
    conditionTitle: z.string(),
    conditionDescription: z.string(),
    conditionExpression: z.string(),
  })
  .superRefine((v, ctx) => {
    const expr = v.conditionExpression.trim()
    if (expr && !v.conditionTitle.trim()) {
      ctx.addIssue({
        code: 'custom',
        path: ['conditionTitle'],
        message: 'Condition title must be specified.',
      })
    }
    if (!expr && (v.conditionTitle.trim() || v.conditionDescription.trim())) {
      ctx.addIssue({
        code: 'custom',
        path: ['conditionExpression'],
        message: 'Condition expression must not be empty.',
      })
    }
  })
export type BindingValues = z.infer<typeof schema>

/** bindingValues reads the form's values out of a Binding body. */
export function bindingValues(body: Body): BindingValues {
  const c = obj(body.condition)
  return {
    role: str(body.role),
    members: (Array.isArray(body.members) ? body.members : []).map(str).join('\n'),
    conditionTitle: str(c.title),
    conditionDescription: str(c.description),
    conditionExpression: str(c.expression),
  }
}

/** bindingBody writes the form's values onto a Binding body. */
export function bindingBody(v: BindingValues, base: Body): Body {
  const out: Body = { ...base, role: v.role.trim(), members: lines(v.members) }
  const expression = v.conditionExpression.trim()
  if (expression) {
    const c: Obj = { ...obj(base.condition), title: v.conditionTitle.trim(), expression }
    if (v.conditionDescription.trim()) c.description = v.conditionDescription.trim()
    else delete c.description
    out.condition = c
  } else {
    delete out.condition
  }
  return out
}

/** withBinding is policy with binding added (at undefined) or replacing bindings[at]. */
export function withBinding(policy: Policy, binding: Binding, at?: number): Policy {
  const bindings = [...(policy.bindings ?? [])]
  if (at === undefined) bindings.push(binding)
  else bindings[at] = binding
  return { ...policy, bindings }
}

/** withoutBinding is policy without bindings[at]. */
export function withoutBinding(policy: Policy, at: number): Policy {
  return { ...policy, bindings: (policy.bindings ?? []).filter((_, i) => i !== at) }
}

const bindingLabel = (b: Binding) =>
  `${b.role}${b.condition?.title ? ` (${b.condition.title})` : ''}`

/**
 * PolicyEditor shows a resource's IAM policy as its bindings, grants and
 * edits bindings in a form or as JSON, and edits the whole policy as JSON.
 */
export function PolicyEditor({ r }: { r: ResourceRef }) {
  const query = useQuery(policyQuery(r))
  const roles = useQuery(predefinedRolesQuery())
  const custom = useQuery({ ...customRolesQuery(r.project), enabled: !!r.project })
  const [editing, setEditing] = useState<'new' | 'json' | number>()
  const qc = useQueryClient()
  const saved = (p: Policy) => {
    qc.setQueryData(policyQuery(r).queryKey, p)
    void qc.invalidateQueries({ queryKey: ['iam', 'policy'] })
  }
  // Editors show a failure where it happened; removing toasts it.
  const save = useMutation({
    meta: { toast: false },
    mutationFn: (p: Policy) => setPolicy(r, p),
    onSuccess: saved,
    onError: () => void qc.invalidateQueries({ queryKey: policyQuery(r).queryKey }),
  })
  const titles = new Map<string, string | undefined>()
  for (const role of [...(roles.data ?? []), ...(custom.data ?? [])]) {
    titles.set(role.name, role.title)
  }
  const policy = query.data
  const bindings = policy?.bindings ?? []
  const kind = kindOf(r.kind)

  return (
    <div className="flex flex-col gap-6">
      <Card>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <CardTitle>Bindings</CardTitle>
          {policy && editing === undefined && (
            <div className="flex flex-wrap gap-2">
              <Button variant="outline" asChild>
                <Link to={simulatorPath('', r.kind, r.name)}>
                  <FlaskConical aria-hidden />
                  Test permissions
                </Link>
              </Button>
              <Button variant="outline" onClick={() => setEditing('json')}>
                <Pencil aria-hidden />
                Edit policy JSON
              </Button>
              <Button onClick={() => setEditing('new')}>
                <Plus aria-hidden />
                Grant access
              </Button>
            </div>
          )}
        </div>
        <QueryStatus query={query} />
        {policy && typeof editing === 'number' && bindings[editing] && (
          <BindingEditor
            key={editing}
            title={`Edit binding ${bindingLabel(bindings[editing])}`}
            binding={bindings[editing]}
            roleTitles={titles}
            onSave={async (b) => {
              const p = await save.mutateAsync(withBinding(policy, b, editing))
              toast.success(`Updated the binding of ${b.role}.`)
              setEditing(undefined)
              return p
            }}
            onCancel={() => setEditing(undefined)}
          />
        )}
        {policy && editing === 'new' && (
          <BindingEditor
            title="Grant access"
            roleTitles={titles}
            onSave={async (b) => {
              const p = await save.mutateAsync(withBinding(policy, b))
              toast.success(`Granted ${b.role} to ${(b.members ?? []).join(', ')}.`)
              setEditing(undefined)
              return p
            }}
            onCancel={() => setEditing(undefined)}
          />
        )}
        {policy && editing === 'json' && (
          <PolicyJson
            policy={policy}
            onSave={async (p) => {
              await save.mutateAsync(p)
              toast.success('Saved the policy.')
              setEditing(undefined)
            }}
            onCancel={() => setEditing(undefined)}
          />
        )}
        {policy && bindings.length === 0 && (
          <p className="text-sm text-muted-foreground">
            No bindings on this {kind.label.toLowerCase()}
            {r.kind === 'project'
              ? '. Requests without credentials, such as the console’s, act as the Default principal, which is owner of every Project.'
              : ': only those of its Project apply.'}
          </p>
        )}
        {bindings.length > 0 && (
          <Table aria-label="Bindings">
            <TableHeader>
              <TableRow>
                <TableHead>Role</TableHead>
                <TableHead>Members</TableHead>
                <TableHead>Condition</TableHead>
                <TableHead>
                  <span className="sr-only">Actions</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {bindings.map((b, i) => (
                <TableRow key={`${b.role}-${i}`} data-testid="binding">
                  <TableCell>
                    <Link
                      to={rolePath(b.role)}
                      className="font-mono text-primary underline-offset-4 hover:underline"
                    >
                      {b.role}
                    </Link>
                    {titles.get(b.role) && (
                      <div className="text-xs text-muted-foreground">{titles.get(b.role)}</div>
                    )}
                  </TableCell>
                  <TableCell>
                    <ul className="flex flex-col gap-0.5 font-mono text-xs">
                      {(b.members ?? []).map((m) => (
                        <li key={m} className="break-all">
                          {m}
                        </li>
                      ))}
                    </ul>
                  </TableCell>
                  <TableCell>
                    {b.condition && (
                      <>
                        <div className="text-sm">{b.condition.title}</div>
                        <div className="font-mono text-xs break-all text-muted-foreground">
                          {b.condition.expression}
                        </div>
                      </>
                    )}
                  </TableCell>
                  <TableCell>
                    <div className="flex flex-wrap justify-end gap-2">
                      <Button
                        size="sm"
                        variant="outline"
                        aria-label={`Edit binding ${bindingLabel(b)}`}
                        disabled={editing !== undefined}
                        onClick={() => setEditing(i)}
                      >
                        <Pencil aria-hidden />
                        Edit
                      </Button>
                      <ConfirmDialog
                        trigger={
                          <Button
                            size="sm"
                            variant="outline"
                            aria-label={`Remove binding ${bindingLabel(b)}`}
                            disabled={editing !== undefined}
                          >
                            <Trash2 aria-hidden />
                            Remove
                          </Button>
                        }
                        title={`Remove binding ${bindingLabel(b)}?`}
                        description={`Its ${(b.members ?? []).length} member(s) lose ${b.role} on this ${kind.label.toLowerCase()}.`}
                        confirmLabel="Remove binding"
                        onConfirm={async () => {
                          try {
                            await save.mutateAsync(withoutBinding(policy!, i))
                          } catch (e) {
                            toast.error(errorMessage(e))
                            throw e
                          }
                          toast.success(`Removed the binding of ${b.role}.`)
                        }}
                      />
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Card>
      {policy && (
        <Card>
          <CardTitle>Policy</CardTitle>
          <JsonView value={policy} label="Policy JSON" />
        </Card>
      )}
    </div>
  )
}

const defaults: BindingValues = {
  role: '',
  members: '',
  conditionTitle: '',
  conditionDescription: '',
  conditionExpression: '',
}

/** BindingEditor edits one binding in a form or as JSON. */
function BindingEditor({
  title,
  binding,
  roleTitles,
  onSave,
  onCancel,
}: {
  title: string
  binding?: Binding
  roleTitles: Map<string, string | undefined>
  onSave: (b: Binding) => Promise<unknown>
  onCancel: () => void
}) {
  const id = useId()
  const initial = (binding ?? {}) as unknown as Body
  const form = useForm<BindingValues>({
    resolver: zodResolver(schema),
    defaultValues: binding ? bindingValues(initial) : defaults,
  })
  const errors = form.formState.errors
  return (
    <section
      aria-label={title}
      className="flex flex-col gap-3 rounded-md border p-4"
      data-testid="binding-editor"
    >
      <h3 className="font-semibold">{title}</h3>
      <ResourceEditor
        form={form}
        initialBody={binding ? initial : bindingBody(defaults, {})}
        toBody={bindingBody}
        fromBody={bindingValues}
        fields={['role', 'members']}
        onSubmit={(body) => onSave(body as unknown as Binding)}
        submitLabel="Save"
        onCancel={onCancel}
        jsonHint="One binding of the policy: role, members and an optional condition. Saved through setIamPolicy with the policy's etag."
      >
        <Field
          id={`${id}-role`}
          label="Role"
          error={errors.role?.message}
          hint="A predefined role (roles/…) or a custom one (projects/PROJECT/roles/…)."
        >
          <Input
            list={`${id}-roles`}
            autoComplete="off"
            spellCheck={false}
            className="font-mono"
            {...fieldAria(`${id}-role`, errors.role?.message, 'A')}
            {...form.register('role')}
          />
          <datalist id={`${id}-roles`}>
            {[...roleTitles].map(([name, t]) => (
              <option key={name} value={name}>
                {t}
              </option>
            ))}
          </datalist>
        </Field>
        <Field
          id={`${id}-members`}
          label="Members"
          error={errors.members?.message}
          hint="One per line, e.g. user:alice@example.com, serviceAccount:SA_EMAIL, group:…, allUsers."
        >
          <Textarea
            rows={3}
            spellCheck={false}
            className="font-mono"
            {...fieldAria(`${id}-members`, errors.members?.message, 'One')}
            {...form.register('members')}
          />
        </Field>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field
            id={`${id}-ctitle`}
            label="Condition title"
            error={errors.conditionTitle?.message}
            hint="Optional: the binding applies only when the condition holds."
          >
            <Input
              autoComplete="off"
              {...fieldAria(`${id}-ctitle`, errors.conditionTitle?.message, 'Optional')}
              {...form.register('conditionTitle')}
            />
          </Field>
          <Field id={`${id}-cdesc`} label="Condition description">
            <Input
              autoComplete="off"
              {...fieldAria(`${id}-cdesc`)}
              {...form.register('conditionDescription')}
            />
          </Field>
        </div>
        <Field
          id={`${id}-cexpr`}
          label="Condition expression"
          error={errors.conditionExpression?.message}
          hint='CEL over request.time, resource.name and resource.type, e.g. request.time < timestamp("2030-01-01T00:00:00Z")'
        >
          <Textarea
            rows={2}
            spellCheck={false}
            className="font-mono"
            {...fieldAria(`${id}-cexpr`, errors.conditionExpression?.message, 'CEL')}
            {...form.register('conditionExpression')}
          />
        </Field>
      </ResourceEditor>
    </section>
  )
}

/** PolicyJson edits the whole policy, audit configs and all, as JSON. */
function PolicyJson({
  policy,
  onSave,
  onCancel,
}: {
  policy: Policy
  onSave: (p: Policy) => Promise<unknown>
  onCancel: () => void
}) {
  const id = useId()
  const [text, setText] = useState(() => JSON.stringify(policy, null, 2))
  const [error, setError] = useState<string>()
  const [pending, setPending] = useState(false)
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(undefined)
    let body: Body
    try {
      body = parseBody(text)
    } catch (err) {
      setError(errorMessage(err))
      return
    }
    setPending(true)
    try {
      await onSave(body)
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setPending(false)
    }
  }
  return (
    <form
      aria-label="Edit policy JSON"
      onSubmit={(e) => void submit(e)}
      noValidate
      className="flex flex-col gap-3 rounded-md border p-4"
    >
      <label htmlFor={id} className="text-sm font-medium">
        Policy
      </label>
      <Textarea
        id={id}
        spellCheck={false}
        className="min-h-80 font-mono text-xs"
        value={text}
        aria-invalid={!!error}
        aria-describedby={`${id}-hint${error ? ` ${id}-error` : ''}`}
        onChange={(e) => {
          setText(e.target.value)
          setError(undefined)
        }}
      />
      <p id={`${id}-hint`} className="text-xs text-muted-foreground">
        Sent as setIamPolicy&apos;s policy. Keep the etag to fail on concurrent changes; remove it
        to overwrite them.
      </p>
      {error && (
        <p
          id={`${id}-error`}
          role="alert"
          className="rounded-md border border-destructive/50 p-3 text-sm text-destructive"
        >
          {error}
        </p>
      )}
      <div className="flex gap-2">
        <Button type="submit" disabled={pending}>
          Save policy
        </Button>
        <Button type="button" variant="outline" onClick={onCancel}>
          Cancel
        </Button>
      </div>
    </form>
  )
}
