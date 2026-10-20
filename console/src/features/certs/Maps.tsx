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

import { time } from '../gcs/format'
import { lastSegment, mapsQuery } from './api'
import { CertsNav, ChooseProject, mapPath } from './CertsLayout'

/**
 * Maps lists the chosen Project's certificate maps, which are all global,
 * with how many target proxies serve each. ?q= keeps those whose ID
 * contains it.
 */
export function Maps() {
  const [view, setView] = useViewState()
  const project = view.project ?? ''
  const [text, setText] = useState(view.q ?? '')
  const query = useQuery({ ...mapsQuery(project), enabled: !!project })
  if (!project) return <ChooseProject what="see its certificate maps" />
  const all = query.data ?? []
  const shown = all.filter((m) => lastSegment(m.name).includes(text))

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Certificate Manager</h1>
          <p className="text-sm text-muted-foreground">
            Certificate maps of Project <span className="font-mono">{project}</span>
          </p>
        </div>
        <Button asChild>
          <Link to="/certs/maps/create">
            <Plus aria-hidden />
            Create certificate map
          </Link>
        </Button>
      </div>
      <CertsNav />
      <Card>
        <form
          role="search"
          aria-label="Filter certificate maps"
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => e.preventDefault()}
        >
          <label htmlFor="maps-filter" className="flex min-w-48 flex-1 flex-col gap-1 text-sm">
            Filter
            <Input
              id="maps-filter"
              type="search"
              placeholder="Certificate map ID"
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
          <p className="text-sm text-muted-foreground">No certificate maps in this Project yet.</p>
        )}
        {query.data && all.length > 0 && shown.length === 0 && (
          <p className="text-sm text-muted-foreground">No certificate maps match the filter.</p>
        )}
        {shown.length > 0 && (
          <Table aria-label="Certificate maps">
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Description</TableHead>
                <TableHead>Target proxies</TableHead>
                <TableHead>Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((m) => {
                const id = lastSegment(m.name)
                return (
                  <TableRow key={m.name} data-testid="certificate-map">
                    <TableCell>
                      <Link
                        className="font-mono text-primary underline-offset-4 hover:underline"
                        to={mapPath({ map: id })}
                      >
                        {id}
                      </Link>
                    </TableCell>
                    <TableCell>{m.description}</TableCell>
                    <TableCell>{m.gclbTargets?.length ?? 0}</TableCell>
                    <TableCell>{time(m.createTime) ?? '—'}</TableCell>
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
