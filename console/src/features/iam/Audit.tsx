import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { CircleCheck, KeyRound, Trash2 } from 'lucide-react'
import { useState } from 'react'
import { useSearchParams } from 'react-router'

import { infoQuery } from '@/api/queries'
import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { Badge } from '@/components/ui/badge'
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
import { serviceName } from '@/lib/services'
import { toast } from '@/lib/toast'
import { useViewState } from '@/lib/viewState'

import { clearDenials, denialsQuery, grantOnProject, type Denial } from './api'
import { LEVELS, rolePath } from './IamLayout'

export type DenialStatus = 'missing' | 'granted'

/** denialMatches reports whether q (lower case) is in a denial's Principal, permission, resource, role or deny policy. */
export const denialMatches = (d: Denial, q: string) =>
  [d.principal, d.permission, d.resource, d.role, d.method, d.denyPolicy].some((s) =>
    (s ?? '').toLowerCase().includes(q),
  )

function formatTime(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}

/**
 * Audit is the IAM audit view (FR-UI-014): every call the audit IAM
 * enforcement level would deny, with the missing permission, the smallest
 * predefined role that grants it and a one-click grant of that role on
 * the Project owning the resource. ?project= scopes it to one Project;
 * ?status= (missing or granted) and ?q= filter it.
 */
export function Audit() {
  const [view, setView] = useViewState()
  const project = view.project ?? ''
  const [text, setText] = useState(view.q ?? '')
  const [search, setSearch] = useSearchParams()
  const status = (search.get('status') ?? '') as DenialStatus | ''
  const qc = useQueryClient()
  const { data: info } = useQuery(infoQuery())
  const query = useQuery(denialsQuery(project))
  const clear = useMutation({
    mutationFn: clearDenials,
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['iam', 'denials'] })
      toast.success('Cleared the IAM audit log.')
    },
  })

  const all = query.data ?? []
  const q = text.toLowerCase()
  const shown = all.filter(
    (d) => denialMatches(d, q) && (!status || (status === 'granted' ? d.granted : !d.granted)),
  )
  const missing = all.filter((d) => !d.granted).length

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">IAM audit</h1>
          <p className="text-sm text-muted-foreground">
            Calls whose caller lacked a permission
            {project ? (
              <>
                {' '}
                on resources of Project <span className="font-mono">{project}</span>
              </>
            ) : (
              ', in every Project'
            )}
            , and the smallest predefined role that grants it. Granting binds that role on the
            resource&apos;s Project.
          </p>
        </div>
        {all.length > 0 && (
          <ConfirmDialog
            trigger={
              <Button variant="outline">
                <Trash2 aria-hidden />
                Clear
              </Button>
            }
            title="Clear the IAM audit log?"
            description="Every denied call is forgotten, in every Project. Later calls are logged again."
            confirmLabel="Clear"
            onConfirm={() => clear.mutateAsync()}
          />
        )}
      </div>
      {info && (
        <p role="note" className="text-sm text-muted-foreground">
          {info.iamMode === 'off'
            ? 'IAM is off: calls are not checked, so none are logged. Switch the IAM enforcement level to audit or enforce to see them.'
            : LEVELS[info.iamMode]}
        </p>
      )}
      <Card>
        <form
          role="search"
          aria-label="Filter denials"
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => e.preventDefault()}
        >
          <label htmlFor="audit-filter" className="flex min-w-60 flex-1 flex-col gap-1 text-sm">
            Filter
            <Input
              id="audit-filter"
              type="search"
              placeholder="Principal, permission, resource or role"
              value={text}
              onChange={(e) => {
                setText(e.target.value)
                setView({ q: e.target.value })
              }}
            />
          </label>
          <label htmlFor="audit-status" className="flex flex-col gap-1 text-sm">
            Status
            <NativeSelect
              id="audit-status"
              value={status}
              onChange={(e) =>
                setSearch((prev) => {
                  const next = new URLSearchParams(prev)
                  if (e.target.value) next.set('status', e.target.value)
                  else next.delete('status')
                  return next
                })
              }
            >
              <option value="">Any</option>
              <option value="missing">Still missing</option>
              <option value="granted">Granted since</option>
            </NativeSelect>
          </label>
        </form>
      </Card>
      <Card>
        <QueryStatus query={query} />
        {query.data && (
          <p role="status" className="text-sm">
            {all.length === 0
              ? 'No denied calls yet.'
              : `${all.length} denied ${all.length === 1 ? 'call' : 'calls'}; ${missing} still missing.`}
          </p>
        )}
        {query.data && all.length > 0 && shown.length === 0 && (
          <p className="text-sm text-muted-foreground">No denied calls match the filter.</p>
        )}
        {shown.length > 0 && (
          <Table aria-label="Denied calls">
            <TableHeader>
              <TableRow>
                <TableHead>Principal</TableHead>
                <TableHead>Missing permission</TableHead>
                <TableHead>Resource</TableHead>
                <TableHead>Latest call</TableHead>
                <TableHead>Smallest role</TableHead>
                <TableHead>
                  <span className="sr-only">Grant</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((d) => (
                <DenialRow key={d.id} d={d} />
              ))}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  )
}

function DenialRow({ d }: { d: Denial }) {
  const qc = useQueryClient()
  const grant = useMutation({
    mutationFn: () => grantOnProject(d.project!, d.role!, d.principal),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['iam', 'denials'] })
      void qc.invalidateQueries({ queryKey: ['iam', 'policy'] })
      toast.success(`Granted ${d.role} to ${d.principal} on Project ${d.project}.`)
    },
  })
  return (
    <TableRow data-testid="denial" data-granted={d.granted}>
      <TableCell className="font-mono text-xs break-all">{d.principal}</TableCell>
      <TableCell className="font-mono text-xs break-all">{d.permission}</TableCell>
      <TableCell className="font-mono text-xs break-all">{d.resource}</TableCell>
      <TableCell className="text-xs">
        <div className="flex flex-col gap-1">
          {d.method && (
            <span className="font-mono break-all">
              {d.service && <span title={d.service}>{serviceName(d.service)}: </span>}
              {d.method}
            </span>
          )}
          <span className="flex flex-wrap items-center gap-2">
            {d.level === 'enforce' ? (
              <Badge variant="destructive">Failed</Badge>
            ) : (
              <Badge variant="warning">Allowed by audit</Badge>
            )}
            <span>
              {d.count === 1 ? 'once' : `${d.count} times`}, last {formatTime(d.lastTime)}
            </span>
          </span>
        </div>
      </TableCell>
      <TableCell className="text-xs">
        {d.denyPolicy ? (
          <span>
            <span className="text-muted-foreground">Refused by deny policy </span>
            <span className="font-mono break-all">{d.denyPolicy}</span>
          </span>
        ) : d.role ? (
          <Link className="text-primary underline-offset-4 hover:underline" to={rolePath(d.role)}>
            <span className="font-mono">{d.role}</span>
            {d.roleTitle && <span className="block text-muted-foreground">{d.roleTitle}</span>}
          </Link>
        ) : (
          <span className="text-muted-foreground">No predefined role grants it.</span>
        )}
      </TableCell>
      <TableCell className="text-right">
        {d.granted ? (
          <Badge variant="success">
            <CircleCheck aria-hidden />
            Granted
          </Badge>
        ) : d.role && d.project ? (
          <Button
            size="sm"
            disabled={grant.isPending}
            aria-label={`Grant ${d.role} to ${d.principal} on Project ${d.project}`}
            onClick={() => grant.mutate()}
          >
            <KeyRound aria-hidden />
            Grant
          </Button>
        ) : (
          <span className="text-xs text-muted-foreground">
            {d.role ? 'No Project owns the resource.' : ''}
          </span>
        )}
        {d.project && (
          <Link
            className="mt-1 block text-xs text-primary underline-offset-4 hover:underline"
            to={`/iam?project=${encodeURIComponent(d.project)}`}
          >
            Policy of {d.project}
          </Link>
        )}
      </TableCell>
    </TableRow>
  )
}
