import { CircleAlert, CircleCheck, CircleX, LoaderCircle } from 'lucide-react'

import { Badge } from '@/components/ui/badge'

import type { ContainerStatus, KubePod, KubeWorkload } from './api'

type Tone = 'success' | 'warning' | 'destructive' | 'default'

const CLUSTER_TONES: Record<string, Tone> = {
  RUNNING: 'success',
  PROVISIONING: 'warning',
  RECONCILING: 'warning',
  STOPPING: 'warning',
  ERROR: 'destructive',
  DEGRADED: 'destructive',
}

/** StatusBadge shows a status in a tone, spinning while it is transitional. */
export function StatusBadge({ status, tone }: { status: string; tone: Tone }) {
  const Icon = {
    success: CircleCheck,
    warning: LoaderCircle,
    destructive: CircleX,
    default: CircleAlert,
  }[tone]
  return (
    <Badge variant={tone} data-status={status}>
      <Icon
        className={tone === 'warning' ? 'animate-spin motion-reduce:animate-none' : undefined}
        aria-hidden
      />
      {status}
    </Badge>
  )
}

export function ClusterStatusBadge({ status }: { status?: string }) {
  const s = status ?? 'STATUS_UNSPECIFIED'
  return <StatusBadge status={s} tone={CLUSTER_TONES[s] ?? 'default'} />
}

/** age is how long ago an RFC 3339 time was, like kubectl's AGE column. */
export function age(iso: string | undefined, now = Date.now()): string {
  if (!iso) return '—'
  const s = Math.max(0, Math.floor((now - new Date(iso).getTime()) / 1000))
  if (Number.isNaN(s)) return '—'
  if (s < 120) return `${s}s`
  const m = Math.floor(s / 60)
  if (m < 120) return `${m}m`
  const h = Math.floor(m / 60)
  if (h < 48) return `${h}h`
  return `${Math.floor(h / 24)}d`
}

function containerReason(cs: ContainerStatus[] = []): string | undefined {
  for (const c of cs) {
    const r = c.state?.waiting?.reason ?? c.state?.terminated?.reason
    if (r && r !== 'Completed') return r
  }
  return undefined
}

/**
 * podStatus is the STATUS kubectl shows for a pod: a container's waiting
 * or termination reason (CrashLoopBackOff, ImagePullBackOff, Error) wins
 * over the phase.
 */
export function podStatus(p: KubePod): { status: string; tone: Tone } {
  if (p.metadata.deletionTimestamp) {
    return { status: 'Terminating', tone: 'warning' }
  }
  const init = containerReason(p.status?.initContainerStatuses)
  if (init) return { status: `Init:${init}`, tone: 'destructive' }
  const reason = containerReason(p.status?.containerStatuses) ?? p.status?.reason
  const phase = p.status?.phase ?? 'Unknown'
  if (reason) return { status: reason, tone: 'destructive' }
  switch (phase) {
    case 'Running': {
      const cs = p.status?.containerStatuses ?? []
      return { status: 'Running', tone: cs.every((c) => c.ready) ? 'success' : 'warning' }
    }
    case 'Succeeded':
      return { status: 'Completed', tone: 'success' }
    case 'Pending':
      return { status: 'Pending', tone: 'warning' }
    case 'Failed':
      return { status: 'Failed', tone: 'destructive' }
    default:
      return { status: phase, tone: 'default' }
  }
}

export function podReady(p: KubePod): string {
  const cs = p.status?.containerStatuses ?? []
  const total = p.spec?.containers.length ?? cs.length
  return `${cs.filter((c) => c.ready).length}/${total}`
}

export function podRestarts(p: KubePod): number {
  return (p.status?.containerStatuses ?? []).reduce((n, c) => n + c.restartCount, 0)
}

/** workloadReady is a workload's READY column and whether it is healthy. */
export function workloadReady(w: KubeWorkload): { ready: string; ok: boolean } {
  const s = w.status ?? {}
  switch (w.kind) {
    case 'DaemonSet': {
      const want = s.desiredNumberScheduled ?? 0
      const have = s.numberReady ?? 0
      return { ready: `${have}/${want}`, ok: have === want }
    }
    case 'Job': {
      const want = w.spec?.completions ?? 1
      const have = s.succeeded ?? 0
      return { ready: `${have}/${want}`, ok: have >= want && !s.failed }
    }
    default: {
      const want = w.spec?.replicas ?? s.replicas ?? 0
      const have = s.readyReplicas ?? 0
      return { ready: `${have}/${want}`, ok: have === want }
    }
  }
}
