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

import { clustersQuery, type Cluster } from './api'
import { ClusterStatusBadge } from './format'
import { clusterPath } from './GkeLayout'

/** inLocation reports whether a cluster is in location: itself or its region. */
export function inLocation(c: Cluster, location?: string): boolean {
  if (!location) return true
  return c.location === location || c.location.startsWith(`${location}-`)
}

/**
 * Clusters lists the chosen Project's clusters. ?location= keeps those in
 * a zone or region, ?q= those whose name contains it.
 */
export function Clusters() {
  const [view, setView] = useViewState()
  const project = view.project ?? ''
  const [text, setText] = useState(view.q ?? '')
  const [loc, setLoc] = useState(view.location ?? '')
  const query = useQuery(clustersQuery(project))
  const all = query.data ?? []
  const shown = all.filter((c) => inLocation(c, loc) && c.name.includes(text))

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Google Kubernetes Engine</h1>
          <p className="text-sm text-muted-foreground">
            Clusters of Project <span className="font-mono">{project}</span>
          </p>
        </div>
        <Button asChild>
          <Link to="/gke/create">
            <Plus aria-hidden />
            Create cluster
          </Link>
        </Button>
      </div>
      <Card>
        <form
          role="search"
          aria-label="Filter clusters"
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => e.preventDefault()}
        >
          <label htmlFor="gke-location" className="flex flex-col gap-1 text-sm">
            Location
            <Input
              id="gke-location"
              className="w-48"
              placeholder="Zone or region"
              value={loc}
              onChange={(e) => {
                setLoc(e.target.value.trim())
                setView({ location: e.target.value.trim() })
              }}
            />
          </label>
          <label htmlFor="gke-filter" className="flex min-w-48 flex-1 flex-col gap-1 text-sm">
            Filter
            <Input
              id="gke-filter"
              type="search"
              placeholder="Cluster name"
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
          <p className="text-sm text-muted-foreground">No clusters in this Project yet.</p>
        )}
        {query.data && all.length > 0 && shown.length === 0 && (
          <p className="text-sm text-muted-foreground">No clusters match the filters.</p>
        )}
        {shown.length > 0 && (
          <Table aria-label="Clusters">
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Location</TableHead>
                <TableHead>Version</TableHead>
                <TableHead className="text-right">Nodes</TableHead>
                <TableHead>Endpoint</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((c) => (
                <TableRow key={`${c.location}/${c.name}`} data-testid="cluster">
                  <TableCell>
                    <Link
                      className="font-mono text-primary underline-offset-4 hover:underline"
                      to={clusterPath(c.location, c.name)}
                    >
                      {c.name}
                    </Link>
                  </TableCell>
                  <TableCell>
                    <ClusterStatusBadge status={c.status} />
                  </TableCell>
                  <TableCell className="font-mono">{c.location}</TableCell>
                  <TableCell className="font-mono">{c.currentMasterVersion ?? '—'}</TableCell>
                  <TableCell className="text-right tabular-nums">
                    {c.currentNodeCount ?? 0}
                  </TableCell>
                  <TableCell className="font-mono">{c.endpoint ?? '—'}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  )
}
