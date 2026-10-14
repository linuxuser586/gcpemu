import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, expect, it } from 'vitest'

import type { RequestEntry } from '@/api/admin'
import { appendRequest } from '@/api/events'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import { bodyText, matches, size, statusMatches, type Filters, type RequestDetail } from './api'

const rows = () => screen.queryAllByTestId('request')

const entry = (id: number, e: Partial<RequestEntry> = {}): RequestEntry => ({
  id,
  time: `2026-10-14T12:00:0${id % 10}.250Z`,
  service: 'gcs',
  protocol: 'http',
  method: 'GET /storage/v1/b',
  principal: 'user:dev@example.com',
  resource: '//storage.googleapis.com/projects/_/buckets/b',
  status: 200,
  latencyMs: 1.25,
  ...e,
})

const LOG: RequestEntry[] = [
  entry(1, { method: 'POST /storage/v1/b', status: 200 }),
  entry(2, {
    service: 'pubsub',
    protocol: 'grpc',
    method: '/google.pubsub.v1.Publisher/Publish',
    principal: 'serviceAccount:pub@alpha-project.iam.gserviceaccount.com',
    resource: '//pubsub.googleapis.com/projects/alpha-project/topics/t',
    status: 5,
    code: 'NOT_FOUND',
  }),
  entry(3, { method: 'GET /storage/v1/b/missing', status: 404 }),
]

const DETAILS: Record<number, RequestDetail> = {
  1: {
    ...LOG[0]!,
    requestBody: { contentType: 'application/json', size: 15, text: '{"name":"logs"}' },
    responseBody: {
      contentType: 'application/json',
      size: 31,
      text: '{"kind":"storage#bucket","id":1}',
    },
  },
  3: {
    ...LOG[2]!,
    responseBody: { contentType: 'application/octet-stream', size: 2048, binary: true },
  },
}

beforeEach(() => {
  server.use(
    http.get('*/_emu/v1/requests', () => HttpResponse.json({ requests: LOG })),
    http.get('*/_emu/v1/requests/:id', ({ params }) => {
      const d = DETAILS[Number(params.id)]
      return d
        ? HttpResponse.json(d)
        : HttpResponse.json({ error: 'no such request log entry' }, { status: 404 })
    }),
  )
})

it('tails the Request log, newest first, and filters it from the URL', async () => {
  const user = userEvent.setup()
  const { router, queryClient } = renderApp('/requests?status=4xx')
  expect(await screen.findByRole('heading', { name: 'Request log' })).toBeInTheDocument()
  await waitFor(() => expect(rows()).toHaveLength(1))
  expect(rows()[0]).toHaveTextContent('GET /storage/v1/b/missing')
  expect(screen.getByText('Live. 1 of 3 requests match.')).toBeInTheDocument()

  await user.clear(screen.getByLabelText('Status'))
  expect(rows()).toHaveLength(3)
  expect(rows().map((r) => r.textContent)).toEqual([
    expect.stringContaining('/storage/v1/b/missing'),
    expect.stringContaining('/google.pubsub.v1.Publisher/Publish'),
    expect.stringContaining('POST /storage/v1/b'),
  ])
  expect(within(rows()[1]!).getByText('NOT_FOUND')).toBeInTheDocument()
  expect(within(rows()[1]!).getByText('Pub/Sub')).toBeInTheDocument()

  // A new entry arrives on the events stream.
  act(() => appendRequest(queryClient, entry(4, { method: 'DELETE /storage/v1/b/b', status: 204 })))
  await waitFor(() => expect(rows()).toHaveLength(4))
  expect(rows()[0]).toHaveTextContent('DELETE /storage/v1/b/b')

  // Each filter is in the URL.
  await user.selectOptions(screen.getByLabelText('Service'), 'pubsub')
  await user.type(screen.getByLabelText('Principal'), 'serviceAccount:')
  await user.type(screen.getByLabelText('Resource'), 'topics/t')
  await user.type(screen.getByLabelText('Status'), 'not_found')
  await user.type(screen.getByLabelText('Method'), 'Publish')
  expect(rows()).toHaveLength(1)
  await waitFor(() => {
    const q = new URLSearchParams(router.state.location.search)
    expect(Object.fromEntries(q)).toEqual({
      service: 'pubsub',
      principal: 'serviceAccount:',
      resource: 'topics/t',
      status: 'not_found',
      method: 'Publish',
    })
  })
  await user.type(screen.getByLabelText('Method'), 'x')
  expect(screen.getByText('No requests match the filters.')).toBeVisible()
})

it('pauses the tail', async () => {
  const user = userEvent.setup()
  const { queryClient } = renderApp('/requests')
  await waitFor(() => expect(rows()).toHaveLength(3))
  await user.click(screen.getByRole('button', { name: 'Pause' }))
  act(() => appendRequest(queryClient, entry(4)))
  expect(rows()).toHaveLength(3)
  expect(screen.getByText('Paused. 3 requests.')).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Resume' }))
  expect(rows()).toHaveLength(4)
})

it('expands a request to its bodies', async () => {
  const user = userEvent.setup()
  renderApp('/requests')
  await waitFor(() => expect(rows()).toHaveLength(3))

  const open = within(rows()[2]!).getByRole('button', { name: 'Details of POST /storage/v1/b' })
  await user.click(open)
  expect(open).toHaveAttribute('aria-expanded', 'true')
  const details = screen.getByRole('region', { name: 'Details of POST /storage/v1/b' })
  expect(within(details).getByText('user:dev@example.com')).toBeInTheDocument()
  expect(await within(details).findByRole('region', { name: 'Request body' })).toHaveTextContent(
    'application/json, 15 bytes',
  )
  // JSON is indented.
  expect(
    within(details).getByRole('region', { name: 'Response body' }).querySelector('pre')
      ?.textContent,
  ).toBe('{\n  "kind": "storage#bucket",\n  "id": 1\n}')

  // A binary response is described, not shown.
  await user.click(
    within(rows()[0]!).getByRole('button', { name: 'Details of GET /storage/v1/b/missing' }),
  )
  const missing = screen.getByRole('region', { name: 'Details of GET /storage/v1/b/missing' })
  expect(await within(missing).findByText('Binary, 2 KiB; not shown.')).toBeInTheDocument()
  expect(within(missing).getByRole('region', { name: 'Request body' })).toHaveTextContent(
    'No body.',
  )

  // An entry the Instance no longer keeps.
  await user.click(
    within(rows()[1]!).getByRole('button', {
      name: 'Details of /google.pubsub.v1.Publisher/Publish',
    }),
  )
  expect(
    await screen.findByText("The Instance no longer keeps this request's bodies."),
  ).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Close details' }))
  expect(screen.queryByRole('region', { name: /^Details of/ })).not.toBeInTheDocument()
})

it('virtualizes 100,000 rows', async () => {
  const many = Array.from({ length: 100_000 }, (_, i) => entry(i + 1))
  server.use(http.get('*/_emu/v1/requests', () => HttpResponse.json({ requests: many })))
  renderApp('/requests')
  expect(await screen.findByText('Live. 100000 requests.')).toBeInTheDocument()
  expect(screen.getByRole('table', { name: 'Requests' })).toHaveAttribute('aria-rowcount', '100001')
  expect(rows().length).toBeLessThan(50)
})

it('is reached from the sidebar', async () => {
  const user = userEvent.setup()
  renderApp('/?project=alpha-project')
  await user.click(
    within(await screen.findByRole('navigation', { name: 'Services' })).getByRole('link', {
      name: 'Request log',
    }),
  )
  expect(await screen.findByRole('heading', { name: 'Request log' })).toBeInTheDocument()
})

it('matches statuses by number, class or code', () => {
  const http404 = entry(1, { status: 404 })
  const grpc = entry(2, { protocol: 'grpc', status: 5, code: 'NOT_FOUND' })
  expect(statusMatches(http404, '404')).toBe(true)
  expect(statusMatches(http404, '4xx')).toBe(true)
  expect(statusMatches(http404, '5xx')).toBe(false)
  expect(statusMatches(http404, '40')).toBe(false)
  expect(statusMatches(grpc, 'not_found')).toBe(true)
  expect(statusMatches(grpc, '5')).toBe(true)
  const none: Filters = { service: '', method: '', principal: '', status: '', resource: '' }
  expect(matches(grpc, { ...none, method: 'publish' })).toBe(false)
  expect(matches(grpc, { ...none, service: 'gcs', resource: 'BUCKETS/B' })).toBe(true)
})

it('formats bodies and sizes', () => {
  expect(bodyText({ size: 7, text: '{"a":1}' })).toBe('{\n  "a": 1\n}')
  expect(bodyText({ size: 99, text: '{"a":', truncated: true })).toBe('{"a":')
  expect(bodyText({ size: 5, text: 'plain' })).toBe('plain')
  expect(size(1)).toBe('1 byte')
  expect(size(16384)).toBe('16 KiB')
  expect(size(3 * 1024 * 1024 + 1)).toBe('3 MiB')
})
