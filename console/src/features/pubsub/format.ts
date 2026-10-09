import { decodePayload } from '../secrets/api'
import type { PubsubMessage, SubscriptionStats } from './api'

// Formatting shared by the Pub/Sub view.

/** formatAge renders a duration in milliseconds the way people read an age. */
export function formatAge(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000))
  if (s < 60) return `${s} s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m} min`
  const h = Math.floor(m / 60)
  if (h < 24) return m % 60 ? `${h} h ${m % 60} min` : `${h} h`
  const d = Math.floor(h / 24)
  return h % 24 ? `${d} d ${h % 24} h` : `${d} d`
}

/** oldestAge is how long a subscription's oldest unacked message has waited, at now. */
export function oldestAge(s: SubscriptionStats | undefined, now: string): string | undefined {
  if (!s?.oldestUnackedPublishTime) return undefined
  return formatAge(Date.parse(now) - Date.parse(s.oldestUnackedPublishTime))
}

/**
 * messageData reads a message's data: the text when it is UTF-8,
 * otherwise the base64 the API sent, flagged as binary.
 */
export function messageData(m: PubsubMessage): { text: string; binary: boolean } {
  if (!m.data) return { text: '', binary: false }
  const text = decodePayload(m.data)
  return text === undefined ? { text: m.data, binary: true } : { text, binary: false }
}

/** pairs renders a map as key=value, comma-separated. */
export const pairs = (m?: Record<string, string>) =>
  Object.entries(m ?? {})
    .map(([k, v]) => `${k}=${v}`)
    .join(', ')
