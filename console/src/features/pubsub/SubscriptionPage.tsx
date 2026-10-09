import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Pencil, Trash2 } from 'lucide-react'
import { Outlet, useOutletContext, useParams } from 'react-router'

import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import { toast } from '@/lib/toast'

import { formatBytes, formatSeconds } from '../gcs/format'
import {
  deleteSubscription,
  seconds,
  statsQuery,
  subscriptionQuery,
  type Subscription,
} from './api'
import { oldestAge, pairs } from './format'
import { ChooseProject, Tabs, subscriptionPath, useProject } from './PubSubLayout'
import { deliveryLabel, TopicLink } from './Subscriptions'

/**
 * SubscriptionPage is one subscription: its backlog, edit and delete
 * above tabs for its messages and its configuration.
 */
export function SubscriptionPage() {
  const project = useProject()
  const { subscription: id = '' } = useParams()
  const query = useQuery({ ...subscriptionQuery(project, id), enabled: !!project })
  const s = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const del = useMutation({
    mutationFn: () => deleteSubscription(project, id),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['pubsub'] })
      toast.success(`Deleted subscription ${id}.`)
      navigate('/pubsub/subscriptions')
    },
  })
  if (!project) return <ChooseProject what="see its subscriptions" />

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to="/pubsub/subscriptions" className="underline-offset-4 hover:underline">
              Subscriptions
            </Link>{' '}
            /
          </p>
          <h1 className="flex items-center gap-2 text-2xl font-semibold">
            <span className="font-mono">{id}</span>
            {s?.detached && <Badge variant="warning">Detached</Badge>}
          </h1>
        </div>
        {s && (
          <div className="flex flex-wrap gap-2">
            <Button variant="outline" asChild>
              <Link to={subscriptionPath(id, 'edit')}>
                <Pencil aria-hidden />
                Edit subscription
              </Link>
            </Button>
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Delete subscription
                </Button>
              }
              title={`Delete subscription ${id}?`}
              description="Its backlog of unacknowledged messages is deleted with it. This cannot be undone."
              confirmLabel="Delete subscription"
              onConfirm={() => del.mutateAsync()}
            />
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {s && (
        <>
          <Backlog s={s} />
          <Tabs
            label="Subscription"
            base={subscriptionPath(id)}
            tabs={[
              ['', 'Messages'],
              ['overview', 'Overview'],
            ]}
          />
          <Outlet context={s} />
        </>
      )}
    </div>
  )
}

/** useSubscription is the subscription a SubscriptionPage tab belongs to. */
export const useSubscription = () => useOutletContext<Subscription>()

/**
 * Backlog shows what GCP reports as the subscription's metrics: unacked
 * messages, the oldest one's age, messages leased now and dead-lettered.
 */
function Backlog({ s }: { s: Subscription }) {
  const project = useProject()
  const stats = useQuery(statsQuery(project))
  const st = stats.data?.byName.get(s.name)
  const tile = (label: string, value: string | number | undefined, hint?: string) => (
    <div className="flex flex-col gap-0.5">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="text-xl font-semibold tabular-nums">{value ?? '—'}</dd>
      {hint && <dd className="text-xs text-muted-foreground">{hint}</dd>}
    </div>
  )
  return (
    <Card>
      <QueryStatus query={stats} rows={1} />
      {st && stats.data && (
        <dl aria-label="Backlog" className="grid grid-cols-2 gap-4 sm:grid-cols-4">
          {tile('Unacked messages', st.backlog, formatBytes(st.backlogBytes))}
          {tile('Oldest unacked message', oldestAge(st, stats.data.now))}
          {tile('Outstanding', st.outstanding, 'Leased to subscribers now')}
          {tile(
            'Dead-lettered',
            s.deadLetterPolicy?.deadLetterTopic ? st.deadLettered : undefined,
            s.deadLetterPolicy?.deadLetterTopic
              ? 'Since the Instance started'
              : 'No dead-letter topic',
          )}
        </dl>
      )}
    </Card>
  )
}

/** SubscriptionOverview shows the subscription's configuration and the resource as JSON. */
export function SubscriptionOverview() {
  const s = useSubscription()
  const project = useProject()
  const push = s.pushConfig
  const dl = s.deadLetterPolicy
  const rp = s.retryPolicy
  const exp = s.expirationPolicy
  return (
    <div className="flex flex-col gap-6">
      <Card>
        <CardTitle>Configuration</CardTitle>
        <DetailList
          label="Subscription details"
          rows={[
            ['Name', <Mono key="n">{s.name}</Mono>],
            ['Topic', <TopicLink key="t" name={s.topic} project={project} />],
            ['Delivery', deliveryLabel(s)],
            ['Push endpoint', push?.pushEndpoint && <Mono>{push.pushEndpoint}</Mono>],
            [
              'OIDC token',
              push?.oidcToken?.serviceAccountEmail && (
                <Mono>
                  {push.oidcToken.serviceAccountEmail}
                  {push.oidcToken.audience ? ` (audience ${push.oidcToken.audience})` : ''}
                </Mono>
              ),
            ],
            ['Payload', push?.pushEndpoint && (push.noWrapper ? 'Unwrapped' : 'Wrapped')],
            [
              'Acknowledgement deadline',
              s.ackDeadlineSeconds !== undefined && `${s.ackDeadlineSeconds} s`,
            ],
            ['Message retention', formatSeconds(seconds(s.messageRetentionDuration))],
            ['Retain acknowledged messages', s.retainAckedMessages ? 'Yes' : 'No'],
            ['Message ordering', s.enableMessageOrdering ? 'Enabled' : 'Disabled'],
            ['Exactly-once delivery', s.enableExactlyOnceDelivery ? 'Enabled' : 'Disabled'],
            ['Filter', s.filter && <Mono>{s.filter}</Mono>],
            [
              'Dead-letter topic',
              dl?.deadLetterTopic && (
                <Mono>
                  {dl.deadLetterTopic} after {dl.maxDeliveryAttempts ?? 5} delivery attempts
                </Mono>
              ),
            ],
            [
              'Retry policy',
              rp
                ? `Exponential backoff, ${formatSeconds(seconds(rp.minimumBackoff)) ?? '10 s'} to ${formatSeconds(seconds(rp.maximumBackoff)) ?? '600 s'}`
                : 'Retry immediately',
            ],
            [
              'Expiration',
              exp && !exp.ttl
                ? 'Never'
                : `After ${formatSeconds(seconds(exp?.ttl)) ?? '31 days'} of inactivity`,
            ],
            ['Labels', pairs(s.labels) && <Mono>{pairs(s.labels)}</Mono>],
            ['State', s.state && s.state !== 'STATE_UNSPECIFIED' ? s.state : undefined],
          ]}
        />
      </Card>
      <Card>
        <CardTitle>Subscription resource</CardTitle>
        <JsonView value={s} label="Subscription JSON" />
      </Card>
    </div>
  )
}
