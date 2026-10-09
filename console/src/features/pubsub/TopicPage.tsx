import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Pencil, Plus, Trash2 } from 'lucide-react'
import { Outlet, useOutletContext, useParams } from 'react-router'

import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import { toast } from '@/lib/toast'

import { formatSeconds } from '../gcs/format'
import {
  deleteTopic,
  seconds,
  subscriptionsQuery,
  topicQuery,
  topicSubscriptionsQuery,
  type Topic,
} from './api'
import { pairs } from './format'
import { Publish } from './Publish'
import { ChooseProject, Tabs, topicPath, useProject } from './PubSubLayout'
import { SubscriptionTable } from './Subscriptions'

/**
 * TopicPage is one topic: publish, create a subscription to it, edit and
 * delete above tabs for its subscriptions and its configuration.
 */
export function TopicPage() {
  const project = useProject()
  const { topic = '' } = useParams()
  const query = useQuery({ ...topicQuery(project, topic), enabled: !!project })
  const t = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const del = useMutation({
    mutationFn: () => deleteTopic(project, topic),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['pubsub'] })
      toast.success(`Deleted topic ${topic}.`)
      navigate('/pubsub')
    },
  })
  if (!project) return <ChooseProject what="see its topics" />
  const base = topicPath(topic)

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to="/pubsub" className="underline-offset-4 hover:underline">
              Topics
            </Link>{' '}
            /
          </p>
          <h1 className="text-2xl font-semibold">
            <span className="font-mono">{topic}</span>
          </h1>
        </div>
        {t && (
          <div className="flex flex-wrap gap-2">
            <Publish project={project} topic={topic} />
            <Button variant="outline" asChild>
              <Link to={`/pubsub/create-subscription?${new URLSearchParams({ topic })}`}>
                <Plus aria-hidden />
                Create subscription
              </Link>
            </Button>
            <Button variant="outline" asChild>
              <Link to={topicPath(topic, 'edit')}>
                <Pencil aria-hidden />
                Edit topic
              </Link>
            </Button>
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Delete topic
                </Button>
              }
              title={`Delete topic ${topic}?`}
              description="Its subscriptions are kept, but receive no more messages: their topic becomes _deleted-topic_. This cannot be undone."
              confirmLabel="Delete topic"
              onConfirm={() => del.mutateAsync()}
            />
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {t && (
        <>
          <Tabs
            label="Topic"
            base={base}
            tabs={[
              ['', 'Subscriptions'],
              ['overview', 'Overview'],
            ]}
          />
          <Outlet context={t} />
        </>
      )}
    </div>
  )
}

/** useTopic is the topic a TopicPage tab belongs to. */
export const useTopic = () => useOutletContext<Topic>()

/** TopicSubscriptions lists the subscriptions attached to the topic, in any Project. */
export function TopicSubscriptions() {
  const project = useProject()
  const { topic = '' } = useParams()
  const names = useQuery(topicSubscriptionsQuery(project, topic))
  const subs = useQuery(subscriptionsQuery(project))
  return (
    <Card>
      <CardTitle>Subscriptions</CardTitle>
      <QueryStatus query={names} />
      {names.data?.length === 0 && (
        <p className="text-sm text-muted-foreground">
          No subscriptions yet: messages published now reach no one.
        </p>
      )}
      {!!names.data?.length && (
        <SubscriptionTable names={names.data} subs={subs.data ?? []} showTopic={false} />
      )}
    </Card>
  )
}

/** TopicOverview shows the topic's configuration and the resource as JSON. */
export function TopicOverview() {
  const t = useTopic()
  return (
    <div className="flex flex-col gap-6">
      <Card>
        <CardTitle>Configuration</CardTitle>
        <DetailList
          label="Topic details"
          rows={[
            ['Name', <Mono key="n">{t.name}</Mono>],
            ['Labels', pairs(t.labels) && <Mono>{pairs(t.labels)}</Mono>],
            ['Message retention', formatSeconds(seconds(t.messageRetentionDuration)) ?? 'None'],
            ['Schema', t.schemaSettings?.schema && <Mono>{t.schemaSettings.schema}</Mono>],
            ['Schema encoding', t.schemaSettings?.encoding],
            ['KMS key', t.kmsKeyName && <Mono>{t.kmsKeyName}</Mono>],
            ['State', t.state && t.state !== 'STATE_UNSPECIFIED' ? t.state : undefined],
          ]}
        />
      </Card>
      <Card>
        <CardTitle>Topic resource</CardTitle>
        <JsonView value={t} label="Topic JSON" />
      </Card>
    </div>
  )
}
