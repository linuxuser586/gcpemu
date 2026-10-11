import { useMutation, useQuery } from '@tanstack/react-query'
import { Play } from 'lucide-react'
import { useState } from 'react'
import { useSearchParams } from 'react-router'

import { errorMessage } from '@/api/errors'
import { infoQuery } from '@/api/queries'
import { Field, fieldAria } from '@/components/resource/Field'
import { Badge } from '@/components/ui/badge'
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

import {
  kindOf,
  mintToken,
  nameError,
  PERMISSION,
  permissionsQuery,
  PRINCIPAL,
  revokeToken,
  serviceAccountsQuery,
  testPermissions,
  type KindId,
  type ResourceRef,
} from './api'
import { ChooseProject, LEVELS, useProject } from './IamLayout'
import { ResourcePicker } from './ResourcePicker'

const lines = (text: string) =>
  text
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter(Boolean)

interface Result {
  principal: string
  r: ResourceRef
  permissions: string[]
  granted: Set<string>
}

/** simulate asks the resource's testIamPermissions as principal. */
async function simulate(principal: string, r: ResourceRef, permissions: string[]): Promise<Result> {
  const token = await mintToken(principal)
  try {
    const granted = await testPermissions(r, permissions, token)
    return { principal, r, permissions, granted: new Set(granted) }
  } finally {
    void revokeToken(token)
  }
}

/**
 * Simulator is the policy simulator (SRS 4.8.3): which of a set of
 * permissions a Principal holds on a resource, answered by the resource's
 * testIamPermissions called as that Principal. The Principal, resource
 * kind and name are in ?principal=, ?kind= and ?name=.
 */
export function Simulator() {
  const project = useProject()
  const [search, setSearch] = useSearchParams()
  const kind = kindOf(search.get('kind'))
  const [principal, setPrincipal] = useState(search.get('principal') ?? '')
  const [name, setName] = useState(search.get('name') ?? '')
  const [perms, setPerms] = useState<string>()
  const [errors, setErrors] = useState<{ principal?: string; name?: string; perms?: string }>({})
  const { data: info } = useQuery(infoQuery())
  const accounts = useQuery({ ...serviceAccountsQuery(project), enabled: !!project })
  const universe = useQuery({ ...permissionsQuery(project), enabled: !!project })
  const run = useMutation({
    meta: { toast: false },
    mutationFn: ({ p, r, list }: { p: string; r: ResourceRef; list: string[] }) =>
      simulate(p, r, list),
  })
  if (!project) return <ChooseProject title="Policy simulator" what="test permissions in it" />
  const defaults = (universe.data ?? []).filter((p) => kind.perms.some((x) => p.startsWith(x)))
  const text = perms ?? defaults.join('\n')
  const setParams = (patch: Record<string, string>) =>
    setSearch(
      (prev) => {
        const next = new URLSearchParams(prev)
        for (const [k, v] of Object.entries(patch)) {
          if (v && !(k === 'kind' && v === 'project')) next.set(k, v)
          else next.delete(k)
        }
        return next
      },
      { replace: true },
    )

  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    const p = principal.trim()
    const n = name.trim()
    const list = lines(text)
    const errs = {
      principal: PRINCIPAL.test(p)
        ? undefined
        : 'Enter a Principal as user:EMAIL or serviceAccount:EMAIL.',
      name: nameError(kind, n),
      perms:
        list.length === 0
          ? 'Enter at least one permission.'
          : list.find((x) => !PERMISSION.test(x)) &&
            `Permission ${list.find((x) => !PERMISSION.test(x))} is not valid.`,
    }
    setErrors(errs)
    if (errs.principal || errs.name || errs.perms) return
    setParams({ principal: p, name: n })
    run.mutate({ p, r: { project, kind: kind.id, name: n }, list })
  }

  const result = run.data
  const granted = result ? result.permissions.filter((p) => result.granted.has(p)).length : 0
  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-2xl font-semibold">Policy simulator</h1>
        <p className="text-sm text-muted-foreground">
          Which permissions a Principal holds on a resource of Project{' '}
          <span className="font-mono">{project}</span>, as its{' '}
          <span className="font-mono">testIamPermissions</span> answers when called as that
          Principal.
        </p>
      </div>
      <Card>
        <form noValidate aria-label="Simulate" className="flex flex-col gap-4" onSubmit={submit}>
          <Field
            id="sim-principal"
            label="Principal"
            error={errors.principal}
            hint="user:EMAIL or serviceAccount:EMAIL"
          >
            <Input
              list="sim-principals"
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              {...fieldAria('sim-principal', errors.principal, 'user')}
              value={principal}
              onChange={(e) => {
                setPrincipal(e.target.value)
                setErrors((x) => ({ ...x, principal: undefined }))
              }}
            />
            <datalist id="sim-principals">
              {(accounts.data ?? []).map((a) => (
                <option key={a.email} value={`serviceAccount:${a.email}`} />
              ))}
            </datalist>
          </Field>
          <ResourcePicker
            project={project}
            kind={kind.id}
            name={name}
            error={errors.name}
            onKind={(k: KindId) => {
              setName('')
              setPerms(undefined)
              setErrors({})
              setParams({ kind: k, name: '' })
            }}
            onName={(n) => {
              setName(n)
              setErrors((x) => ({ ...x, name: undefined }))
            }}
          />
          <Field
            id="sim-permissions"
            label="Permissions"
            error={errors.perms}
            hint={
              <>
                One per line. By default, every permission the Instance knows on a{' '}
                {kind.label.toLowerCase()}.{' '}
                {perms !== undefined && (
                  <button
                    type="button"
                    className="text-primary underline underline-offset-4"
                    onClick={() => setPerms(undefined)}
                  >
                    Reset
                  </button>
                )}
              </>
            }
          >
            <Textarea
              rows={8}
              spellCheck={false}
              className="font-mono text-xs"
              {...fieldAria('sim-permissions', errors.perms, 'One')}
              value={text}
              onChange={(e) => {
                setPerms(e.target.value)
                setErrors((x) => ({ ...x, perms: undefined }))
              }}
            />
          </Field>
          <Button type="submit" className="w-fit" disabled={run.isPending}>
            <Play aria-hidden />
            Test permissions
          </Button>
          {run.isError && (
            <p
              role="alert"
              className="rounded-md border border-destructive/50 p-3 text-sm text-destructive"
            >
              {errorMessage(run.error)}
            </p>
          )}
        </form>
      </Card>
      {result && (
        <Card>
          <CardTitle>Results</CardTitle>
          <p role="status" className="text-sm">
            <span className="font-mono">{result.principal}</span> holds {granted} of{' '}
            {result.permissions.length} permissions on{' '}
            <span className="font-mono">
              {kindOf(result.r.kind).label}
              {result.r.name ? ` ${result.r.name}` : ` ${result.r.project}`}
            </span>
            .
          </p>
          {info && (
            <p className="text-sm text-muted-foreground">
              {info.iamMode === 'off'
                ? 'IAM is off: every call is allowed, and resources other than Projects report every permission as held.'
                : LEVELS[info.iamMode]}
            </p>
          )}
          <Table aria-label="Results">
            <TableHeader>
              <TableRow>
                <TableHead>Permission</TableHead>
                <TableHead>Result</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {result.permissions.map((p) => (
                <TableRow key={p} data-testid="result">
                  <TableCell className="font-mono text-xs">{p}</TableCell>
                  <TableCell>
                    {result.granted.has(p) ? (
                      <Badge variant="success">Granted</Badge>
                    ) : (
                      <Badge variant="destructive">Denied</Badge>
                    )}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </Card>
      )}
    </div>
  )
}
