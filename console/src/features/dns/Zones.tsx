import { useQuery } from '@tanstack/react-query'
import { Plus } from 'lucide-react'
import { useState } from 'react'

import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Badge } from '@/components/ui/badge'
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

import { time } from '../gcs/format'
import { networkLabel, zoneNetworks, zonesQuery, type ManagedZone } from './api'
import { ChooseProject, DnsNav, zonePath } from './DnsLayout'

/** VisibilityBadge says whether a zone is public or private. */
export function VisibilityBadge({ z }: { z: ManagedZone }) {
  return z.visibility === 'private' ? (
    <Badge variant="warning">Private</Badge>
  ) : (
    <Badge variant="outline">Public</Badge>
  )
}

/** Networks lists the networks a private zone is bound to. */
export function Networks({ z, project }: { z: ManagedZone; project: string }) {
  const nets = zoneNetworks(z)
  if (z.visibility !== 'private') return <span className="text-muted-foreground">—</span>
  if (nets.length === 0) return <span className="text-muted-foreground">None</span>
  return (
    <span className="font-mono text-xs">
      {nets.map((n) => networkLabel(n, project)).join(', ')}
    </span>
  )
}

/**
 * Zones lists the chosen Project's managed zones with their network
 * bindings. ?q= keeps those whose name or DNS name contains it.
 */
export function Zones() {
  const [view, setView] = useViewState()
  const project = view.project ?? ''
  const [text, setText] = useState(view.q ?? '')
  const query = useQuery({ ...zonesQuery(project), enabled: !!project })
  if (!project) return <ChooseProject what="see its managed zones" />
  const all = query.data ?? []
  const shown = all.filter((z) => z.name.includes(text) || z.dnsName.includes(text))

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Cloud DNS</h1>
          <p className="text-sm text-muted-foreground">
            Managed zones of Project <span className="font-mono">{project}</span>
          </p>
        </div>
        <Button asChild>
          <Link to="/dns/create">
            <Plus aria-hidden />
            Create zone
          </Link>
        </Button>
      </div>
      <DnsNav />
      <Card>
        <form
          role="search"
          aria-label="Filter zones"
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => e.preventDefault()}
        >
          <label htmlFor="zones-filter" className="flex min-w-48 flex-1 flex-col gap-1 text-sm">
            Filter
            <Input
              id="zones-filter"
              type="search"
              placeholder="Zone name or DNS name"
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
          <p className="text-sm text-muted-foreground">No managed zones in this Project yet.</p>
        )}
        {query.data && all.length > 0 && shown.length === 0 && (
          <p className="text-sm text-muted-foreground">No zones match the filter.</p>
        )}
        {shown.length > 0 && (
          <Table aria-label="Zones">
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>DNS name</TableHead>
                <TableHead>Visibility</TableHead>
                <TableHead>Networks</TableHead>
                <TableHead>Description</TableHead>
                <TableHead>Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((z) => (
                <TableRow key={z.name} data-testid="zone">
                  <TableCell>
                    <Link
                      className="font-mono text-primary underline-offset-4 hover:underline"
                      to={zonePath(z.name)}
                    >
                      {z.name}
                    </Link>
                  </TableCell>
                  <TableCell className="font-mono">{z.dnsName}</TableCell>
                  <TableCell>
                    <VisibilityBadge z={z} />
                  </TableCell>
                  <TableCell>
                    <Networks z={z} project={project} />
                  </TableCell>
                  <TableCell>{z.description}</TableCell>
                  <TableCell>{time(z.creationTime)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  )
}
