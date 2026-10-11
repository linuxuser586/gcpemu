import { useQuery } from '@tanstack/react-query'
import { Plus } from 'lucide-react'
import { useState } from 'react'

import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
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
import { useViewState } from '@/lib/viewState'

import { customRolesQuery, predefinedRolesQuery, type Role } from './api'
import { rolePath } from './IamLayout'

/** matches reports whether a role's name or title contains q (lower case). */
const matches = (r: Role, q: string) =>
  r.name.toLowerCase().includes(q) || (r.title ?? '').toLowerCase().includes(q)

/**
 * Roles lists the chosen Project's custom roles, deleted ones too, and the
 * Instance's predefined roles (FR-IAM-002); ?q= keeps those whose name or
 * title contains it.
 */
export function Roles() {
  const [view, setView] = useViewState()
  const project = view.project ?? ''
  const [text, setText] = useState(view.q ?? '')
  const custom = useQuery({ ...customRolesQuery(project), enabled: !!project })
  const predefined = useQuery(predefinedRolesQuery())
  const q = text.toLowerCase()
  const customShown = (custom.data ?? []).filter((r) => matches(r, q))
  const predefinedShown = (predefined.data ?? []).filter((r) => matches(r, q))

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Roles</h1>
          <p className="text-sm text-muted-foreground">
            Predefined roles of the emulated Services, and custom roles of a Project.
          </p>
        </div>
        {project && (
          <Button asChild>
            <Link to="/iam/create-role">
              <Plus aria-hidden />
              Create role
            </Link>
          </Button>
        )}
      </div>
      <Card>
        <form role="search" aria-label="Filter roles" onSubmit={(e) => e.preventDefault()}>
          <label htmlFor="roles-filter" className="flex flex-col gap-1 text-sm">
            Filter
            <Input
              id="roles-filter"
              type="search"
              placeholder="Role name or title"
              value={text}
              onChange={(e) => {
                setText(e.target.value)
                setView({ q: e.target.value })
              }}
            />
          </label>
        </form>
      </Card>
      <Card>
        <CardTitle>Custom roles</CardTitle>
        {!project && (
          <p role="status" className="text-sm text-muted-foreground">
            Choose a Project with the Project switcher to see its custom roles.
          </p>
        )}
        {project && <QueryStatus query={custom} />}
        {custom.data && custom.data.length === 0 && (
          <p className="text-sm text-muted-foreground">No custom roles in this Project yet.</p>
        )}
        {custom.data && custom.data.length > 0 && customShown.length === 0 && (
          <p className="text-sm text-muted-foreground">No custom roles match the filter.</p>
        )}
        {customShown.length > 0 && <RoleTable label="Custom roles" roles={customShown} />}
      </Card>
      <Card>
        <CardTitle>Predefined roles</CardTitle>
        <QueryStatus query={predefined} />
        {predefined.data && predefinedShown.length === 0 && (
          <p className="text-sm text-muted-foreground">No predefined roles match the filter.</p>
        )}
        {predefinedShown.length > 0 && (
          <RoleTable label="Predefined roles" roles={predefinedShown} />
        )}
      </Card>
    </div>
  )
}

function RoleTable({ label, roles }: { label: string; roles: Role[] }) {
  return (
    <Table aria-label={label}>
      <TableHeader>
        <TableRow>
          <TableHead>Title</TableHead>
          <TableHead>Name</TableHead>
          <TableHead>Stage</TableHead>
          <TableHead>Permissions</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {roles.map((r) => (
          <TableRow key={r.name} data-testid="role">
            <TableCell>
              <Link
                className="text-primary underline-offset-4 hover:underline"
                to={rolePath(r.name)}
              >
                {r.title || r.name}
              </Link>
              {r.deleted && (
                <Badge variant="destructive" className="ml-2">
                  Deleted
                </Badge>
              )}
            </TableCell>
            <TableCell className="font-mono text-xs break-all">{r.name}</TableCell>
            <TableCell>{r.stage}</TableCell>
            <TableCell>{r.includedPermissions?.length ?? 0}</TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}
