import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Pencil, Plus, Trash2 } from 'lucide-react'
import { useForm, useWatch } from 'react-hook-form'
import { useParams } from 'react-router'
import { z } from 'zod'

import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { Field, fieldAria } from '@/components/resource/Field'
import { ResourceEditor, type Body } from '@/components/resource/ResourceEditor'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { toast } from '@/lib/toast'

import {
  deleteUser,
  insertUser,
  isRunning,
  updateUser,
  usersQuery,
  waitOperation,
  type User,
} from './api'
import { NotRunning, useInstance } from './InstancePage'
import { instancePath, useInstanceRef } from './SqlLayout'

// Users (FR-SQL-002, FR-SQL-006, FR-UI-011): built-in users with a
// password, and IAM users and service accounts that log in with an
// access token. users.update changes a built-in user's password.

const str = (v: unknown) => (typeof v === 'string' ? v : '')

const TYPES = [
  ['BUILT_IN', 'Built-in (password)'],
  ['CLOUD_IAM_USER', 'IAM user'],
  ['CLOUD_IAM_SERVICE_ACCOUNT', 'IAM service account'],
] as const

const typeLabel = (t?: string) => TYPES.find(([k]) => k === (t || 'BUILT_IN'))?.[1] ?? t

/** Users lists the instance's database users. */
export function Users() {
  const i = useInstance()
  const ref = useInstanceRef()
  const base = instancePath(ref.instance)
  const query = useQuery(usersQuery(ref))
  const qc = useQueryClient()
  const del = useMutation({
    mutationFn: async (name: string) => waitOperation(ref.project, await deleteUser(ref, name)),
    onSuccess: (_op, name) => {
      void qc.invalidateQueries({ queryKey: ['sql'] })
      toast.success(`Deleted user ${name}.`)
    },
  })
  const running = isRunning(i)
  const users = query.data ?? []
  return (
    <div className="flex flex-col gap-4">
      {!running && <NotRunning i={i} what="Users can be added, changed and deleted" />}
      <Card>
        {running && (
          <div className="flex justify-end">
            <Button asChild size="sm">
              <Link to={`${base}/users/create`}>
                <Plus aria-hidden />
                Add user
              </Link>
            </Button>
          </div>
        )}
        <QueryStatus query={query} />
        {query.data && users.length === 0 && (
          <p className="text-sm text-muted-foreground">No users.</p>
        )}
        {users.length > 0 && (
          <Table aria-label="Users">
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>
                  <span className="sr-only">Actions</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {users.map((u) => (
                <TableRow key={u.name} data-testid="user">
                  <TableCell className="font-mono">{u.name}</TableCell>
                  <TableCell>{typeLabel(u.type)}</TableCell>
                  <TableCell>
                    <div className="flex justify-end gap-2">
                      {running && (u.type ?? 'BUILT_IN') === 'BUILT_IN' && (
                        <Button variant="outline" size="sm" asChild>
                          <Link
                            to={`${base}/users/${encodeURIComponent(u.name)}/edit`}
                            aria-label={`Edit user ${u.name}`}
                          >
                            <Pencil aria-hidden />
                            Edit
                          </Link>
                        </Button>
                      )}
                      <ConfirmDialog
                        trigger={
                          <Button
                            variant="outline"
                            size="sm"
                            disabled={!running}
                            aria-label={`Delete user ${u.name}`}
                          >
                            <Trash2 aria-hidden />
                            Delete
                          </Button>
                        }
                        title={`Delete user ${u.name}?`}
                        description="Its role is dropped; objects it owns must belong to another role first."
                        confirmLabel="Delete user"
                        onConfirm={() => del.mutateAsync(u.name)}
                      />
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  )
}

// The API's messages for a user's name, by type (validateUser).
const createSchema = z
  .object({
    name: z.string(),
    type: z.enum(['BUILT_IN', 'CLOUD_IAM_USER', 'CLOUD_IAM_SERVICE_ACCOUNT']),
    password: z.string(),
  })
  .superRefine((v, ctx) => {
    const issue = (message: string) => ctx.addIssue({ code: 'custom', path: ['name'], message })
    if (!v.name) return issue('Invalid request: User name is required.')
    if (v.name.length > 63) {
      return issue(
        `Invalid request: User name (${v.name}) is too long; the maximum is 63 characters.`,
      )
    }
    if (v.type === 'BUILT_IN' && v.name.startsWith('cloudsql')) {
      return issue(`Invalid request: User name (${v.name}) is reserved.`)
    }
    if (v.type === 'CLOUD_IAM_USER' && !v.name.includes('@')) {
      return issue(`Invalid request: IAM user name (${v.name}) must be an email address.`)
    }
    if (v.type === 'CLOUD_IAM_SERVICE_ACCOUNT') {
      if (v.name.endsWith('.gserviceaccount.com')) {
        return issue(
          `Invalid request: For a service account, the user name must be the email address without the .gserviceaccount.com domain suffix (${v.name.slice(0, -'.gserviceaccount.com'.length)}).`,
        )
      }
      if (!v.name.includes('@')) {
        return issue(
          `Invalid request: IAM service account user name (${v.name}) must be the service account email without .gserviceaccount.com.`,
        )
      }
    }
  })
type CreateValues = z.infer<typeof createSchema>

export function userBody(v: CreateValues, base: Body): Body {
  const b: Body = { ...base, name: v.name, type: v.type }
  if (v.type === 'BUILT_IN' && v.password) b.password = v.password
  else delete b.password
  return b
}

const userValues = (b: Body, prev: CreateValues): CreateValues => ({
  name: str(b.name),
  type: (str(b.type) || 'BUILT_IN') as CreateValues['type'],
  password: str(b.password) || (b.password === undefined ? '' : prev.password),
})

/** CreateUser adds a database user. */
export function CreateUser() {
  const ref = useInstanceRef()
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const back = () => navigate(`${instancePath(ref.instance)}/users`)
  const form = useForm<CreateValues>({
    resolver: zodResolver(createSchema),
    defaultValues: { name: '', type: 'BUILT_IN', password: '' },
  })
  const type = useWatch({ control: form.control, name: 'type' })
  const errors = form.formState.errors
  const create = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => waitOperation(ref.project, await insertUser(ref, body)),
    onSuccess: (_op, body) => {
      void qc.invalidateQueries({ queryKey: ['sql'] })
      toast.success(`Added user ${str(body.name)}.`)
      back()
    },
  })
  const hint =
    type === 'CLOUD_IAM_USER'
      ? 'The user’s email address. It logs in with an access token when the cloudsql.iam_authentication flag is on.'
      : type === 'CLOUD_IAM_SERVICE_ACCOUNT'
        ? 'The service account’s email without .gserviceaccount.com.'
        : undefined
  return (
    <Card>
      <h2 className="text-lg font-semibold">Add user</h2>
      <ResourceEditor
        form={form}
        initialBody={{ type: 'BUILT_IN' }}
        toBody={userBody}
        fromBody={userValues}
        fields={['name', 'password']}
        onSubmit={(body) => create.mutateAsync(body)}
        submitLabel="Add"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">users.insert</span>.
          </>
        }
      >
        <Field id="user-type" label="Type">
          <NativeSelect id="user-type" {...form.register('type')}>
            {TYPES.map(([v, label]) => (
              <option key={v} value={v}>
                {label}
              </option>
            ))}
          </NativeSelect>
        </Field>
        <Field id="user-name" label="Name" error={errors.name?.message} hint={hint}>
          <Input
            autoComplete="off"
            spellCheck={false}
            {...fieldAria('user-name', errors.name?.message, hint)}
            {...form.register('name')}
          />
        </Field>
        {type === 'BUILT_IN' && (
          <Field id="user-password" label="Password">
            <Input
              type="password"
              autoComplete="new-password"
              {...fieldAria('user-password')}
              {...form.register('password')}
            />
          </Field>
        )}
      </ResourceEditor>
    </Card>
  )
}

const editSchema = z.object({ password: z.string() })
type EditValues = z.infer<typeof editSchema>

/** EditUser sets a built-in user's password with users.update. */
export function EditUser() {
  const ref = useInstanceRef()
  const { user = '' } = useParams()
  const query = useQuery(usersQuery(ref))
  const u = query.data?.find((x) => x.name === user)
  return (
    <Card>
      <h2 className="text-lg font-semibold">
        Edit user <span className="font-mono">{user}</span>
      </h2>
      <QueryStatus query={query} />
      {query.data && !u && (
        <p role="status" className="text-sm text-muted-foreground">
          Instance {ref.instance} has no user <span className="font-mono">{user}</span>.
        </p>
      )}
      {u && <EditUserForm user={u} />}
    </Card>
  )
}

function EditUserForm({ user }: { user: User }) {
  const ref = useInstanceRef()
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const back = () => navigate(`${instancePath(ref.instance)}/users`)
  const form = useForm<EditValues>({
    resolver: zodResolver(editSchema),
    defaultValues: { password: '' },
  })
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) =>
      waitOperation(ref.project, await updateUser(ref, user.name, body)),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['sql'] })
      toast.success(`Updated user ${user.name}.`)
      back()
    },
  })
  return (
    <ResourceEditor
      form={form}
      initialBody={{ name: user.name }}
      toBody={(v, base) => ({ ...base, password: v.password })}
      fromBody={(b) => ({ password: str(b.password) })}
      fields={['password']}
      onSubmit={(body) => save.mutateAsync(body)}
      submitLabel="Save"
      onCancel={back}
      jsonHint={
        <>
          Sent to <span className="font-mono">users.update</span> with{' '}
          <span className="font-mono">?name={user.name}</span>.
        </>
      }
    >
      <Field id="user-new-password" label="New password" hint="Empty keeps the current password.">
        <Input
          type="password"
          autoComplete="new-password"
          {...fieldAria('user-new-password', undefined, 'Empty')}
          {...form.register('password')}
        />
      </Field>
    </ResourceEditor>
  )
}
