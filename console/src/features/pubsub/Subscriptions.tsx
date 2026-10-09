import { useQuery } from '@tanstack/react-query'
import { Plus } from 'lucide-react'

import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
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

import { formatBytes } from '../gcs/format'
import {
  DELETED_TOPIC,
  lastSegment,
  projectOf,
  statsQuery,
  subscriptionsQuery,
  type Subscription,
} from './api'
import { oldestAge } from './format'
import { ChooseProject, ListHeader, subscriptionPath, topicPath, useProject } from './PubSubLayout'
import { Filter } from './Topics'

/** deliveryLabel says how a subscription delivers its messages. */
export const deliveryLabel = (s: Subscription) => (s.pushConfig?.pushEndpoint ? 'Push' : 'Pull')

/** TopicLink links a topic of the chosen Project, and names any other. */
export function TopicLink({ name, project }: { name: string; project: string }) {
  if (name === DELETED_TOPIC) return <span className="text-muted-foreground">Deleted topic</span>
  if (projectOf(name) !== project) return <span className="font-mono text-xs">{name}</span>
  return (
    <Link
      className="font-mono text-primary underline-offset-4 hover:underline"
      to={topicPath(lastSegment(name))}
    >
      {lastSegment(name)}
    </Link>
  )
}

/**
 * SubscriptionTable lists subscriptions by name with their backlog: those
 * of other Projects (attached to a topic of this one) are named only.
 */
export function SubscriptionTable({
  names,
  subs,
  showTopic,
}: {
  names: string[]
  subs: Subscription[]
  showTopic: boolean
}) {
  const project = useProject()
  const stats = useQuery({ ...statsQuery(project), enabled: !!project })
  const byName = new Map(subs.map((s) => [s.name, s]))
  return (
    <Table aria-label="Subscriptions">
      <TableHeader>
        <TableRow>
          <TableHead>Subscription ID</TableHead>
          {showTopic && <TableHead>Topic</TableHead>}
          <TableHead>Delivery</TableHead>
          <TableHead>Backlog</TableHead>
          <TableHead>Oldest unacked</TableHead>
          <TableHead>Dead-lettered</TableHead>
          <TableHead>Ack deadline</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {names.map((name) => {
          const s = byName.get(name)
          const st = stats.data?.byName.get(name)
          const id = lastSegment(name)
          return (
            <TableRow key={name} data-testid="subscription">
              <TableCell>
                {projectOf(name) === project ? (
                  <Link
                    className="font-mono text-primary underline-offset-4 hover:underline"
                    to={subscriptionPath(id)}
                  >
                    {id}
                  </Link>
                ) : (
                  <span className="font-mono text-xs">{name}</span>
                )}
                {s?.detached && (
                  <Badge variant="warning" className="ml-2">
                    Detached
                  </Badge>
                )}
              </TableCell>
              {showTopic && (
                <TableCell>{s && <TopicLink name={s.topic} project={project} />}</TableCell>
              )}
              <TableCell>{s && deliveryLabel(s)}</TableCell>
              <TableCell>
                {st && (
                  <>
                    {st.backlog}{' '}
                    <span className="text-xs text-muted-foreground">
                      ({formatBytes(st.backlogBytes)})
                    </span>
                  </>
                )}
              </TableCell>
              <TableCell>{st && stats.data && (oldestAge(st, stats.data.now) ?? '—')}</TableCell>
              <TableCell>
                {st && (s?.deadLetterPolicy?.deadLetterTopic ? st.deadLettered : '—')}
              </TableCell>
              <TableCell>
                {s?.ackDeadlineSeconds !== undefined && `${s.ackDeadlineSeconds} s`}
              </TableCell>
            </TableRow>
          )
        })}
      </TableBody>
    </Table>
  )
}

/**
 * Subscriptions lists the chosen Project's subscriptions with their
 * backlog, the age of their oldest unacked message and how many messages
 * they dead-lettered. ?q= keeps those whose ID contains it.
 */
export function Subscriptions() {
  const project = useProject()
  const [view] = useViewState()
  const query = useQuery({ ...subscriptionsQuery(project), enabled: !!project })
  if (!project) return <ChooseProject what="see its topics and subscriptions" />
  const all = query.data ?? []
  const shown = all.filter((s) => lastSegment(s.name).includes(view.q ?? ''))

  return (
    <div className="flex flex-col gap-6">
      <ListHeader
        action={
          <Button asChild>
            <Link to="/pubsub/create-subscription">
              <Plus aria-hidden />
              Create subscription
            </Link>
          </Button>
        }
      />
      <Card>
        <Filter label="Filter subscriptions" placeholder="Subscription ID" />
        <QueryStatus query={query} />
        {query.data && all.length === 0 && (
          <p className="text-sm text-muted-foreground">No subscriptions in this Project yet.</p>
        )}
        {query.data && all.length > 0 && shown.length === 0 && (
          <p className="text-sm text-muted-foreground">No subscriptions match the filter.</p>
        )}
        {shown.length > 0 && (
          <SubscriptionTable names={shown.map((s) => s.name)} subs={all} showTopic />
        )}
      </Card>
    </div>
  )
}
