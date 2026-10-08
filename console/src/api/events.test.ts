import { QueryClient } from '@tanstack/react-query'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import type { RequestEntry } from './admin'
import { connectEvents, FLUSH_MS, RECONNECT_MS, staleKeys } from './events'

// FakeEventSource stands in for the browser's: tests emit events and
// errors on the latest instance.
class FakeEventSource extends EventTarget {
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSED = 2
  static instances: FakeEventSource[] = []
  readyState = FakeEventSource.OPEN
  onerror: ((e: Event) => void) | null = null
  constructor(readonly url: string) {
    super()
    FakeEventSource.instances.push(this)
  }
  close() {
    this.readyState = FakeEventSource.CLOSED
  }
  emit(type: string, data: unknown, id = '') {
    this.dispatchEvent(new MessageEvent(type, { data: JSON.stringify(data), lastEventId: id }))
  }
  fail() {
    this.readyState = FakeEventSource.CLOSED
    this.onerror?.(new Event('error'))
  }
}

const latest = () => FakeEventSource.instances.at(-1)!
const URL_ = 'http://gw/_emu/v1/events'

let client: QueryClient
let disconnect: () => void

beforeEach(() => {
  vi.useFakeTimers()
  FakeEventSource.instances = []
  client = new QueryClient()
  disconnect = connectEvents(client, {
    EventSource: FakeEventSource as unknown as typeof EventSource,
    url: URL_,
  })
})

afterEach(() => {
  disconnect()
  vi.useRealTimers()
})

/** seed caches a query and returns whether it is stale. */
function seed(key: unknown[], data: unknown = 1) {
  client.setQueryData(key, data)
  return () => client.getQueryState(key)?.isInvalidated
}

it('maps events to query keys', () => {
  expect(staleKeys('resource', { service: 'gcs', namespace: 'gcs/buckets', key: 'b' })).toEqual([
    ['admin', 'resources'],
    ['gcs'],
  ])
  expect(staleKeys('resource', { service: 'core', namespace: 'core/projects', key: 'p' })).toEqual([
    ['admin', 'resources'],
    ['iam'],
  ])
  expect(staleKeys('operation', { service: 'sql', name: 'p/op', done: true })).toEqual([
    ['admin', 'operations'],
    ['sql'],
  ])
  expect(staleKeys('reset', {})).toBe('all')
  expect(staleKeys('gap', {})).toBe('all')
  expect(staleKeys('request', {})).toEqual([])
})

it('invalidates the queries a resource event affects, batched', async () => {
  const counts = seed(['admin', 'resources', 'counts'])
  const buckets = seed(['gcs', 'buckets', 'p'])
  const topics = seed(['pubsub', 'topics', 'p'])
  latest().emit('resource', { service: 'gcs', namespace: 'gcs/buckets', key: 'b' }, 'e-1')
  latest().emit('resource', { service: 'gcs', namespace: 'gcs/objects', key: 'b\u0000o' }, 'e-2')
  expect(counts()).toBe(false)
  await vi.advanceTimersByTimeAsync(FLUSH_MS)
  expect(counts()).toBe(true)
  expect(buckets()).toBe(true)
  expect(topics()).toBe(false)
})

it('invalidates everything on reset and gap', async () => {
  for (const type of ['reset', 'gap']) {
    const topics = seed(['pubsub', 'topics', 'p'])
    const info = seed(['admin', 'info'])
    latest().emit(type, {})
    await vi.advanceTimersByTimeAsync(FLUSH_MS)
    expect(topics()).toBe(true)
    expect(info()).toBe(true)
  }
})

it('appends Request log entries to matching cached logs', () => {
  const entry = (service: string): RequestEntry => ({ service, method: 'GET /x', status: 200 })
  const all = seed(['admin', 'requests', ''], [entry('pubsub')])
  const gcsKey = ['admin', 'requests', 'gcs']
  client.setQueryData(gcsKey, [])
  latest().emit('request', entry('gcs'))
  latest().emit('request', entry('pubsub'))
  expect(
    client.getQueryData<RequestEntry[]>(['admin', 'requests', ''])?.map((e) => e.service),
  ).toEqual(['pubsub', 'gcs', 'pubsub'])
  expect(client.getQueryData<RequestEntry[]>(gcsKey)?.map((e) => e.service)).toEqual(['gcs'])
  expect(all()).toBe(false)
})

it('reopens a closed stream, resuming from the last event ID', async () => {
  latest().emit('resource', { service: 'gcs', namespace: 'gcs/buckets', key: 'b' }, 'abc-7')
  expect(latest().url).toBe(URL_)
  latest().fail()
  expect(FakeEventSource.instances).toHaveLength(1)
  await vi.advanceTimersByTimeAsync(RECONNECT_MS)
  expect(FakeEventSource.instances).toHaveLength(2)
  expect(latest().url).toBe(`${URL_}?lastEventId=abc-7`)

  disconnect()
  expect(latest().readyState).toBe(FakeEventSource.CLOSED)
  latest().fail()
  await vi.advanceTimersByTimeAsync(RECONNECT_MS)
  expect(FakeEventSource.instances).toHaveLength(2)
})

it('ignores events that are not JSON', async () => {
  const counts = seed(['admin', 'resources', 'counts'])
  latest().dispatchEvent(new MessageEvent('resource', { data: 'not json' }))
  await vi.advanceTimersByTimeAsync(FLUSH_MS)
  expect(counts()).toBe(false)
})
