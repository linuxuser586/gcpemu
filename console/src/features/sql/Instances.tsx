import { useQuery } from '@tanstack/react-query'
import { Plus } from 'lucide-react'
import { useState } from 'react'

import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
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
import { useViewState } from '@/lib/viewState'

import { instancesQuery, ipOf } from './api'
import { InstanceStateBadge, instancePath } from './SqlLayout'

/**
 * Instances lists the chosen Project's Cloud SQL instances. ?location=
 * keeps those in a region, ?q= those whose name contains it.
 */
export function Instances() {
  const [view, setView] = useViewState()
  const project = view.project ?? ''
  const [text, setText] = useState(view.q ?? '')
  const [loc, setLoc] = useState(view.location ?? '')
  const query = useQuery(instancesQuery(project))
  const all = query.data ?? []
  const shown = all.filter(
    (i) => (!loc || i.region === loc || i.gceZone === loc) && i.name.includes(text),
  )

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Cloud SQL</h1>
          <p className="text-sm text-muted-foreground">
            Instances of Project <span className="font-mono">{project}</span>
          </p>
        </div>
        <Button asChild>
          <Link to="/sql/create">
            <Plus aria-hidden />
            Create instance
          </Link>
        </Button>
      </div>
      <Card>
        <form
          role="search"
          aria-label="Filter instances"
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => e.preventDefault()}
        >
          <label htmlFor="sql-location" className="flex flex-col gap-1 text-sm">
            Region
            <Input
              id="sql-location"
              className="w-48"
              placeholder="Region or zone"
              value={loc}
              onChange={(e) => {
                setLoc(e.target.value.trim())
                setView({ location: e.target.value.trim() })
              }}
            />
          </label>
          <label htmlFor="sql-filter" className="flex min-w-48 flex-1 flex-col gap-1 text-sm">
            Filter
            <Input
              id="sql-filter"
              type="search"
              placeholder="Instance name"
              value={text}
              onChange={(e) => {
                setText(e.target.value)
                setView({ q: e.target.value })
              }}
            />
          </label>
        </form>
        <QueryStatus query={query} />
        {query.data && all.length === 0 && (
          <p className="text-sm text-muted-foreground">No instances in this Project yet.</p>
        )}
        {query.data && all.length > 0 && shown.length === 0 && (
          <p className="text-sm text-muted-foreground">No instances match the filters.</p>
        )}
        {shown.length > 0 && (
          <Table aria-label="Instances">
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>State</TableHead>
                <TableHead>Version</TableHead>
                <TableHead>Region</TableHead>
                <TableHead>Connection name</TableHead>
                <TableHead>Public IP</TableHead>
                <TableHead>Private IP</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((i) => (
                <TableRow key={i.name} data-testid="instance">
                  <TableCell>
                    <Link
                      className="font-mono text-primary underline-offset-4 hover:underline"
                      to={instancePath(i.name)}
                    >
                      {i.name}
                    </Link>
                  </TableCell>
                  <TableCell>
                    <InstanceStateBadge instance={i} />
                  </TableCell>
                  <TableCell className="font-mono">{i.databaseVersion ?? '—'}</TableCell>
                  <TableCell className="font-mono">{i.region ?? '—'}</TableCell>
                  <TableCell className="font-mono">{i.connectionName ?? '—'}</TableCell>
                  <TableCell className="font-mono">{ipOf(i, 'PRIMARY') ?? '—'}</TableCell>
                  <TableCell className="font-mono">{ipOf(i, 'PRIVATE') ?? '—'}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  )
}
