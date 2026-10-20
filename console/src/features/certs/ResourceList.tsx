import { useQuery, type UseQueryOptions } from '@tanstack/react-query'
import { Plus } from 'lucide-react'
import { useState, type ReactNode } from 'react'

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
import { lastSegment, locationOf } from './api'
import { CertsNav, ChooseProject } from './CertsLayout'

/** Column is a list column: its heading and each resource's cell. */
export type Column<T> = [head: string, cell: (r: T) => ReactNode]

interface Listed {
  name: string
  description?: string
  createTime?: string
}

/**
 * ResourceList lists the chosen Project's resources of one Certificate
 * Manager kind in every location: name, location, the kind's own columns,
 * description and creation time. ?q= keeps those whose ID contains it.
 * singular names the kind in lower case and title in the plural.
 */
export function ResourceList<T extends Listed>({
  title,
  singular,
  query: queryOf,
  columns,
  createPath,
  pathOf,
  testId,
}: {
  title: string
  singular: string
  query: (project: string) => UseQueryOptions<T[], Error, T[], string[]>
  columns: Column<T>[]
  createPath: string
  pathOf: (r: { location: string; id: string }) => string
  testId: string
}) {
  const [view, setView] = useViewState()
  const project = view.project ?? ''
  const [text, setText] = useState(view.q ?? '')
  const query = useQuery({ ...queryOf(project), enabled: !!project })
  const plural = title.charAt(0).toLowerCase() + title.slice(1)
  if (!project) return <ChooseProject what={`see its ${plural}`} />
  const all = query.data ?? []
  const shown = all.filter((r) => lastSegment(r.name).includes(text))
  const article = /^[aeiou]/i.test(singular) ? 'an' : 'a'

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Certificate Manager</h1>
          <p className="text-sm text-muted-foreground">
            {title} of Project <span className="font-mono">{project}</span>
          </p>
        </div>
        <Button asChild>
          <Link to={createPath}>
            <Plus aria-hidden />
            Create {singular}
          </Link>
        </Button>
      </div>
      <CertsNav />
      <Card>
        <form
          role="search"
          aria-label={`Filter ${plural}`}
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => e.preventDefault()}
        >
          <label
            htmlFor={`${testId}-filter`}
            className="flex min-w-48 flex-1 flex-col gap-1 text-sm"
          >
            Filter
            <Input
              id={`${testId}-filter`}
              type="search"
              placeholder={`ID of ${article} ${singular}`}
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
          <p className="text-sm text-muted-foreground">No {plural} in this Project yet.</p>
        )}
        {query.data && all.length > 0 && shown.length === 0 && (
          <p className="text-sm text-muted-foreground">No {plural} match the filter.</p>
        )}
        {shown.length > 0 && (
          <Table aria-label={title}>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Location</TableHead>
                {columns.map(([head]) => (
                  <TableHead key={head}>{head}</TableHead>
                ))}
                <TableHead>Description</TableHead>
                <TableHead>Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((r) => {
                const id = lastSegment(r.name)
                const location = locationOf(r.name)
                return (
                  <TableRow key={r.name} data-testid={testId}>
                    <TableCell>
                      <Link
                        className="font-mono text-primary underline-offset-4 hover:underline"
                        to={pathOf({ location, id })}
                      >
                        {id}
                      </Link>
                    </TableCell>
                    <TableCell className="font-mono">{location}</TableCell>
                    {columns.map(([head, cell]) => (
                      <TableCell key={head}>{cell(r)}</TableCell>
                    ))}
                    <TableCell>{r.description}</TableCell>
                    <TableCell>{time(r.createTime) ?? '—'}</TableCell>
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
