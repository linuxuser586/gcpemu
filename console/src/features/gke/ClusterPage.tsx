import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Download, Pencil, Trash2 } from 'lucide-react'
import { Outlet, useOutletContext } from 'react-router'

import { Link, NavLink, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import { toast } from '@/lib/toast'
import { cn } from '@/lib/utils'

import { clusterQuery, deleteCluster, isRunning, type Cluster } from './api'
import { ClusterStatusBadge } from './format'
import { clusterPath, useClusterRef } from './GkeLayout'
import { buildKubeconfig, contextName, downloadText } from './kubeconfig'

const TABS = [
  ['', 'Overview'],
  ['nodePools', 'Node pools'],
  ['nodes', 'Nodes'],
  ['namespaces', 'Namespaces'],
  ['workloads', 'Workloads'],
  ['pods', 'Pods'],
  ['negs', 'NEGs'],
] as const

/**
 * ClusterPage is one cluster: its status and actions (download
 * kubeconfig, edit, delete) above tabs for its node pools, its
 * Kubernetes objects and its NEGs.
 */
export function ClusterPage() {
  const ref = useClusterRef()
  const query = useQuery(clusterQuery(ref))
  const c = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const del = useMutation({
    mutationFn: () => deleteCluster(ref),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['gke'] })
      toast.success(`Deleting cluster ${ref.cluster}.`)
      navigate('/gke')
    },
  })
  const base = clusterPath(ref.location, ref.cluster)

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to="/gke" className="underline-offset-4 hover:underline">
              Clusters
            </Link>{' '}
            / <span className="font-mono">{ref.location}</span>
          </p>
          <h1 className="flex items-center gap-3 text-2xl font-semibold">
            <span className="font-mono">{ref.cluster}</span>
            {c && <ClusterStatusBadge status={c.status} />}
          </h1>
        </div>
        {c && (
          <div className="flex flex-wrap gap-2">
            <Button
              variant="outline"
              disabled={!isRunning(c) || !c.endpoint}
              title={isRunning(c) ? undefined : 'The cluster is not running'}
              onClick={() =>
                downloadText(
                  `kubeconfig-${contextName(ref.project, c)}.yaml`,
                  buildKubeconfig(ref.project, c),
                )
              }
            >
              <Download aria-hidden />
              Download kubeconfig
            </Button>
            <Button variant="outline" asChild>
              <Link to={`${base}/edit`}>
                <Pencil aria-hidden />
                Edit
              </Link>
            </Button>
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Delete
                </Button>
              }
              title={`Delete cluster ${ref.cluster}?`}
              description="Its control plane, nodes, workloads and NEGs are removed. This cannot be undone."
              confirmLabel="Delete cluster"
              onConfirm={() => del.mutateAsync()}
            />
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {c?.status === 'ERROR' && c.statusMessage && (
        <p
          role="alert"
          className="rounded-md border border-destructive/50 p-3 text-sm text-destructive"
        >
          {c.statusMessage}
        </p>
      )}
      {c && (
        <>
          <nav aria-label="Cluster" className="flex flex-wrap gap-1 border-b">
            {TABS.map(([path, label]) => (
              <NavLink
                key={path}
                to={path ? `${base}/${path}` : base}
                end={!path}
                className={({ isActive }) =>
                  cn(
                    '-mb-px border-b-2 px-3 py-2 text-sm font-medium outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50',
                    isActive
                      ? 'border-primary text-foreground'
                      : 'border-transparent text-muted-foreground hover:text-foreground',
                  )
                }
              >
                {label}
              </NavLink>
            ))}
          </nav>
          <Outlet context={c} />
        </>
      )}
    </div>
  )
}

const time = (iso?: string) => (iso ? new Date(iso).toLocaleString() : undefined)

/** useCluster is the cluster a ClusterPage tab belongs to. */
export const useCluster = () => useOutletContext<Cluster>()

/** ClusterOverview shows the cluster's fields and the resource as JSON. */
export function ClusterOverview() {
  const c = useCluster()
  const labels = Object.entries(c.resourceLabels ?? {})
  return (
    <div className="flex flex-col gap-6">
      <Card>
        <CardTitle>Details</CardTitle>
        <DetailList
          label="Cluster details"
          rows={[
            ['Location', <Mono>{c.location}</Mono>],
            ['Node zones', c.locations?.length ? <Mono>{c.locations.join(', ')}</Mono> : undefined],
            ['Endpoint', c.endpoint && <Mono>{c.endpoint}</Mono>],
            [
              'Control plane version',
              c.currentMasterVersion && <Mono>{c.currentMasterVersion}</Mono>,
            ],
            ['Node version', c.currentNodeVersion && <Mono>{c.currentNodeVersion}</Mono>],
            ['Nodes', String(c.currentNodeCount ?? 0)],
            ['Release channel', c.releaseChannel?.channel],
            [
              'Workload Identity',
              c.workloadIdentityConfig?.workloadPool && (
                <Mono>{c.workloadIdentityConfig.workloadPool}</Mono>
              ),
            ],
            ['Private nodes', c.privateClusterConfig?.enablePrivateNodes ? 'Yes' : 'No'],
            ['Network', c.network && <Mono>{c.network}</Mono>],
            ['Subnetwork', c.subnetwork && <Mono>{c.subnetwork}</Mono>],
            ['Pod range', c.clusterIpv4Cidr && <Mono>{c.clusterIpv4Cidr}</Mono>],
            ['Service range', c.servicesIpv4Cidr && <Mono>{c.servicesIpv4Cidr}</Mono>],
            ['Logging service', c.loggingService && <Mono>{c.loggingService}</Mono>],
            ['Monitoring service', c.monitoringService && <Mono>{c.monitoringService}</Mono>],
            [
              'Labels',
              labels.length > 0 && <Mono>{labels.map(([k, v]) => `${k}=${v}`).join(', ')}</Mono>,
            ],
            ['Created', time(c.createTime)],
          ]}
        />
      </Card>
      <Card>
        <CardTitle>Cluster resource</CardTitle>
        <JsonView value={c} label="Cluster JSON" />
      </Card>
    </div>
  )
}

/** NotRunning explains why a cluster's Kubernetes objects are not shown. */
export function NotRunning({ c }: { c: Cluster }) {
  return (
    <Card role="status">
      <p className="text-sm text-muted-foreground">
        The cluster is {c.status ?? 'not running'}. Its Kubernetes objects are shown once it is
        RUNNING.
      </p>
    </Card>
  )
}
