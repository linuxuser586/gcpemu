import { queryOptions } from '@tanstack/react-query'

import { admin, unwrap, type SubscriptionStats } from '@/api/admin'
import { gcpFetch } from '@/api/fetch'

// The Pub/Sub view's reads and writes, all through pubsub/v1 REST, and
// the subscription statistics GCP keeps in Cloud Monitoring through the
// admin API. Types cover the fields the view reads; the raw JSON editor
// covers the rest.

export interface Topic {
  name: string
  labels?: Record<string, string>
  messageRetentionDuration?: string
  kmsKeyName?: string
  schemaSettings?: { schema?: string; encoding?: string }
  state?: string
}

export interface PushConfig {
  pushEndpoint?: string
  attributes?: Record<string, string>
  oidcToken?: { serviceAccountEmail?: string; audience?: string }
  pubsubWrapper?: Record<string, never>
  noWrapper?: { writeMetadata?: boolean }
}

export interface Subscription {
  name: string
  topic: string
  pushConfig?: PushConfig
  ackDeadlineSeconds?: number
  retainAckedMessages?: boolean
  messageRetentionDuration?: string
  labels?: Record<string, string>
  enableMessageOrdering?: boolean
  expirationPolicy?: { ttl?: string }
  filter?: string
  deadLetterPolicy?: { deadLetterTopic?: string; maxDeliveryAttempts?: number }
  retryPolicy?: { minimumBackoff?: string; maximumBackoff?: string }
  detached?: boolean
  enableExactlyOnceDelivery?: boolean
  topicMessageRetentionDuration?: string
  state?: string
}

export interface PubsubMessage {
  data?: string
  attributes?: Record<string, string>
  messageId?: string
  publishTime?: string
  orderingKey?: string
}

export interface ReceivedMessage {
  ackId: string
  message: PubsubMessage
  deliveryAttempt?: number
}

export interface Snapshot {
  name: string
  topic: string
  expireTime?: string
  labels?: Record<string, string>
}

export type { SubscriptionStats }

const enc = encodeURIComponent
const API = '/pubsub/v1'

export const topicName = (project: string, topic: string) => `projects/${project}/topics/${topic}`
export const subscriptionName = (project: string, sub: string) =>
  `projects/${project}/subscriptions/${sub}`

const topicPath = (project: string, topic: string) =>
  `${API}/projects/${enc(project)}/topics/${enc(topic)}`
const subPath = (project: string, sub: string) =>
  `${API}/projects/${enc(project)}/subscriptions/${enc(sub)}`

/** lastSegment is the ID at the end of a resource name. */
export const lastSegment = (name: string) => name.slice(name.lastIndexOf('/') + 1)

/** projectOf is the Project of a resource name. */
export const projectOf = (name: string) => name.split('/')[1] ?? ''

/** DELETED_TOPIC is the topic of a subscription whose topic was deleted. */
export const DELETED_TOPIC = '_deleted-topic_'

/** RESOURCE_ID is the API's topic and subscription ID rule. */
export const RESOURCE_ID = /^[a-zA-Z][a-zA-Z0-9\-_.~+%]{2,254}$/
export const validId = (id: string) => RESOURCE_ID.test(id) && !/^goog/i.test(id)

/** invalidName is the API's message for a malformed topic or subscription name. */
export const invalidName = (kind: 'topics' | 'subscriptions', name: string) =>
  `Invalid [${kind}] name: (name=${name})`

const json = (body: unknown) => ({ body: JSON.stringify(body) })

async function listAll<T>(path: string, field: string): Promise<T[]> {
  const out: T[] = []
  let pageToken = ''
  do {
    const q = new URLSearchParams({ pageSize: '1000' })
    if (pageToken) q.set('pageToken', pageToken)
    const res = await gcpFetch<Record<string, unknown> & { nextPageToken?: string }>(`${path}?${q}`)
    out.push(...((res[field] as T[] | undefined) ?? []))
    pageToken = res.nextPageToken ?? ''
  } while (pageToken)
  return out
}

// ---- topics ----

export const topicsQuery = (project: string) =>
  queryOptions({
    queryKey: ['pubsub', 'topics', project],
    queryFn: () => listAll<Topic>(`${API}/projects/${enc(project)}/topics`, 'topics'),
  })

export const topicQuery = (project: string, topic: string) =>
  queryOptions({
    queryKey: ['pubsub', 'topic', project, topic],
    queryFn: () => gcpFetch<Topic>(topicPath(project, topic)),
  })

/** topicSubscriptionsQuery names the subscriptions attached to a topic, in any Project. */
export const topicSubscriptionsQuery = (project: string, topic: string) =>
  queryOptions({
    queryKey: ['pubsub', 'topic-subscriptions', project, topic],
    queryFn: () => listAll<string>(`${topicPath(project, topic)}/subscriptions`, 'subscriptions'),
  })

export const createTopic = (project: string, id: string, body: unknown) =>
  gcpFetch<Topic>(topicPath(project, id), { method: 'PUT', ...json(body) })

/** patchTopic sends topics.patch: the fields to change and an updateMask naming them. */
export const patchTopic = (project: string, id: string, topic: unknown, mask: string[]) =>
  gcpFetch<Topic>(topicPath(project, id), {
    method: 'PATCH',
    ...json({ topic, updateMask: mask.join(',') }),
  })

export const deleteTopic = (project: string, id: string) =>
  gcpFetch<void>(topicPath(project, id), { method: 'DELETE' })

export const publish = (project: string, topic: string, messages: PubsubMessage[]) =>
  gcpFetch<{ messageIds: string[] }>(`${topicPath(project, topic)}:publish`, {
    method: 'POST',
    ...json({ messages }),
  })

// ---- subscriptions ----

export const subscriptionsQuery = (project: string) =>
  queryOptions({
    queryKey: ['pubsub', 'subscriptions', project],
    queryFn: () =>
      listAll<Subscription>(`${API}/projects/${enc(project)}/subscriptions`, 'subscriptions'),
  })

export const subscriptionQuery = (project: string, sub: string) =>
  queryOptions({
    queryKey: ['pubsub', 'subscription', project, sub],
    queryFn: () => gcpFetch<Subscription>(subPath(project, sub)),
  })

export const createSubscription = (project: string, id: string, body: unknown) =>
  gcpFetch<Subscription>(subPath(project, id), { method: 'PUT', ...json(body) })

/** patchSubscription sends subscriptions.patch: the fields to change and an updateMask. */
export const patchSubscription = (
  project: string,
  id: string,
  subscription: unknown,
  mask: string[],
) =>
  gcpFetch<Subscription>(subPath(project, id), {
    method: 'PATCH',
    ...json({ subscription, updateMask: mask.join(',') }),
  })

export const deleteSubscription = (project: string, id: string) =>
  gcpFetch<void>(subPath(project, id), { method: 'DELETE' })

/** pull leases up to max messages without waiting for more. */
export const pull = async (project: string, sub: string, max: number) =>
  (
    await gcpFetch<{ receivedMessages?: ReceivedMessage[] }>(`${subPath(project, sub)}:pull`, {
      method: 'POST',
      ...json({ maxMessages: max, returnImmediately: true }),
    })
  ).receivedMessages ?? []

export const acknowledge = (project: string, sub: string, ackIds: string[]) =>
  gcpFetch<void>(`${subPath(project, sub)}:acknowledge`, { method: 'POST', ...json({ ackIds }) })

/** nack returns leased messages to the subscription for redelivery now. */
export const nack = (project: string, sub: string, ackIds: string[]) =>
  gcpFetch<void>(`${subPath(project, sub)}:modifyAckDeadline`, {
    method: 'POST',
    ...json({ ackIds, ackDeadlineSeconds: 0 }),
  })

/** seek moves a subscription to a time (RFC 3339) or a snapshot's full name. */
export const seek = (project: string, sub: string, to: { time: string } | { snapshot: string }) =>
  gcpFetch<void>(`${subPath(project, sub)}:seek`, { method: 'POST', ...json(to) })

export const snapshotsQuery = (project: string) =>
  queryOptions({
    queryKey: ['pubsub', 'snapshots', project],
    queryFn: () => listAll<Snapshot>(`${API}/projects/${enc(project)}/snapshots`, 'snapshots'),
  })

// ---- statistics ----

/**
 * statsQuery is the backlog, oldest unacked message and dead-lettered
 * count of every subscription of a Project, and the Emulator clock they
 * are measured against. Publishes and acks are writes, so events keep it
 * fresh.
 */
export const statsQuery = (project: string) =>
  queryOptions({
    queryKey: ['pubsub', 'stats', project],
    queryFn: async () => {
      const res = await unwrap(
        admin.GET('/_emu/v1/pubsub/subscriptions', { params: { query: { project } } }),
      )
      return { now: res.now, byName: new Map(res.subscriptions.map((s) => [s.name, s])) }
    },
  })

// ---- durations ----

/** seconds reads a protobuf Duration ("86400s") as whole seconds. */
export const seconds = (d?: string) => (d ? d.replace(/s$/, '').replace(/\.0+$/, '') : '')

/** duration writes whole seconds as a protobuf Duration. */
export const duration = (s: string) => `${s}s`

/** goDuration writes seconds as the API's messages do, e.g. "1h0m0s". */
export function goDuration(s: number): string {
  if (s === 0) return '0s'
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  const rest = s % 60
  if (h) return `${h}h${m}m${rest}s`
  if (m) return `${m}m${rest}s`
  return `${rest}s`
}
