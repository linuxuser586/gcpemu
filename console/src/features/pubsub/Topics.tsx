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

import { formatSeconds } from '../gcs/format'
import { lastSegment, seconds, subscriptionsQuery, topicsQuery } from './api'
import { pairs } from './format'
import { ChooseProject, ListHeader, topicPath, useProject } from './PubSubLayout'

/** Filter is the ?q= filter of a list, kept in the URL. */
export function Filter({ label, placeholder }: { label: string; placeholder: string }) {
  const [view, setView] = useViewState()
  const [text, setText] = useState(view.q ?? '')
  return (
    <form role="search" aria-label={label} onSubmit={(e) => e.preventDefault()}>
      <label htmlFor="pubsub-filter" className="flex max-w-md flex-col gap-1 text-sm">
        Filter
        <Input
          id="pubsub-filter"
          type="search"
          placeholder={placeholder}
          value={text}
          onChange={(e) => {
            setText(e.target.value)
            setView({ q: e.target.value })
          }}
        />
      </label>
    </form>
  )
}

/**
 * Topics lists the chosen Project's topics with how many of its
 * subscriptions each has. ?q= keeps those whose ID contains it.
 */
export function Topics() {
  const project = useProject()
  const [view] = useViewState()
  const query = useQuery({ ...topicsQuery(project), enabled: !!project })
  const subs = useQuery({ ...subscriptionsQuery(project), enabled: !!project })
  if (!project) return <ChooseProject what="see its topics and subscriptions" />
  const all = query.data ?? []
  const shown = all.filter((t) => lastSegment(t.name).includes(view.q ?? ''))
  const counts = new Map<string, number>()
  for (const s of subs.data ?? []) counts.set(s.topic, (counts.get(s.topic) ?? 0) + 1)

  return (
    <div className="flex flex-col gap-6">
      <ListHeader
        action={
          <Button asChild>
            <Link to="/pubsub/create-topic">
              <Plus aria-hidden />
              Create topic
            </Link>
          </Button>
        }
      />
      <Card>
        <Filter label="Filter topics" placeholder="Topic ID" />
        <QueryStatus query={query} />
        {query.data && all.length === 0 && (
          <p className="text-sm text-muted-foreground">No topics in this Project yet.</p>
        )}
        {query.data && all.length > 0 && shown.length === 0 && (
          <p className="text-sm text-muted-foreground">No topics match the filter.</p>
        )}
        {shown.length > 0 && (
          <Table aria-label="Topics">
            <TableHeader>
              <TableRow>
                <TableHead>Topic ID</TableHead>
                <TableHead>Subscriptions</TableHead>
                <TableHead>Labels</TableHead>
                <TableHead>Retention</TableHead>
                <TableHead>Schema</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((t) => {
                const id = lastSegment(t.name)
                return (
                  <TableRow key={t.name} data-testid="topic">
                    <TableCell>
                      <Link
                        className="font-mono text-primary underline-offset-4 hover:underline"
                        to={topicPath(id)}
                      >
                        {id}
                      </Link>
                    </TableCell>
                    <TableCell>{subs.data ? (counts.get(t.name) ?? 0) : ''}</TableCell>
                    <TableCell className="font-mono text-xs">{pairs(t.labels)}</TableCell>
                    <TableCell>
                      {formatSeconds(seconds(t.messageRetentionDuration)) ?? '—'}
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {t.schemaSettings?.schema ? lastSegment(t.schemaSettings.schema) : ''}
                    </TableCell>
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
