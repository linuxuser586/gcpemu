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

import { time } from '../gcs/format'
import { lastSegment, locationsQuery, secretsQuery, type Secret } from './api'
import { ChooseProject, secretPath } from './SecretsLayout'

/** replicationLabel summarizes where a secret's payloads live. */
export function replicationLabel(s: Secret, location: string): string {
  if (location) return `Regional (${location})`
  const um = s.replication?.userManaged?.replicas
  if (um?.length) return `User-managed (${um.map((r) => r.location).join(', ')})`
  return 'Automatic'
}

/**
 * Secrets lists the chosen Project's secrets in one location: global
 * secrets by default, regional ones with ?location=. ?q= keeps those
 * whose ID contains it.
 */
export function Secrets() {
  const [view, setView] = useViewState()
  const project = view.project ?? ''
  const location = view.location ?? ''
  const [text, setText] = useState(view.q ?? '')
  const query = useQuery({ ...secretsQuery(project, location), enabled: !!project })
  const locations = useQuery({ ...locationsQuery(project), enabled: !!project })
  if (!project) return <ChooseProject what="see its secrets" />
  const all = query.data ?? []
  const shown = all.filter((s) => lastSegment(s.name).includes(text))

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Secret Manager</h1>
          <p className="text-sm text-muted-foreground">
            {location ? 'Regional secrets' : 'Global secrets'} of Project{' '}
            <span className="font-mono">{project}</span>
            {location && (
              <>
                {' '}
                in <span className="font-mono">{location}</span>
              </>
            )}
          </p>
        </div>
        <Button asChild>
          <Link to="/secrets/create">
            <Plus aria-hidden />
            Create secret
          </Link>
        </Button>
      </div>
      <Card>
        <form
          role="search"
          aria-label="Filter secrets"
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => e.preventDefault()}
        >
          <label htmlFor="secrets-location" className="flex flex-col gap-1 text-sm">
            Location
            <NativeSelect
              id="secrets-location"
              className="w-56"
              value={location}
              onChange={(e) => setView({ location: e.target.value })}
            >
              <option value="">Global</option>
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
          <label htmlFor="secrets-filter" className="flex min-w-48 flex-1 flex-col gap-1 text-sm">
            Filter
            <Input
              id="secrets-filter"
              type="search"
              placeholder="Secret ID"
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
            {location ? `No secrets in ${location} yet.` : 'No global secrets in this Project yet.'}
          </p>
        )}
        {query.data && all.length > 0 && shown.length === 0 && (
          <p className="text-sm text-muted-foreground">No secrets match the filter.</p>
        )}
        {shown.length > 0 && (
          <Table aria-label="Secrets">
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Replication</TableHead>
                <TableHead>Labels</TableHead>
                <TableHead>Expires</TableHead>
                <TableHead>Next rotation</TableHead>
                <TableHead>Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((s) => {
                const id = lastSegment(s.name)
                return (
                  <TableRow key={s.name} data-testid="secret">
                    <TableCell>
                      <Link
                        className="font-mono text-primary underline-offset-4 hover:underline"
                        to={secretPath({ location, secret: id })}
                      >
                        {id}
                      </Link>
                    </TableCell>
                    <TableCell>{replicationLabel(s, location)}</TableCell>
                    <TableCell className="font-mono text-xs">
                      {Object.entries(s.labels ?? {})
                        .map(([k, v]) => `${k}=${v}`)
                        .join(', ')}
                    </TableCell>
                    <TableCell>{time(s.expireTime) ?? 'Never'}</TableCell>
                    <TableCell>{time(s.rotation?.nextRotationTime) ?? '—'}</TableCell>
                    <TableCell>{time(s.createTime)}</TableCell>
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
