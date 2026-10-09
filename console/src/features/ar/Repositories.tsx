import { useQuery } from '@tanstack/react-query'
import { Plus } from 'lucide-react'
import { useState } from 'react'

import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
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
import { useViewState } from '@/lib/viewState'

import { formatBytes, time } from '../gcs/format'
import { lastSegment, locationsQuery, repositoriesQuery, type Repository } from './api'
import { ChooseProject, repoPath } from './ArLayout'

/** MODE_LABEL names a repository mode for people. */
export const MODE_LABEL: Record<string, string> = {
  STANDARD_REPOSITORY: 'Standard',
  REMOTE_REPOSITORY: 'Remote',
  VIRTUAL_REPOSITORY: 'Virtual',
}

/** repoLocation is the location a repository's name is in. */
export const repoLocation = (r: Repository) => r.name.split('/')[3] ?? ''

/**
 * Repositories lists the chosen Project's repositories: in every location
 * by default, in one with ?location=. ?q= keeps those whose ID contains
 * it.
 */
export function Repositories() {
  const [view, setView] = useViewState()
  const project = view.project ?? ''
  const location = view.location ?? ''
  const [text, setText] = useState(view.q ?? '')
  const query = useQuery({ ...repositoriesQuery(project, location), enabled: !!project })
  const locations = useQuery({ ...locationsQuery(project), enabled: !!project })
  if (!project) return <ChooseProject what="see its repositories" />
  const all = query.data ?? []
  const shown = all.filter((r) => lastSegment(r.name).includes(text))

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Artifact Registry</h1>
          <p className="text-sm text-muted-foreground">
            Repositories of Project <span className="font-mono">{project}</span>
            {location ? (
              <>
                {' '}
                in <span className="font-mono">{location}</span>
              </>
            ) : (
              ' in every location'
            )}
          </p>
        </div>
        <Button asChild>
          <Link to="/ar/create">
            <Plus aria-hidden />
            Create repository
          </Link>
        </Button>
      </div>
      <Card>
        <form
          role="search"
          aria-label="Filter repositories"
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => e.preventDefault()}
        >
          <label htmlFor="ar-location" className="flex flex-col gap-1 text-sm">
            Location
            <NativeSelect
              id="ar-location"
              className="w-56"
              value={location}
              onChange={(e) => setView({ location: e.target.value })}
            >
              <option value="">All locations</option>
              {location && !locations.data?.some((l) => l.locationId === location) && (
                <option value={location}>{location}</option>
              )}
              {(locations.data ?? []).map((l) => (
                <option key={l.locationId} value={l.locationId}>
                  {l.locationId}
                  {l.displayName ? ` (${l.displayName})` : ''}
                </option>
              ))}
            </NativeSelect>
          </label>
          <label htmlFor="ar-filter" className="flex min-w-48 flex-1 flex-col gap-1 text-sm">
            Filter
            <Input
              id="ar-filter"
              type="search"
              placeholder="Repository ID"
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
          <p className="text-sm text-muted-foreground">
            {location
              ? `No repositories in ${location} yet.`
              : 'No repositories in this Project yet.'}
          </p>
        )}
        {query.data && all.length > 0 && shown.length === 0 && (
          <p className="text-sm text-muted-foreground">No repositories match the filter.</p>
        )}
        {shown.length > 0 && (
          <Table aria-label="Repositories">
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Location</TableHead>
                <TableHead>Format</TableHead>
                <TableHead>Mode</TableHead>
                <TableHead>Description</TableHead>
                <TableHead>Labels</TableHead>
                <TableHead className="text-right">Size</TableHead>
                <TableHead>Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((r) => {
                const id = lastSegment(r.name)
                const loc = repoLocation(r)
                return (
                  <TableRow key={r.name} data-testid="repository">
                    <TableCell>
                      <Link
                        className="font-mono text-primary underline-offset-4 hover:underline"
                        to={repoPath({ location: loc, repo: id })}
                      >
                        {id}
                      </Link>
                    </TableCell>
                    <TableCell className="font-mono">{loc}</TableCell>
                    <TableCell>{r.format === 'DOCKER' ? 'Docker' : r.format}</TableCell>
                    <TableCell>{MODE_LABEL[r.mode ?? ''] ?? r.mode}</TableCell>
                    <TableCell>{r.description}</TableCell>
                    <TableCell className="font-mono text-xs">
                      {Object.entries(r.labels ?? {})
                        .map(([k, v]) => `${k}=${v}`)
                        .join(', ')}
                    </TableCell>
                    <TableCell className="text-right">{formatBytes(r.sizeBytes ?? 0)}</TableCell>
                    <TableCell>{time(r.createTime)}</TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  )
}
