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

import { bucketsQuery, type Bucket } from './api'
import { time } from './format'
import { browsePath, ChooseProject } from './GcsLayout'

/** inLocation reports whether a bucket is in location, ignoring case. */
export function inLocation(b: Bucket, location?: string): boolean {
  if (!location) return true
  return (b.location ?? '').toLowerCase().startsWith(location.toLowerCase())
}

/**
 * Buckets lists the chosen Project's buckets. ?location= keeps those in
 * a location, ?q= those whose name contains it.
 */
export function Buckets() {
  const [view, setView] = useViewState()
  const project = view.project ?? ''
  const [text, setText] = useState(view.q ?? '')
  const [loc, setLoc] = useState(view.location ?? '')
  const query = useQuery({ ...bucketsQuery(project), enabled: !!project })
  if (!project) return <ChooseProject what="see its buckets" />
  const all = query.data ?? []
  const shown = all.filter((b) => inLocation(b, loc) && b.name.includes(text))

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Cloud Storage</h1>
          <p className="text-sm text-muted-foreground">
            Buckets of Project <span className="font-mono">{project}</span>
          </p>
        </div>
        <Button asChild>
          <Link to="/gcs/create">
            <Plus aria-hidden />
            Create bucket
          </Link>
        </Button>
      </div>
      <Card>
        <form
          role="search"
          aria-label="Filter buckets"
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => e.preventDefault()}
        >
          <label htmlFor="gcs-location" className="flex flex-col gap-1 text-sm">
            Location
            <Input
              id="gcs-location"
              className="w-48"
              placeholder="e.g. US or us-east1"
              value={loc}
              onChange={(e) => {
                setLoc(e.target.value.trim())
                setView({ location: e.target.value.trim() })
              }}
            />
          </label>
          <label htmlFor="gcs-filter" className="flex min-w-48 flex-1 flex-col gap-1 text-sm">
            Filter
            <Input
              id="gcs-filter"
              type="search"
              placeholder="Bucket name"
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
          <p className="text-sm text-muted-foreground">No buckets in this Project yet.</p>
        )}
        {query.data && all.length > 0 && shown.length === 0 && (
          <p className="text-sm text-muted-foreground">No buckets match the filters.</p>
        )}
        {shown.length > 0 && (
          <Table aria-label="Buckets">
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Location</TableHead>
                <TableHead>Storage class</TableHead>
                <TableHead>Versioning</TableHead>
                <TableHead>Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((b) => (
                <TableRow key={b.name} data-testid="bucket">
                  <TableCell>
                    <Link
                      className="font-mono text-primary underline-offset-4 hover:underline"
                      to={browsePath(b.name)}
                    >
                      {b.name}
                    </Link>
                  </TableCell>
                  <TableCell className="font-mono">
                    {b.location}
                    {b.locationType && (
                      <span className="text-muted-foreground"> ({b.locationType})</span>
                    )}
                  </TableCell>
                  <TableCell>{b.storageClass}</TableCell>
                  <TableCell>{b.versioning?.enabled ? 'On' : 'Off'}</TableCell>
                  <TableCell>{time(b.timeCreated)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  )
}
