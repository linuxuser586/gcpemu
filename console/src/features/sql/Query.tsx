import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Play } from 'lucide-react'
import { useId, useState } from 'react'

import { errorMessage } from '@/api/errors'
import { QueryStatus } from '@/components/QueryStatus'
import { Field, fieldAria } from '@/components/resource/Field'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
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
import { Textarea } from '@/components/ui/textarea'
import { toast } from '@/lib/toast'

import {
  databasesQuery,
  executeSql,
  isRunning,
  patchInstance,
  usersQuery,
  type ExecuteSqlResponse,
  type QueryResult,
} from './api'
import { NotRunning, useInstance } from './InstancePage'
import { DEFAULT_ROW_LIMIT, readOnlySql, unwrapResults } from './runner'
import { useInstanceRef } from './SqlLayout'

/**
 * Query is the SQL query runner (SRS 4.8.3): statements run through
 * instances.executeSql (the Data API) as a database user, in a read-only
 * transaction unless the user unticks it.
 */
export function Query() {
  const i = useInstance()
  const ref = useInstanceRef()
  const qc = useQueryClient()
  const allow = useMutation({
    mutationFn: () => patchInstance(ref, { settings: { dataApiAccess: 'ALLOW_DATA_API' } }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['sql'] })
      toast.success(`Allowing Data API access on ${ref.instance}.`)
    },
  })
  if (i.settings?.dataApiAccess !== 'ALLOW_DATA_API') {
    return (
      <Card role="status">
        <CardTitle>Data API access is off</CardTitle>
        <p className="text-sm text-muted-foreground">
          The query runner sends statements through{' '}
          <span className="font-mono">instances.executeSql</span>, which an instance answers only
          when its <span className="font-mono">settings.dataApiAccess</span> is{' '}
          <span className="font-mono">ALLOW_DATA_API</span>.
        </p>
        <div>
          <Button disabled={allow.isPending} onClick={() => allow.mutate()}>
            Allow Data API access
          </Button>
        </div>
      </Card>
    )
  }
  if (!isRunning(i)) return <NotRunning i={i} what="Statements can run" />
  return <Runner />
}

function Runner() {
  const ref = useInstanceRef()
  const id = useId()
  const databases = useQuery(databasesQuery(ref))
  const users = useQuery(usersQuery(ref))
  const [database, setDatabase] = useState('postgres')
  const [user, setUser] = useState('postgres')
  const [sql, setSql] = useState('SELECT version();')
  const [readOnly, setReadOnly] = useState(true)
  const [rowLimit, setRowLimit] = useState(String(DEFAULT_ROW_LIMIT))
  const run = useMutation({
    meta: { toast: false },
    mutationFn: async (v: { sql: string; readOnly: boolean }) => {
      const res = await executeSql(ref, {
        user,
        database,
        sqlStatement: v.readOnly ? readOnlySql(v.sql) : v.sql,
        rowLimit: /^\d+$/.test(rowLimit) && rowLimit !== '0' ? rowLimit : undefined,
        partialResultMode: 'ALLOW_PARTIAL_RESULT',
        application: 'gcpemu-console',
      })
      return v.readOnly ? unwrapResults(res) : res
    },
  })
  const submit = () => {
    if (sql.trim()) run.mutate({ sql, readOnly })
  }
  const dbNames = [...new Set(['postgres', ...(databases.data ?? []).map((d) => d.name)])]
  const userNames = [...new Set(['postgres', ...(users.data ?? []).map((u) => u.name)])]

  return (
    <div className="flex flex-col gap-6">
      <Card>
        <form
          aria-label="Query"
          className="flex flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault()
            submit()
          }}
        >
          <div className="grid gap-4 sm:grid-cols-3">
            <Field id={`${id}-db`} label="Database">
              <NativeSelect
                id={`${id}-db`}
                value={database}
                onChange={(e) => setDatabase(e.target.value)}
              >
                {dbNames.map((n) => (
                  <option key={n} value={n}>
                    {n}
                  </option>
                ))}
              </NativeSelect>
            </Field>
            <Field id={`${id}-user`} label="User">
              <NativeSelect
                id={`${id}-user`}
                value={user}
                onChange={(e) => setUser(e.target.value)}
              >
                {userNames.map((n) => (
                  <option key={n} value={n}>
                    {n}
                  </option>
                ))}
              </NativeSelect>
            </Field>
            <Field id={`${id}-limit`} label="Row limit" hint="Rows shown per statement; 0 for all.">
              <Input
                type="number"
                min={0}
                value={rowLimit}
                onChange={(e) => setRowLimit(e.target.value)}
                {...fieldAria(`${id}-limit`, undefined, 'Rows')}
              />
            </Field>
          </div>
          <QueryStatus query={databases} rows={1} />
          <Field
            id={`${id}-sql`}
            label="SQL"
            hint="Statements separated by semicolons. Ctrl+Enter or ⌘+Enter runs them."
          >
            <Textarea
              rows={8}
              spellCheck={false}
              className="font-mono"
              value={sql}
              onChange={(e) => setSql(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
                  e.preventDefault()
                  submit()
                }
              }}
              {...fieldAria(`${id}-sql`, undefined, 'Statements')}
            />
          </Field>
          <div className="flex flex-wrap items-center gap-4">
            <Button type="submit" disabled={run.isPending || !sql.trim()}>
              <Play aria-hidden />
              Run
            </Button>
            <label className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                className="size-4"
                checked={readOnly}
                onChange={(e) => setReadOnly(e.target.checked)}
              />
              Read-only
            </label>
            {!readOnly && (
              <p className="text-sm text-amber-700 dark:text-amber-400">
                Statements can change data.
              </p>
            )}
          </div>
        </form>
      </Card>
      {run.isError && (
        <p
          role="alert"
          className="rounded-md border border-destructive/50 p-3 text-sm text-destructive"
        >
          {errorMessage(run.error)}
        </p>
      )}
      {run.data && <Results res={run.data} />}
    </div>
  )
}

/** Results shows each statement's rows, or the error that stopped them. */
function Results({ res }: { res: ExecuteSqlResponse }) {
  const results = res.results ?? []
  return (
    <section aria-label="Results" className="flex flex-col gap-4">
      {res.status && (
        <p
          role="alert"
          className="rounded-md border border-destructive/50 p-3 font-mono text-sm break-words text-destructive"
        >
          {res.status.message}
        </p>
      )}
      {(res.messages ?? []).map((m, n) => (
        <p key={n} className="rounded-md bg-muted p-2 font-mono text-xs">
          {m.severity}: {m.message}
        </p>
      ))}
      {!res.status && results.length === 0 && (
        <p className="text-sm text-muted-foreground">No statements ran.</p>
      )}
      {results.map((r, n) => (
        <ResultTable key={n} result={r} n={n + 1} />
      ))}
      {res.metadata?.sqlStatementExecutionTime && (
        <p className="text-xs text-muted-foreground">
          Ran in {res.metadata.sqlStatementExecutionTime}.
        </p>
      )}
    </section>
  )
}

function ResultTable({ result, n }: { result: QueryResult; n: number }) {
  const cols = result.columns ?? []
  const rows = result.rows ?? []
  return (
    <Card data-testid="result">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h3 className="text-sm font-semibold">
          Statement {n}: <span className="font-mono">{result.message ?? ''}</span>
        </h3>
        {result.partialResult && (
          <p className="text-xs text-muted-foreground">
            Showing the first {rows.length} rows only.
          </p>
        )}
      </div>
      {cols.length > 0 && (
        <div className="max-h-[32rem] overflow-auto">
          <Table aria-label={`Result ${n}`}>
            <TableHeader>
              <TableRow>
                {cols.map((c, k) => (
                  <TableHead key={k}>
                    <span className="font-mono">{c.name}</span>{' '}
                    <span className="text-xs font-normal text-muted-foreground">{c.type}</span>
                  </TableHead>
                ))}
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((r, k) => (
                <TableRow key={k}>
                  {r.values.map((v, j) => (
                    <TableCell key={j} className="font-mono whitespace-pre">
                      {v.nullValue ? (
                        <span className="text-muted-foreground italic">NULL</span>
                      ) : (
                        (v.value ?? '')
                      )}
                    </TableCell>
                  ))}
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}
    </Card>
  )
}
