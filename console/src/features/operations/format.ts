import type { Operation } from '@/api/admin'

export type OperationState = 'running' | 'succeeded' | 'failed'

export function operationState(op: Operation): OperationState {
  if (!op.done) return 'running'
  return op.error ? 'failed' : 'succeeded'
}

/**
 * durationMs is how long op took, or has been running at now. It is
 * undefined when the Service did not record when op started.
 */
export function durationMs(op: Operation, now: number): number | undefined {
  if (!op.startTime) return undefined
  const start = Date.parse(op.startTime)
  const end = op.endTime ? Date.parse(op.endTime) : op.done ? undefined : now
  if (end === undefined || Number.isNaN(start) || Number.isNaN(end)) return undefined
  return Math.max(0, end - start)
}

/** formatDuration renders a duration compactly: "350 ms", "2.4 s", "3 m 05 s", "1 h 02 m". */
export function formatDuration(ms: number): string {
  if (ms < 1000) return `${Math.round(ms)} ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)} s`
  const s = Math.floor(ms / 1000)
  const pad = (n: number) => String(n).padStart(2, '0')
  if (s < 3600) return `${Math.floor(s / 60)} m ${pad(s % 60)} s`
  return `${Math.floor(s / 3600)} h ${pad(Math.floor(s / 60) % 60)} m`
}

/** matches reports whether op mentions text in its name, type or target. */
export function matches(op: Operation, text: string): boolean {
  const t = text.trim().toLowerCase()
  if (!t) return true
  return [op.name, op.type, op.target, op.error?.message].some((f) => f?.toLowerCase().includes(t))
}
