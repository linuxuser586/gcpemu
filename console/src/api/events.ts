import type { QueryClient, QueryKey } from '@tanstack/react-query'

import type { OperationChange, RequestEntry, ResourceChange } from './admin'
import { origin } from './fetch'

// The cache bridge (FR-UI-006): one EventSource per tab on
// /_emu/v1/events. Each event marks the queries it affects stale, so
// open views refetch without polling; Request log entries are appended
// to cached logs instead. A `reset`, or a `gap` after a reconnect that
// missed events, invalidates everything.

export const EVENTS_PATH = '/_emu/v1/events'

/** REQUESTS_KEPT caps a cached Request log, as the Instance does. */
export const REQUESTS_KEPT = 2000

/** FLUSH_MS batches the invalidations of a burst of events. */
export const FLUSH_MS = 50

/** RECONNECT_MS is the wait before replacing a closed EventSource. */
export const RECONNECT_MS = 1000

const RESOURCES: QueryKey = ['admin', 'resources']
const OPERATIONS: QueryKey = ['admin', 'operations']
const REQUESTS: QueryKey = ['admin', 'requests']

/**
 * staleKeys returns the query key prefixes an event makes stale, or 'all'.
 * Request events patch the cache instead and return none.
 */
export function staleKeys(type: string, data: unknown): QueryKey[] | 'all' {
  switch (type) {
    case 'resource': {
      const { service } = data as ResourceChange
      // Projects are state the emulator owns ("core/projects"); the
      // console reads them through the iam Service.
      return [RESOURCES, [service === 'core' ? 'iam' : service]]
    }
    case 'operation': {
      const { service } = data as OperationChange
      return [OPERATIONS, [service]]
    }
    case 'reset':
    case 'gap':
      return 'all'
    default:
      return []
  }
}

/** appendRequest adds a Request log entry to every cached log it belongs in. */
export function appendRequest(client: QueryClient, entry: RequestEntry) {
  for (const [key, old] of client.getQueriesData<RequestEntry[]>({ queryKey: REQUESTS })) {
    const service = key[2]
    if (!old || (service && service !== entry.service)) continue
    client.setQueryData<RequestEntry[]>(key, [...old, entry].slice(-REQUESTS_KEPT))
  }
}

export interface EventsOptions {
  /** EventSource implementation; tests pass a fake. */
  EventSource?: typeof EventSource
  url?: string
}

const TYPES = ['resource', 'operation', 'request', 'reset', 'gap'] as const

/**
 * connectEvents keeps client in step with the Instance and returns a
 * function that disconnects. EventSource resumes from the last event ID
 * by itself after a dropped connection; when it gives up (the Instance
 * was down), a new one is opened that passes the ID as lastEventId.
 */
export function connectEvents(client: QueryClient, opts: EventsOptions = {}): () => void {
  const ES = opts.EventSource ?? globalThis.EventSource
  const base = opts.url ?? new URL(EVENTS_PATH, origin()).href
  let lastId = ''
  let es: EventSource | undefined
  let reconnect: ReturnType<typeof setTimeout> | undefined
  let flush: ReturnType<typeof setTimeout> | undefined
  let pending = new Map<string, QueryKey>()
  let all = false
  let closed = false

  const flushNow = () => {
    flush = undefined
    if (all) {
      void client.invalidateQueries()
    } else {
      for (const queryKey of pending.values()) void client.invalidateQueries({ queryKey })
    }
    pending = new Map()
    all = false
  }

  const onEvent = (e: MessageEvent<string>) => {
    if (e.lastEventId) lastId = e.lastEventId
    let data: unknown
    try {
      data = JSON.parse(e.data)
    } catch {
      return
    }
    if (e.type === 'request') {
      appendRequest(client, data as RequestEntry)
      return
    }
    const keys = staleKeys(e.type, data)
    if (keys === 'all') all = true
    else for (const k of keys) pending.set(JSON.stringify(k), k)
    if ((all || pending.size > 0) && flush === undefined) flush = setTimeout(flushNow, FLUSH_MS)
  }

  const open = () => {
    reconnect = undefined
    const url = new URL(base)
    if (lastId) url.searchParams.set('lastEventId', lastId)
    const source = new ES(url.href)
    for (const t of TYPES) source.addEventListener(t, onEvent as EventListener)
    source.onerror = () => {
      if (source.readyState !== ES.CLOSED || closed) return
      // The browser stopped retrying: start over, resuming from lastId.
      reconnect ??= setTimeout(open, RECONNECT_MS)
    }
    es = source
  }

  open()
  return () => {
    closed = true
    es?.close()
    clearTimeout(reconnect)
    clearTimeout(flush)
  }
}
