import { useQueries, useQuery, type UseQueryResult } from '@tanstack/react-query'
import { ScrollText } from 'lucide-react'
import { useState } from 'react'
import { useSearchParams } from 'react-router'

import { errorMessage } from '@/api/errors'
import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { useViewState } from '@/lib/viewState'

import {
  isRunning,
  kubeQuery,
  nsPath,
  WORKLOAD_KINDS,
  type KubeList,
  type KubeNamespace,
  type KubeNode,
  type KubePod,
  type KubeWorkload,
} from './api'
import { NotRunning, useCluster } from './ClusterPage'
import { age, podReady, podRestarts, podStatus, StatusBadge, workloadReady } from './format'
import { clusterPath, useClusterRef } from './GkeLayout'

// The cluster's Kubernetes objects through the Connect gateway (ADR 0002):
// nodes, namespaces, workloads and pods. No events report them, so the
// queries poll. ?namespace= scopes workloads and pods, ?q= filters names.

const NODEPOOL_LABEL = 'cloud.google.com/gke-nodepool'
const ZONE_LABEL = 'topology.kubernetes.io/zone'

const linkClass = 'font-mono text-primary underline-offset-4 hover:underline'

function Empty({ children }: { children: React.ReactNode }) {
  return <p className="text-sm text-muted-foreground">{children}</p>
}

/** useNameFilter is the ?q= name filter, owned by its input while typing. */
function useNameFilter(): [string, (v: string) => void] {
  const [view, setView] = useViewState()
  const [text, setText] = useState(view.q ?? '')
  return [
    text,
    (v) => {
      setText(v)
      setView({ q: v })
    },
  ]
}

function NameFilter({
  value,
  onChange,
  label,
}: {
  value: string
  onChange: (v: string) => void
  label: string
}) {
  return (
    <label htmlFor="kube-filter" className="flex min-w-48 flex-1 flex-col gap-1 text-sm">
      Filter
      <Input
        id="kube-filter"
        type="search"
        placeholder={label}
        value={value}
        onChange={(e) => onChange(e.target.value)}
      />
    </label>
  )
}

/** NamespaceSelect sets ?namespace=, listing the cluster's namespaces. */
function NamespaceSelect() {
  const ref = useClusterRef()
  const [search, setSearch] = useSearchParams()
  const namespaces = useQuery(kubeQuery<KubeList<KubeNamespace>>(ref, '/api/v1/namespaces'))
  const current = search.get('namespace') ?? ''
  const names = namespaces.data?.items.map((n) => n.metadata.name) ?? []
  if (current && !names.includes(current)) names.push(current)
  return (
    <label htmlFor="kube-namespace" className="flex flex-col gap-1 text-sm">
      Namespace
      <NativeSelect
        id="kube-namespace"
        value={current}
        onChange={(e) =>
          setSearch((prev) => {
            const next = new URLSearchParams(prev)
            if (e.target.value) next.set('namespace', e.target.value)
            else next.delete('namespace')
            return next
          })
        }
      >
        <option value="">All namespaces</option>
        {names.map((n) => (
          <option key={n} value={n}>
            {n}
          </option>
        ))}
      </NativeSelect>
    </label>
  )
}

function Filters({ children }: { children: React.ReactNode }) {
  return (
    <form
      role="search"
      aria-label="Filter"
      className="flex flex-wrap items-end gap-3"
      onSubmit={(e) => e.preventDefault()}
    >
      {children}
    </form>
  )
}

// ---- nodes ----

function nodeReady(n: KubeNode): { status: string; tone: 'success' | 'destructive' | 'warning' } {
  const ready = n.status?.conditions?.find((c) => c.type === 'Ready')
  const base = ready?.status === 'True' ? 'Ready' : 'NotReady'
  if (n.spec?.unschedulable) return { status: `${base},SchedulingDisabled`, tone: 'warning' }
  return { status: base, tone: base === 'Ready' ? 'success' : 'destructive' }
}

/** NodesTable lists the cluster's nodes, or one node pool's. */
export function NodesTable({ pool }: { pool?: string }) {
  const ref = useClusterRef()
  const c = useCluster()
  const query = useQuery(kubeQuery<KubeList<KubeNode>>(ref, '/api/v1/nodes', isRunning(c)))
  if (!isRunning(c)) return <NotRunning c={c} />
  const nodes = (query.data?.items ?? []).filter(
    (n) => !pool || n.metadata.labels?.[NODEPOOL_LABEL] === pool,
  )
  return (
    <>
      <QueryStatus query={query} />
      {query.data && nodes.length === 0 && <Empty>No nodes.</Empty>}
      {nodes.length > 0 && (
        <Table aria-label="Nodes">
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Node pool</TableHead>
              <TableHead>Zone</TableHead>
              <TableHead>Internal IP</TableHead>
              <TableHead>Version</TableHead>
              <TableHead>Age</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {nodes.map((n) => {
              const s = nodeReady(n)
              return (
                <TableRow key={n.metadata.name} data-testid="node">
                  <TableCell className="font-mono">{n.metadata.name}</TableCell>
                  <TableCell>
                    <StatusBadge {...s} />
                  </TableCell>
                  <TableCell className="font-mono">
                    {n.metadata.labels?.[NODEPOOL_LABEL] ?? '—'}
                  </TableCell>
                  <TableCell className="font-mono">
                    {n.metadata.labels?.[ZONE_LABEL] ?? '—'}
                  </TableCell>
                  <TableCell className="font-mono">
                    {n.status?.addresses?.find((a) => a.type === 'InternalIP')?.address ?? '—'}
                  </TableCell>
                  <TableCell className="font-mono">
                    {n.status?.nodeInfo?.kubeletVersion ?? '—'}
                  </TableCell>
                  <TableCell>{age(n.metadata.creationTimestamp)}</TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      )}
    </>
  )
}

export function Nodes() {
  return (
    <Card>
      <NodesTable />
    </Card>
  )
}

// ---- namespaces ----

export function Namespaces() {
  const ref = useClusterRef()
  const c = useCluster()
  const query = useQuery(
    kubeQuery<KubeList<KubeNamespace>>(ref, '/api/v1/namespaces', isRunning(c)),
  )
  const [text, setText] = useNameFilter()
  if (!isRunning(c)) return <NotRunning c={c} />
  const shown = (query.data?.items ?? []).filter((n) => n.metadata.name.includes(text))
  const base = clusterPath(ref.location, ref.cluster)
  return (
    <Card>
      <Filters>
        <NameFilter value={text} onChange={setText} label="Namespace name" />
      </Filters>
      <QueryStatus query={query} />
      {shown.length > 0 && (
        <Table aria-label="Namespaces">
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Age</TableHead>
              <TableHead>
                <span className="sr-only">Objects</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {shown.map((n) => {
              const phase = n.status?.phase ?? 'Unknown'
              const ns = encodeURIComponent(n.metadata.name)
              return (
                <TableRow key={n.metadata.name} data-testid="namespace">
                  <TableCell className="font-mono">{n.metadata.name}</TableCell>
                  <TableCell>
                    <StatusBadge status={phase} tone={phase === 'Active' ? 'success' : 'warning'} />
                  </TableCell>
                  <TableCell>{age(n.metadata.creationTimestamp)}</TableCell>
                  <TableCell className="flex gap-3 text-sm">
                    <Link className={linkClass} to={`${base}/workloads?namespace=${ns}`}>
                      Workloads
                    </Link>
                    <Link className={linkClass} to={`${base}/pods?namespace=${ns}`}>
                      Pods
                    </Link>
                  </TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}

// ---- workloads ----

/** firstError is the error of the first failed query, if any. */
function firstError(queries: UseQueryResult[]) {
  return queries.find((q) => q.isError)?.error
}

export function Workloads() {
  const ref = useClusterRef()
  const c = useCluster()
  const [search] = useSearchParams()
  const namespace = search.get('namespace') ?? undefined
  const [text, setText] = useNameFilter()
  const queries = useQueries({
    queries: WORKLOAD_KINDS.map((k) =>
      kubeQuery<KubeList<KubeWorkload>>(ref, nsPath(k.prefix, k.resource, namespace), isRunning(c)),
    ),
  })
  if (!isRunning(c)) return <NotRunning c={c} />
  // List items carry no kind; it is the list's.
  const all = queries.flatMap((q, i) =>
    (q.data?.items ?? []).map((w) => ({ ...w, kind: WORKLOAD_KINDS[i]!.kind })),
  )
  const shown = all
    .filter((w) => w.metadata.name.includes(text))
    .sort(
      (a, b) =>
        (a.metadata.namespace ?? '').localeCompare(b.metadata.namespace ?? '') ||
        a.metadata.name.localeCompare(b.metadata.name),
    )
  const loading = queries.some((q) => q.isPending)
  const error = firstError(queries)
  return (
    <Card>
      <Filters>
        <NamespaceSelect />
        <NameFilter value={text} onChange={setText} label="Workload name" />
      </Filters>
      {error !== undefined && (
        <p role="alert" className="text-sm text-destructive">
          {errorMessage(error)}
        </p>
      )}
      {loading && <QueryStatus query={queries.find((q) => q.isPending)!} />}
      {!loading && shown.length === 0 && <Empty>No workloads.</Empty>}
      {shown.length > 0 && (
        <Table aria-label="Workloads">
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Kind</TableHead>
              <TableHead>Namespace</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Ready</TableHead>
              <TableHead>Age</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {shown.map((w) => {
              const r = workloadReady(w)
              return (
                <TableRow
                  key={`${w.kind}/${w.metadata.namespace}/${w.metadata.name}`}
                  data-testid="workload"
                >
                  <TableCell className="font-mono">{w.metadata.name}</TableCell>
                  <TableCell>{w.kind}</TableCell>
                  <TableCell className="font-mono">{w.metadata.namespace}</TableCell>
                  <TableCell>
                    <StatusBadge
                      status={r.ok ? 'OK' : 'Not ready'}
                      tone={r.ok ? 'success' : 'warning'}
                    />
                  </TableCell>
                  <TableCell className="tabular-nums">{r.ready}</TableCell>
                  <TableCell>{age(w.metadata.creationTimestamp)}</TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}

// ---- pods ----

export function Pods() {
  const ref = useClusterRef()
  const c = useCluster()
  const [search] = useSearchParams()
  const namespace = search.get('namespace') ?? undefined
  const [text, setText] = useNameFilter()
  const query = useQuery(
    kubeQuery<KubeList<KubePod>>(ref, nsPath('/api/v1', 'pods', namespace), isRunning(c)),
  )
  if (!isRunning(c)) return <NotRunning c={c} />
  const shown = (query.data?.items ?? [])
    .filter((p) => p.metadata.name.includes(text))
    .sort(
      (a, b) =>
        (a.metadata.namespace ?? '').localeCompare(b.metadata.namespace ?? '') ||
        a.metadata.name.localeCompare(b.metadata.name),
    )
  const base = clusterPath(ref.location, ref.cluster)
  return (
    <Card>
      <Filters>
        <NamespaceSelect />
        <NameFilter value={text} onChange={setText} label="Pod name" />
      </Filters>
      <QueryStatus query={query} />
      {query.data && shown.length === 0 && <Empty>No pods.</Empty>}
      {shown.length > 0 && (
        <Table aria-label="Pods">
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Namespace</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Ready</TableHead>
              <TableHead className="text-right">Restarts</TableHead>
              <TableHead>Node</TableHead>
              <TableHead>IP</TableHead>
              <TableHead>Age</TableHead>
              <TableHead>
                <span className="sr-only">Logs</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {shown.map((p) => {
              const s = podStatus(p)
              const ns = p.metadata.namespace ?? 'default'
              return (
                <TableRow key={`${ns}/${p.metadata.name}`} data-testid="pod">
                  <TableCell className="font-mono">{p.metadata.name}</TableCell>
                  <TableCell className="font-mono">{ns}</TableCell>
                  <TableCell>
                    <StatusBadge {...s} />
                  </TableCell>
                  <TableCell className="tabular-nums">{podReady(p)}</TableCell>
                  <TableCell className="text-right tabular-nums">{podRestarts(p)}</TableCell>
                  <TableCell className="font-mono">{p.spec?.nodeName ?? '—'}</TableCell>
                  <TableCell className="font-mono">{p.status?.podIP ?? '—'}</TableCell>
                  <TableCell>{age(p.metadata.creationTimestamp)}</TableCell>
                  <TableCell>
                    <Link
                      className="inline-flex items-center gap-1 text-sm text-primary underline-offset-4 hover:underline"
                      to={`${base}/pods/${encodeURIComponent(ns)}/${encodeURIComponent(p.metadata.name)}`}
                      aria-label={`Logs of ${p.metadata.name}`}
                    >
                      <ScrollText className="size-4" aria-hidden />
                      Logs
                    </Link>
                  </TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}
