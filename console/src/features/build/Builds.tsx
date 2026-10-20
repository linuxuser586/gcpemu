import { useQuery } from '@tanstack/react-query'

import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Card } from '@/components/ui/card'
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
import { accountLabel, buildRef, buildsQuery, duration, sourceLabel } from './api'
import { BuildHeader, buildPath, ChooseProject, StatusBadge } from './BuildLayout'
import { LocationFilter, useBuildLocation } from './LocationFilter'

/**
 * Builds lists the chosen Project's Builds in one location (?location=,
 * global by default), newest first as the API answers. Build events keep
 * their status live.
 */
export function Builds() {
  const [view] = useViewState()
  const project = view.project ?? ''
  const location = useBuildLocation()
  const query = useQuery({ ...buildsQuery(project, location), enabled: !!project })
  if (!project) return <ChooseProject what="see its Builds" />
  const all = query.data ?? []

  return (
    <div className="flex flex-col gap-6">
      <BuildHeader>
        <p className="text-sm text-muted-foreground">
          Builds of Project <span className="font-mono">{project}</span> in{' '}
          <span className="font-mono">{location}</span>
        </p>
      </BuildHeader>
      <Card>
        <LocationFilter what="Builds" />
        <QueryStatus query={query} />
        {query.data && all.length === 0 && (
          <p className="text-sm text-muted-foreground">No Builds in {location} yet.</p>
        )}
        {all.length > 0 && (
          <Table aria-label="Builds">
            <TableHeader>
              <TableRow>
                <TableHead>Build</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Duration</TableHead>
                <TableHead>Source</TableHead>
                <TableHead>Build trigger</TableHead>
                <TableHead>Service account</TableHead>
                <TableHead>Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {all.map((b) => (
                <TableRow key={b.name} data-testid="build">
                  <TableCell>
                    <Link
                      className="font-mono text-primary underline-offset-4 hover:underline"
                      to={buildPath(buildRef(b))}
                    >
                      {b.id}
                    </Link>
                  </TableCell>
                  <TableCell>
                    <StatusBadge status={b.status} />
                  </TableCell>
                  <TableCell>{duration(b.startTime, b.finishTime)}</TableCell>
                  <TableCell className="font-mono text-xs break-all">
                    {sourceLabel(b.source)}
                  </TableCell>
                  <TableCell className="font-mono text-xs">{b.buildTriggerId}</TableCell>
                  <TableCell className="font-mono text-xs break-all">
                    {accountLabel(b.serviceAccount)}
                  </TableCell>
                  <TableCell>{time(b.createTime)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  )
}
