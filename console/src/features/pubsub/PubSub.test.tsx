import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, expect, it } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import { decodePayload, encodePayload } from '../secrets/api'
import type { ReceivedMessage, Subscription, SubscriptionStats, Topic } from './api'
import { formatAge } from './format'
import { editValues, patchBody } from './SubscriptionForm'

const P = 'alpha-project'
const T = (id: string) => `projects/${P}/topics/${id}`
const S = (id: string) => `projects/${P}/subscriptions/${id}`
const NOW = '2026-10-09T12:00:00Z'

// A small in-memory Pub/Sub REST API and its admin statistics: topics,
// subscriptions, the messages a pull returns, and the requests the view
// sends.
let topics: Topic[] = []
let subs: Subscription[] = []
let stats: SubscriptionStats[] = []
let queued: ReceivedMessage[] = []
let requests: { method: string; path: string; body: unknown }[] = []

const err = (code: number, status: string, message: string) =>
  HttpResponse.json({ error: { code, status, message } }, { status: code })

beforeEach(() => {
  requests = []
  fake.info.services = ['iam', 'pubsub']
  topics = [
    { name: T('orders'), labels: { team: 'shop' }, messageRetentionDuration: '86400s' },
    { name: T('orders-dlq') },
    { name: T('audit') },
  ]
  subs = [
    {
      name: S('orders-worker'),
      topic: T('orders'),
      ackDeadlineSeconds: 30,
      messageRetentionDuration: '604800s',
      expirationPolicy: { ttl: '2678400s' },
      pushConfig: {},
      deadLetterPolicy: { deadLetterTopic: T('orders-dlq'), maxDeliveryAttempts: 5 },
      labels: { tier: 'gold' },
    },
    {
      name: S('orders-push'),
      topic: T('orders'),
      ackDeadlineSeconds: 10,
      pushConfig: { pushEndpoint: 'https://example.com/push' },
    },
  ]
  stats = [
    {
      name: S('orders-worker'),
      topic: T('orders'),
      backlog: 3,
      backlogBytes: 2048,
      outstanding: 1,
      oldestUnackedPublishTime: '2026-10-09T11:55:00Z',
      deadLettered: 2,
    },
    {
      name: S('orders-push'),
      topic: T('orders'),
      backlog: 0,
      backlogBytes: 0,
      outstanding: 0,
      deadLettered: 0,
    },
  ]
  queued = [
    {
      ackId: 'ack-1',
      deliveryAttempt: 1,
      message: {
        messageId: '101',
        data: encodePayload('{"order":1}'),
        attributes: { kind: 'new' },
        orderingKey: 'cust-1',
        publishTime: '2026-10-09T11:55:00Z',
      },
    },
    {
      ackId: 'ack-2',
      deliveryAttempt: 2,
      message: { messageId: '102', data: btoa('\xff\xfe'), publishTime: '2026-10-09T11:56:00Z' },
    },
  ]
  server.use(
    http.get('*/_emu/v1/pubsub/subscriptions', ({ request }) => {
      const project = new URL(request.url).searchParams.get('project')
      return HttpResponse.json({
        now: NOW,
        subscriptions: stats.filter((s) => s.name.startsWith(`projects/${project}/`)),
      })
    }),
    http.all(/\/pubsub\/v1\/(.*)$/, async ({ request }) => {
      const url = new URL(request.url)
      const text = request.method === 'GET' ? '' : await request.text()
      const body: unknown = text ? JSON.parse(text) : undefined
      const path = decodeURIComponent(url.pathname.replace(/^.*\/pubsub\/v1\//, ''))
      requests.push({ method: request.method, path, body })
      const m =
        /^projects\/[^/]+\/(topics|subscriptions|snapshots)(?:\/([^/:]+))?(\/subscriptions)?(?::(\w+))?$/.exec(
          path,
        )
      if (!m) return err(400, 'INVALID_ARGUMENT', `bad path ${path}`)
      const [, kind, id, nested, verb] = m
      if (kind === 'snapshots') {
        return HttpResponse.json({
          snapshots: [{ name: `projects/${P}/snapshots/before-deploy`, topic: T('orders') }],
        })
      }
      if (kind === 'topics') {
        if (!id) return HttpResponse.json({ topics })
        const t = topics.find((x) => x.name === T(id))
        if (request.method === 'PUT') {
          if (t)
            return err(
              409,
              'ALREADY_EXISTS',
              `Resource already exists in the project (resource=${id}).`,
            )
          const created = { ...(body as Topic), name: T(id) }
          topics.push(created)
          return HttpResponse.json(created)
        }
        if (!t) return err(404, 'NOT_FOUND', `Resource not found (resource=${id}).`)
        if (nested) {
          return HttpResponse.json({
            subscriptions: [
              ...subs.filter((s) => s.topic === t.name).map((s) => s.name),
              'projects/beta-project/subscriptions/mirror',
            ],
          })
        }
        if (verb === 'publish') return HttpResponse.json({ messageIds: ['555'] })
        if (request.method === 'PATCH')
          return HttpResponse.json({ ...t, ...(body as { topic: Topic }).topic })
        if (request.method === 'DELETE') {
          topics = topics.filter((x) => x !== t)
          return HttpResponse.json({})
        }
        return HttpResponse.json(t)
      }
      if (!id) return HttpResponse.json({ subscriptions: subs })
      const s = subs.find((x) => x.name === S(id))
      if (request.method === 'PUT') {
        const created = { ...(body as Subscription), name: S(id) }
        subs.push(created)
        return HttpResponse.json(created)
      }
      if (!s) return err(404, 'NOT_FOUND', `Resource not found (resource=${id}).`)
      if (verb === 'pull') {
        const out = queued
        queued = []
        return HttpResponse.json(out.length ? { receivedMessages: out } : {})
      }
      if (verb) return HttpResponse.json({})
      if (request.method === 'PATCH') {
        return HttpResponse.json({ ...s, ...(body as { subscription: Subscription }).subscription })
      }
      if (request.method === 'DELETE') {
        subs = subs.filter((x) => x !== s)
        return HttpResponse.json({})
      }
      return HttpResponse.json(s)
    }),
  )
})

const sent = (method: string, path: RegExp) =>
  requests.filter((r) => r.method === method && path.test(r.path))

it('says how to enable Pub/Sub when it is off', async () => {
  fake.info.services = ['iam']
  renderApp(`/pubsub?project=${P}`)
  expect(
    await screen.findByRole('heading', { name: /Service pubsub is not enabled/ }),
  ).toBeInTheDocument()
})

it('asks for a Project before listing topics', async () => {
  renderApp('/pubsub')
  expect(await screen.findByText(/Choose a Project/)).toBeInTheDocument()
})

it('lists topics with their subscriptions and filters them in the URL', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/pubsub?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Topics' })
  const orders = within(table)
    .getAllByTestId('topic')
    .find((r) => r.textContent?.startsWith('orders2'))
  expect(orders).toHaveTextContent('team=shop')
  expect(orders).toHaveTextContent('1 day')

  await user.type(screen.getByLabelText('Filter'), 'dlq')
  expect(router.state.location.search).toContain('q=dlq')
  expect(within(table).getAllByTestId('topic')).toHaveLength(1)
})

it('lists subscriptions with backlog, oldest unacked age and dead-letter counts', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/pubsub?project=${P}`)
  await user.click(await screen.findByRole('link', { name: 'Subscriptions' }))
  expect(router.state.location.pathname).toBe('/pubsub/subscriptions')
  const table = await screen.findByRole('table', { name: 'Subscriptions' })
  // The fake lists them as created; the API sorts them by name.
  const [worker, push] = within(table).getAllByTestId('subscription')
  await waitFor(() => expect(worker).toHaveTextContent('3 (2.0 KiB)'))
  expect(worker).toHaveTextContent('5 min')
  expect(worker).toHaveTextContent('Pull')
  expect(within(worker!).getAllByRole('cell')[5]).toHaveTextContent('2')
  expect(push).toHaveTextContent('Push')
  expect(within(push!).getAllByRole('cell')[5]).toHaveTextContent('—')
})

it('creates a topic with the API’s validation messages', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/pubsub/create-topic?project=${P}`)
  await user.type(await screen.findByLabelText('Topic ID'), 'goog-x')
  await user.type(screen.getByLabelText('Message retention (seconds)'), '60')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(
    await screen.findByText(`Invalid [topics] name: (name=projects/${P}/topics/goog-x)`),
  ).toBeInTheDocument()
  expect(
    screen.getByText(
      'Invalid message_retention_duration: must be between 10 minutes and 31 days, got 1m0s.',
    ),
  ).toBeInTheDocument()

  await user.clear(screen.getByLabelText('Topic ID'))
  await user.type(screen.getByLabelText('Topic ID'), 'events')
  await user.clear(screen.getByLabelText('Message retention (seconds)'))
  await user.type(screen.getByLabelText('Message retention (seconds)'), '3600')
  await user.type(screen.getByLabelText('Labels'), 'env=dev')
  await user.click(screen.getByRole('tab', { name: 'JSON' }))
  const json: unknown = JSON.parse(screen.getByLabelText<HTMLTextAreaElement>('Request body').value)
  expect(json).toEqual({ labels: { env: 'dev' }, messageRetentionDuration: '3600s' })
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() => expect(router.state.location.pathname).toBe('/pubsub/topics/events'))
  expect(sent('PUT', /topics\/events$/)).toHaveLength(1)
})

it('edits a topic with an updateMask of what changed', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/pubsub/topics/orders/edit?project=${P}`)
  await user.clear(await screen.findByLabelText('Message retention (seconds)'))
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(router.state.location.pathname).toBe('/pubsub/topics/orders/overview'))
  const [patch] = sent('PATCH', /topics\/orders$/)
  expect(patch!.body).toEqual({
    topic: { messageRetentionDuration: null },
    updateMask: 'messageRetentionDuration',
  })
})

it('publishes a message with attributes and an ordering key', async () => {
  const user = userEvent.setup()
  renderApp(`/pubsub/topics/orders?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Subscriptions' })
  expect(within(table).getByText('projects/beta-project/subscriptions/mirror')).toBeInTheDocument()

  await user.click(screen.getByRole('button', { name: 'Publish message' }))
  const dialog = await screen.findByRole('dialog')
  await user.click(within(dialog).getByRole('button', { name: 'Publish' }))
  expect(
    within(dialog).getByText(/One or more messages in the publish request is empty/),
  ).toBeInTheDocument()
  await user.type(within(dialog).getByLabelText('Attributes'), 'googkey=1')
  await user.click(within(dialog).getByRole('button', { name: 'Publish' }))
  expect(
    within(dialog).getByText(
      "The attribute key 'googkey' is reserved; keys may not begin with 'goog'.",
    ),
  ).toBeInTheDocument()
  expect(sent('POST', /:publish$/)).toHaveLength(0)

  await user.clear(within(dialog).getByLabelText('Attributes'))
  await user.type(within(dialog).getByLabelText('Attributes'), 'kind=new\nurl=a=b')
  await user.type(within(dialog).getByLabelText('Message body'), 'hello ✓')
  await user.type(within(dialog).getByLabelText('Ordering key'), 'cust-1')
  await user.click(within(dialog).getByRole('button', { name: 'Publish' }))
  expect(await screen.findByText('Published message 555.')).toBeInTheDocument()
  const [pub] = sent('POST', /topics\/orders:publish$/)
  const [m] = (pub!.body as { messages: { data: string }[] }).messages
  expect(m).toEqual({
    data: encodePayload('hello ✓'),
    attributes: { kind: 'new', url: 'a=b' },
    orderingKey: 'cust-1',
  })
  expect(decodePayload(m!.data)).toBe('hello ✓')
})

it('deletes a topic', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/pubsub/topics/audit/overview?project=${P}`)
  expect(await screen.findByLabelText('Topic details')).toHaveTextContent('None')
  await user.click(screen.getByRole('button', { name: 'Delete topic' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete topic' }),
  )
  await waitFor(() => expect(router.state.location.pathname).toBe('/pubsub'))
  expect(sent('DELETE', /topics\/audit$/)).toHaveLength(1)
})

it('creates a subscription to the topic it came from, checking the API’s rules', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/pubsub/topics/orders?project=${P}`)
  await user.click(await screen.findByRole('link', { name: 'Create subscription' }))
  await user.type(await screen.findByLabelText('Subscription ID'), 'ab')
  expect(screen.getByLabelText('Topic')).toHaveValue(T('orders'))
  await user.clear(screen.getByLabelText('Acknowledgement deadline (seconds)'))
  await user.type(screen.getByLabelText('Acknowledgement deadline (seconds)'), '5')
  await user.selectOptions(screen.getByLabelText('Delivery type'), 'push')
  await user.type(screen.getByLabelText('Push endpoint'), 'ftp://x')
  await user.type(screen.getByLabelText('Dead-letter topic'), T('orders-dlq'))
  await user.clear(screen.getByLabelText('Maximum delivery attempts'))
  await user.type(screen.getByLabelText('Maximum delivery attempts'), '200')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(
    await screen.findByText(`Invalid [subscriptions] name: (name=projects/${P}/subscriptions/ab)`),
  ).toBeInTheDocument()
  expect(screen.getByText(/Invalid ack_deadline_seconds: 5\./)).toBeInTheDocument()
  expect(
    screen.getByText('Invalid push_config.push_endpoint: "ftp://x" is not a valid URL.'),
  ).toBeInTheDocument()
  expect(
    screen.getByText(
      'Invalid dead_letter_policy.max_delivery_attempts: 200. It must be between 5 and 100.',
    ),
  ).toBeInTheDocument()

  await user.clear(screen.getByLabelText('Subscription ID'))
  await user.type(screen.getByLabelText('Subscription ID'), 'orders-audit')
  await user.clear(screen.getByLabelText('Acknowledgement deadline (seconds)'))
  await user.type(screen.getByLabelText('Acknowledgement deadline (seconds)'), '60')
  await user.clear(screen.getByLabelText('Push endpoint'))
  await user.type(screen.getByLabelText('Push endpoint'), 'https://svc.example/push')
  await user.clear(screen.getByLabelText('Maximum delivery attempts'))
  await user.type(screen.getByLabelText('Maximum delivery attempts'), '7')
  await user.click(screen.getByLabelText('Message ordering'))
  await user.selectOptions(screen.getByLabelText('Retry policy'), 'backoff')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/pubsub/subscriptions/orders-audit'),
  )
  const [create] = sent('PUT', /subscriptions\/orders-audit$/)
  expect(create!.body).toEqual({
    topic: T('orders'),
    enableMessageOrdering: true,
    pushConfig: { pushEndpoint: 'https://svc.example/push' },
    ackDeadlineSeconds: 60,
    messageRetentionDuration: '604800s',
    expirationPolicy: { ttl: '2678400s' },
    deadLetterPolicy: { deadLetterTopic: T('orders-dlq'), maxDeliveryAttempts: 7 },
    retryPolicy: { minimumBackoff: '10s', maximumBackoff: '600s' },
  })
})

it('edits a subscription with an updateMask of what changed', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/pubsub/subscriptions/orders-worker/edit?project=${P}`)
  await user.clear(await screen.findByLabelText('Dead-letter topic'))
  await user.selectOptions(screen.getByLabelText('Expiration'), 'never')
  await user.click(screen.getByLabelText('Retain acknowledged messages'))
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/pubsub/subscriptions/orders-worker/overview'),
  )
  const [patch] = sent('PATCH', /subscriptions\/orders-worker$/)
  expect(patch!.body).toEqual({
    subscription: { retainAckedMessages: true, expirationPolicy: {}, deadLetterPolicy: null },
    updateMask: 'retainAckedMessages,expirationPolicy,deadLetterPolicy',
  })
})

it('shows the backlog, pulls and acks, and peeks without acking', async () => {
  const user = userEvent.setup()
  renderApp(`/pubsub/subscriptions/orders-worker?project=${P}`)
  const backlog = await screen.findByLabelText('Backlog')
  expect(backlog).toHaveTextContent('Unacked messages3')
  expect(backlog).toHaveTextContent('5 min')
  expect(backlog).toHaveTextContent('Dead-lettered2')

  await user.click(screen.getByRole('button', { name: 'Pull' }))
  const table = await screen.findByRole('table', { name: 'Pulled messages' })
  const [first, second] = within(table).getAllByTestId('message')
  expect(first).toHaveTextContent('{"order":1}')
  expect(first).toHaveTextContent('kind=new')
  expect(first).toHaveTextContent('cust-1')
  expect(second).toHaveTextContent('base64')
  await user.click(within(first!).getByRole('button', { name: 'Ack message 101' }))
  await waitFor(() => expect(within(table).getAllByTestId('message')).toHaveLength(1))
  expect(sent('POST', /:acknowledge$/)[0]!.body).toEqual({ ackIds: ['ack-1'] })

  queued = [{ ackId: 'ack-3', message: { messageId: '103', data: encodePayload('peek me') } }]
  await user.click(screen.getByRole('button', { name: 'Peek' }))
  expect(await screen.findByText('peek me')).toBeInTheDocument()
  expect(screen.getByText('Peeked')).toBeInTheDocument()
  expect(screen.queryByRole('button', { name: /^Ack message/ })).not.toBeInTheDocument()
  expect(sent('POST', /:modifyAckDeadline$/)[0]!.body).toEqual({
    ackIds: ['ack-3'],
    ackDeadlineSeconds: 0,
  })
})

it('purges by seeking to the Emulator clock, and seeks to a snapshot', async () => {
  const user = userEvent.setup()
  renderApp(`/pubsub/subscriptions/orders-worker?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Purge messages' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Purge messages' }),
  )
  await waitFor(() => expect(sent('POST', /:seek$/)).toHaveLength(1))
  expect(sent('POST', /:seek$/)[0]!.body).toEqual({ time: NOW })

  await user.click(screen.getByRole('button', { name: 'Seek' }))
  const dialog = await screen.findByRole('dialog')
  await user.selectOptions(within(dialog).getByLabelText('Seek to'), 'snapshot')
  await user.selectOptions(
    within(dialog).getByLabelText('Snapshot'),
    await within(dialog).findByRole('option', { name: 'before-deploy' }),
  )
  await user.click(within(dialog).getByRole('button', { name: 'Seek' }))
  await waitFor(() => expect(sent('POST', /:seek$/)).toHaveLength(2))
  expect(sent('POST', /:seek$/)[1]!.body).toEqual({
    snapshot: `projects/${P}/snapshots/before-deploy`,
  })
})

it('shows the configuration and deletes a subscription', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/pubsub/subscriptions/orders-worker/overview?project=${P}`)
  const details = await screen.findByLabelText('Subscription details')
  expect(details).toHaveTextContent(`${T('orders-dlq')} after 5 delivery attempts`)
  expect(details).toHaveTextContent('After 31 days of inactivity')
  await user.click(screen.getByRole('button', { name: 'Delete subscription' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete subscription' }),
  )
  await waitFor(() => expect(router.state.location.pathname).toBe('/pubsub/subscriptions'))
  expect(sent('DELETE', /subscriptions\/orders-worker$/)).toHaveLength(1)
})

it('builds subscription patch bodies per field', () => {
  const s = subs[0]!
  const v = editValues(s)
  expect(patchBody(s, v, {})).toEqual({})
  expect(
    patchBody(
      s,
      { ...v, delivery: 'push', pushEndpoint: 'http://h/p', retry: 'backoff', minBackoff: '1' },
      {},
    ),
  ).toEqual({
    pushConfig: { pushEndpoint: 'http://h/p' },
    retryPolicy: { minimumBackoff: '1s', maximumBackoff: '600s' },
  })
  expect(patchBody(s, { ...v, labels: '', ackDeadline: '' }, {})).toEqual({
    labels: {},
    ackDeadlineSeconds: null,
  })
})

it('formats ages', () => {
  expect(formatAge(42_000)).toBe('42 s')
  expect(formatAge(5 * 60_000)).toBe('5 min')
  expect(formatAge(2 * 3600_000 + 60_000)).toBe('2 h 1 min')
  expect(formatAge(3 * 86400_000)).toBe('3 d')
})
