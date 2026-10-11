import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ArchiveRestore, Pencil, Trash2 } from 'lucide-react'
import { useState } from 'react'
import { useParams } from 'react-router'

import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { toast } from '@/lib/toast'

import { customRoleQuery, deleteRole, predefinedRoleQuery, undeleteRole, type Role } from './api'
import { ChooseProject, rolePath, useProject } from './IamLayout'

/** PredefinedRolePage is one predefined role (roles/ID) and its permissions. */
export function PredefinedRolePage() {
  const { role = '' } = useParams()
  const query = useQuery(predefinedRoleQuery(role))
  return <RoleView query={query} name={`roles/${role}`} />
}

/**
 * CustomRolePage is one custom role of the chosen Project: edit, delete
 * and undelete it above its details and permissions.
 */
export function CustomRolePage() {
  const project = useProject()
  const { role = '' } = useParams()
  const query = useQuery({ ...customRoleQuery(project, role), enabled: !!project })
  const qc = useQueryClient()
  const r = query.data
  const del = useMutation({
    mutationFn: () => deleteRole(project, role, r?.etag),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['iam'] })
      toast.success(`Deleted role ${role}. It can be undeleted from its page.`)
    },
  })
  const undelete = useMutation({
    mutationFn: () => undeleteRole(project, role, r?.etag),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['iam'] })
      toast.success(`Undeleted role ${role}.`)
    },
  })
  if (!project) return <ChooseProject title="Custom role" what="see its custom roles" />
  const name = `projects/${project}/roles/${role}`
  return (
    <RoleView
      query={query}
      name={name}
      actions={
        r &&
        (r.deleted ? (
          <Button variant="outline" disabled={undelete.isPending} onClick={() => undelete.mutate()}>
            <ArchiveRestore aria-hidden />
            Undelete role
          </Button>
        ) : (
          <>
            <Button variant="outline" asChild>
              <Link to={rolePath(name, 'edit')}>
                <Pencil aria-hidden />
                Edit role
              </Link>
            </Button>
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Delete role
                </Button>
              }
              title={`Delete role ${role}?`}
              description="Bindings to it stop granting anything; the role can be undeleted, and its ID cannot be reused until then."
              confirmLabel="Delete role"
              onConfirm={() => del.mutateAsync()}
            />
          </>
        ))
      }
    />
  )
}

function RoleView({
  query,
  name,
  actions,
}: {
  query: ReturnType<typeof useQuery<Role>>
  name: string
  actions?: React.ReactNode
}) {
  const [filter, setFilter] = useState('')
  const r = query.data
  const perms = (r?.includedPermissions ?? []).filter((p) => p.includes(filter))
  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to="/iam/roles" className="underline-offset-4 hover:underline">
              Roles
            </Link>{' '}
            / <span className="font-mono">{name}</span>
          </p>
          <h1 className="flex flex-wrap items-center gap-2 text-2xl font-semibold">
            {r?.title || name}
            {r?.deleted && <Badge variant="destructive">Deleted</Badge>}
          </h1>
        </div>
        {actions && <div className="flex flex-wrap gap-2">{actions}</div>}
      </div>
      <QueryStatus query={query} />
      {r && (
        <>
          <Card>
            <CardTitle>Details</CardTitle>
            <DetailList
              label="Role details"
              rows={[
                ['Name', <Mono key="n">{r.name}</Mono>],
                ['Title', r.title],
                ['Description', r.description],
                ['Stage', r.stage],
                ['Permissions', String(r.includedPermissions?.length ?? 0)],
              ]}
            />
          </Card>
          <Card>
            <CardTitle>Permissions</CardTitle>
            <label htmlFor="role-perm-filter" className="flex flex-col gap-1 text-sm">
              Filter
              <Input
                id="role-perm-filter"
                type="search"
                placeholder="Permission"
                value={filter}
                onChange={(e) => setFilter(e.target.value)}
              />
            </label>
            {perms.length === 0 ? (
              <p className="text-sm text-muted-foreground">No permissions.</p>
            ) : (
              <ul
                aria-label="Permissions"
                className="grid gap-x-6 gap-y-0.5 font-mono text-xs sm:grid-cols-2 lg:grid-cols-3"
              >
                {perms.map((p) => (
                  <li key={p} className="break-all">
                    {p}
                  </li>
                ))}
              </ul>
            )}
          </Card>
          <Card>
            <CardTitle>Role resource</CardTitle>
            <JsonView value={r} label="Role JSON" />
          </Card>
        </>
      )}
    </div>
  )
}
