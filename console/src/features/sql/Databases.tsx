import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Pencil, Plus, Trash2 } from 'lucide-react'
import { useForm } from 'react-hook-form'
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
  databasesQuery,
  deleteDatabase,
  insertDatabase,
  isRunning,
  patchDatabase,
  waitOperation,
  type Database,
} from './api'
import { NotRunning, useInstance } from './InstancePage'
import { instancePath, useInstanceRef } from './SqlLayout'

// Databases (FR-SQL-002, FR-UI-011): databases.insert, patch and delete.
// PostgreSQL cannot change a database's name, charset or collation, so
// an edit the API accepts changes nothing; the form shows its message.

const str = (v: unknown) => (typeof v === 'string' ? v : '')

/** Databases lists the instance's databases. */
export function Databases() {
  const i = useInstance()
  const ref = useInstanceRef()
  const base = instancePath(ref.instance)
  const query = useQuery(databasesQuery(ref))
  const qc = useQueryClient()
  const del = useMutation({
    mutationFn: async (name: string) => waitOperation(ref.project, await deleteDatabase(ref, name)),
    onSuccess: (_op, name) => {
      void qc.invalidateQueries({ queryKey: ['sql'] })
      toast.success(`Deleted database ${name}.`)
    },
  })
  const running = isRunning(i)
  const dbs = query.data ?? []
  return (
    <div className="flex flex-col gap-4">
      {!running && <NotRunning i={i} what="Databases can be created and deleted" />}
      <Card>
        {running && (
          <div className="flex justify-end">
            <Button asChild size="sm">
              <Link to={`${base}/databases/create`}>
                <Plus aria-hidden />
                Create database
              </Link>
            </Button>
          </div>
        )}
        <QueryStatus query={query} />
        {query.data && dbs.length === 0 && (
          <p className="text-sm text-muted-foreground">No databases.</p>
        )}
        {dbs.length > 0 && (
          <Table aria-label="Databases">
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Charset</TableHead>
                <TableHead>Collation</TableHead>
                <TableHead>
                  <span className="sr-only">Actions</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {dbs.map((d) => (
                <TableRow key={d.name} data-testid="database">
                  <TableCell className="font-mono">{d.name}</TableCell>
                  <TableCell className="font-mono">{d.charset ?? '—'}</TableCell>
                  <TableCell className="font-mono">{d.collation ?? '—'}</TableCell>
                  <TableCell>
                    <div className="flex justify-end gap-2">
                      <Button variant="outline" size="sm" asChild>
                        <Link
                          to={`${base}/databases/${encodeURIComponent(d.name)}/edit`}
                          aria-label={`Edit database ${d.name}`}
                        >
                          <Pencil aria-hidden />
                          Edit
                        </Link>
                      </Button>
                      <ConfirmDialog
                        trigger={
                          <Button
                            variant="outline"
                            size="sm"
                            disabled={!running}
                            aria-label={`Delete database ${d.name}`}
                          >
                            <Trash2 aria-hidden />
                            Delete
                          </Button>
                        }
                        title={`Delete database ${d.name}?`}
                        description="It is dropped with everything in it, closing its connections. This cannot be undone."
                        confirmLabel="Delete database"
                        onConfirm={() => del.mutateAsync(d.name)}
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

const schema = z.object({
  name: z
    .string()
    .min(1, 'Invalid request: Invalid database name ().')
    .max(63, 'Invalid request: a database name has at most 63 characters.'),
  charset: z.string().trim(),
  collation: z.string().trim(),
})
type Values = z.infer<typeof schema>

export function databaseBody(v: Values, base: Body): Body {
  const b: Body = { ...base, name: v.name }
  for (const k of ['charset', 'collation'] as const) {
    if (v[k]) b[k] = v[k]
    else delete b[k]
  }
  return b
}

const databaseValues = (b: Body): Values => ({
  name: str(b.name),
  charset: str(b.charset),
  collation: str(b.collation),
})

/** CreateDatabase creates a database owned by cloudsqlsuperuser. */
export function CreateDatabase() {
  const ref = useInstanceRef()
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const back = () => navigate(`${instancePath(ref.instance)}/databases`)
  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: { name: '', charset: '', collation: '' },
  })
  const errors = form.formState.errors
  const create = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => waitOperation(ref.project, await insertDatabase(ref, body)),
    onSuccess: (_op, body) => {
      void qc.invalidateQueries({ queryKey: ['sql'] })
      toast.success(`Created database ${str(body.name)}.`)
      back()
    },
  })
  return (
    <Card>
      <h2 className="text-lg font-semibold">Create database</h2>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={databaseBody}
        fromBody={databaseValues}
        fields={['name', 'charset', 'collation']}
        onSubmit={(body) => create.mutateAsync(body)}
        submitLabel="Create"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">databases.insert</span>.
          </>
        }
      >
        <Field id="database-name" label="Name" error={errors.name?.message}>
          <Input
            autoComplete="off"
            spellCheck={false}
            {...fieldAria('database-name', errors.name?.message)}
            {...form.register('name')}
          />
        </Field>
        <DatabaseFields form={form} />
      </ResourceEditor>
    </Card>
  )
}

function DatabaseFields({ form }: { form: ReturnType<typeof useForm<Values>> }) {
  return (
    <div className="grid gap-4 sm:grid-cols-2">
      <Field id="database-charset" label="Charset" hint="Empty for UTF8.">
        <Input
          spellCheck={false}
          {...fieldAria('database-charset', undefined, 'Empty')}
          {...form.register('charset')}
        />
      </Field>
      <Field id="database-collation" label="Collation" hint="Empty for en_US.UTF8.">
        <Input
          spellCheck={false}
          {...fieldAria('database-collation', undefined, 'Empty')}
          {...form.register('collation')}
        />
      </Field>
    </div>
  )
}

/** EditDatabase sends databases.patch. */
export function EditDatabase() {
  const ref = useInstanceRef()
  const { database = '' } = useParams()
  const query = useQuery(databasesQuery(ref))
  const db = query.data?.find((d) => d.name === database)
  return (
    <Card>
      <h2 className="text-lg font-semibold">
        Edit database <span className="font-mono">{database}</span>
      </h2>
      <QueryStatus query={query} />
      {query.data && !db && (
        <p role="status" className="text-sm text-muted-foreground">
          Instance {ref.instance} has no database <span className="font-mono">{database}</span>.
        </p>
      )}
      {db && <EditDatabaseForm db={db} />}
    </Card>
  )
}

function EditDatabaseForm({ db }: { db: Database }) {
  const ref = useInstanceRef()
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const back = () => navigate(`${instancePath(ref.instance)}/databases`)
  const defaults = { name: db.name, charset: db.charset ?? '', collation: db.collation ?? '' }
  const form = useForm<Values>({ resolver: zodResolver(schema), defaultValues: defaults })
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) =>
      waitOperation(ref.project, await patchDatabase(ref, db.name, body)),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['sql'] })
      toast.success(`Updated database ${db.name}.`)
      back()
    },
  })
  return (
    <ResourceEditor
      form={form}
      initialBody={databaseBody(defaults, {})}
      toBody={databaseBody}
      fromBody={databaseValues}
      fields={['charset', 'collation']}
      onSubmit={(body) => save.mutateAsync(body)}
      submitLabel="Save"
      onCancel={back}
      jsonHint={
        <>
          Sent to <span className="font-mono">databases.patch</span>.
        </>
      }
    >
      <p className="text-sm text-muted-foreground">
        PostgreSQL cannot change a database&rsquo;s charset or collation once it is created.
      </p>
      <DatabaseFields form={form} />
    </ResourceEditor>
  )
}
