import { queryOptions } from '@tanstack/react-query'

import { ApiError } from '@/api/errors'
import { gcpFetch, gcpText } from '@/api/fetch'
import { POLL_MS } from '@/api/queries'

// The GKE view's reads and writes: the container v1 API for clusters,
// node pools and Operations, the Connect gateway (ADR 0002) for each
// cluster's Kubernetes objects, and the compute API for the NEGs GKE
// creates (FR-GKE-008). Types cover the fields the view reads; the raw
// JSON editor covers the rest.

export type ClusterStatus =
  | 'STATUS_UNSPECIFIED'
  | 'PROVISIONING'
  | 'RUNNING'
  | 'RECONCILING'
  | 'STOPPING'
  | 'ERROR'
  | 'DEGRADED'

export type TaintEffect = 'NO_SCHEDULE' | 'PREFER_NO_SCHEDULE' | 'NO_EXECUTE'

export interface Taint {
  key: string
  value?: string
  effect: TaintEffect
}

export interface NodeConfig {
  machineType?: string
  diskSizeGb?: number
  imageType?: string
  serviceAccount?: string
  labels?: Record<string, string>
  taints?: Taint[]
}

export interface NodePool {
  name: string
  config?: NodeConfig
  initialNodeCount?: number
  locations?: string[]
  version?: string
  status?: string
  statusMessage?: string
  instanceGroupUrls?: string[]
  autoscaling?: { enabled?: boolean; minNodeCount?: number; maxNodeCount?: number }
  etag?: string
  selfLink?: string
}

export interface Cluster {
  name: string
  id?: string
  description?: string
  location: string
  locations?: string[]
  status?: ClusterStatus
  statusMessage?: string
  endpoint?: string
  initialClusterVersion?: string
  currentMasterVersion?: string
  currentNodeVersion?: string
  currentNodeCount?: number
  createTime?: string
  nodePools?: NodePool[]
  resourceLabels?: Record<string, string>
  labelFingerprint?: string
  masterAuth?: { clusterCaCertificate?: string }
  releaseChannel?: { channel?: string }
  workloadIdentityConfig?: { workloadPool?: string }
  privateClusterConfig?: { enablePrivateNodes?: boolean; enablePrivateEndpoint?: boolean }
  network?: string
  subnetwork?: string
  clusterIpv4Cidr?: string
  servicesIpv4Cidr?: string
  loggingService?: string
  monitoringService?: string
  selfLink?: string
  etag?: string
}

/** ContainerOperation is the container API's own Operation resource. */
export interface ContainerOperation {
  name: string
  operationType?: string
  status?: 'STATUS_UNSPECIFIED' | 'PENDING' | 'RUNNING' | 'DONE' | 'ABORTING'
  statusMessage?: string
  location?: string
  zone?: string
  targetLink?: string
  error?: { code?: number; message?: string }
}

export interface ServerConfig {
  defaultClusterVersion?: string
  validMasterVersions?: string[]
  validNodeVersions?: string[]
  channels?: { channel: string; defaultVersion?: string; validVersions?: string[] }[]
}

/** ClusterRef names a cluster, and optionally one of its node pools. */
export interface ClusterRef {
  project: string
  location: string
  cluster: string
}

export const RELEASE_CHANNELS = ['RAPID', 'REGULAR', 'STABLE', 'EXTENDED', 'UNSPECIFIED'] as const

/** NAME is GKE's cluster and node pool name rule, with its own message. */
export const NAME = /^[a-z](?:[-a-z0-9]{0,38}[a-z0-9])?$/
export const nameMessage = (kind: 'cluster' | 'nodePool', name: string) =>
  `Invalid value for field '${kind}.name': "${name}". Must match regex '(?:[a-z](?:[-a-z0-9]{0,38}[a-z0-9])?)'.`

const API = '/container/v1/'

export const parentName = (project: string, location: string) =>
  `projects/${project}/locations/${location}`
export const clusterName = (r: ClusterRef) =>
  `${parentName(r.project, r.location)}/clusters/${r.cluster}`
export const poolName = (r: ClusterRef, pool: string) => `${clusterName(r)}/nodePools/${pool}`

/** isRunning reports whether a cluster's Kubernetes API can be reached. */
export const isRunning = (c?: Cluster) => c?.status === 'RUNNING' || c?.status === 'RECONCILING'

// ---- container v1 ----

export const listClusters = async (project: string) =>
  (await gcpFetch<{ clusters?: Cluster[] }>(`${API}${parentName(project, '-')}/clusters`))
    .clusters ?? []

export const getCluster = (r: ClusterRef) => gcpFetch<Cluster>(API + clusterName(r))

const post = <T>(path: string, body: unknown, method = 'POST') =>
  gcpFetch<T>(API + path, { method, body: JSON.stringify(body) })

export const createCluster = (project: string, location: string, body: unknown) =>
  post<ContainerOperation>(`${parentName(project, location)}/clusters`, body)

export const updateCluster = (r: ClusterRef, body: unknown) =>
  post<ContainerOperation>(clusterName(r), body, 'PUT')

export const setResourceLabels = (
  r: ClusterRef,
  resourceLabels: Record<string, string>,
  labelFingerprint?: string,
) =>
  post<ContainerOperation>(`${clusterName(r)}:setResourceLabels`, {
    resourceLabels,
    labelFingerprint,
  })

export const deleteCluster = (r: ClusterRef) =>
  gcpFetch<ContainerOperation>(API + clusterName(r), { method: 'DELETE' })

export const createNodePool = (r: ClusterRef, body: unknown) =>
  post<ContainerOperation>(`${clusterName(r)}/nodePools`, body)

export const updateNodePool = (r: ClusterRef, pool: string, body: unknown) =>
  post<ContainerOperation>(poolName(r, pool), body, 'PUT')

export const setNodePoolSize = (r: ClusterRef, pool: string, nodeCount: number) =>
  post<ContainerOperation>(`${poolName(r, pool)}:setSize`, { nodeCount })

export const deleteNodePool = (r: ClusterRef, pool: string) =>
  gcpFetch<ContainerOperation>(API + poolName(r, pool), { method: 'DELETE' })

/** OP_POLL_MS is how often waitOperation checks an Operation. */
export const OP_POLL_MS = 500

/**
 * waitOperation resolves once op is DONE, or rejects with its error. Edits
 * that take several calls (labels, then the rest) wait between them, since
 * a cluster runs one Operation at a time.
 */
export async function waitOperation(project: string, op: ContainerOperation) {
  const location = op.location ?? op.zone ?? '-'
  let cur = op
  while (cur.status !== 'DONE') {
    await new Promise((r) => setTimeout(r, OP_POLL_MS))
    cur = await gcpFetch<ContainerOperation>(
      `${API}${parentName(project, location)}/operations/${op.name}`,
    )
  }
  if (cur.error?.message) {
    throw new ApiError(400, 'FAILED_PRECONDITION', cur.error.message)
  }
  return cur
}

export const clustersQuery = (project: string) =>
  queryOptions({
    queryKey: ['gke', 'clusters', project],
    queryFn: () => listClusters(project),
  })

export const clusterQuery = (r: ClusterRef) =>
  queryOptions({
    queryKey: ['gke', 'cluster', r],
    queryFn: () => getCluster(r),
  })

export const serverConfigQuery = (project: string, location: string) =>
  queryOptions({
    queryKey: ['gke', 'serverConfig', project, location],
    queryFn: () => gcpFetch<ServerConfig>(`${API}${parentName(project, location)}/serverConfig`),
    staleTime: Infinity,
  })

// ---- Kubernetes through the Connect gateway ----

export interface ObjectMeta {
  name: string
  namespace?: string
  uid?: string
  creationTimestamp?: string
  deletionTimestamp?: string
  labels?: Record<string, string>
  annotations?: Record<string, string>
  ownerReferences?: { kind: string; name: string }[]
}

export interface KubeList<T> {
  items: T[]
}

export interface KubeNode {
  metadata: ObjectMeta
  spec?: { unschedulable?: boolean; taints?: { key: string; value?: string; effect: string }[] }
  status?: {
    conditions?: { type: string; status: string; reason?: string; message?: string }[]
    addresses?: { type: string; address: string }[]
    nodeInfo?: { kubeletVersion?: string; architecture?: string; containerRuntimeVersion?: string }
  }
}

export interface KubeNamespace {
  metadata: ObjectMeta
  status?: { phase?: string }
}

export interface ContainerStatus {
  name: string
  ready: boolean
  restartCount: number
  image?: string
  state?: {
    waiting?: { reason?: string; message?: string }
    running?: { startedAt?: string }
    terminated?: { reason?: string; exitCode?: number }
  }
}

export interface KubePod {
  metadata: ObjectMeta
  spec?: { nodeName?: string; containers: { name: string; image?: string }[] }
  status?: {
    phase?: string
    reason?: string
    podIP?: string
    containerStatuses?: ContainerStatus[]
    initContainerStatuses?: ContainerStatus[]
  }
}

export interface KubeWorkload {
  kind: string
  metadata: ObjectMeta
  spec?: { replicas?: number; completions?: number; suspend?: boolean }
  status?: {
    replicas?: number
    readyReplicas?: number
    availableReplicas?: number
    updatedReplicas?: number
    // DaemonSet
    desiredNumberScheduled?: number
    numberReady?: number
    // Job
    succeeded?: number
    failed?: number
    active?: number
  }
}

/** gatewayPath is a Kubernetes API path through the Connect gateway. */
export const gatewayPath = (r: ClusterRef, path: string) =>
  `/connectgateway/v1/projects/${r.project}/locations/${r.location}/gkeMemberships/${r.cluster}${path}`

/** kubeQuery reads one Kubernetes API path; it polls, as no events report it. */
export const kubeQuery = <T>(r: ClusterRef, path: string, enabled = true) =>
  queryOptions({
    queryKey: ['gke', 'k8s', r, path],
    queryFn: () => gcpFetch<T>(gatewayPath(r, path)),
    refetchInterval: POLL_MS,
    enabled,
  })

/** nsPath scopes a collection to a namespace, or to all of them. */
export const nsPath = (prefix: string, resource: string, namespace?: string) =>
  namespace ? `${prefix}/namespaces/${namespace}/${resource}` : `${prefix}/${resource}`

export const WORKLOAD_KINDS = [
  { kind: 'Deployment', prefix: '/apis/apps/v1', resource: 'deployments' },
  { kind: 'StatefulSet', prefix: '/apis/apps/v1', resource: 'statefulsets' },
  { kind: 'DaemonSet', prefix: '/apis/apps/v1', resource: 'daemonsets' },
  { kind: 'Job', prefix: '/apis/batch/v1', resource: 'jobs' },
] as const

export interface LogOptions {
  container?: string
  tailLines: number
  previous?: boolean
}

export const podLogQuery = (
  r: ClusterRef,
  namespace: string,
  pod: string,
  opts: LogOptions,
  follow: boolean,
) => {
  const q = new URLSearchParams({ tailLines: String(opts.tailLines), timestamps: 'true' })
  if (opts.container) q.set('container', opts.container)
  if (opts.previous) q.set('previous', 'true')
  const path = `/api/v1/namespaces/${namespace}/pods/${pod}/log?${q}`
  return queryOptions({
    queryKey: ['gke', 'logs', r, path],
    queryFn: () => gcpText(gatewayPath(r, path)),
    refetchInterval: follow ? POLL_MS : false,
  })
}

// ---- NEGs (compute) ----

export interface NEG {
  name: string
  zone?: string
  description?: string
  size?: number
  networkEndpointType?: string
  network?: string
  selfLink?: string
}

/** negOwner is the Kubernetes Service a GKE NEG's description names. */
export interface NegOwner {
  clusterUid: string
  namespace: string
  service: string
  port: string
}

const str = (v: unknown) => (typeof v === 'string' ? v : '')

export function negOwner(neg: NEG): NegOwner | undefined {
  try {
    const d = JSON.parse(neg.description ?? '') as Record<string, unknown>
    if (typeof d['cluster-uid'] !== 'string') return undefined
    return {
      clusterUid: d['cluster-uid'],
      namespace: str(d.namespace),
      service: str(d['service-name']),
      port: str(d.port),
    }
  } catch {
    return undefined
  }
}

export const negsQuery = (project: string, enabled = true) =>
  queryOptions({
    queryKey: ['compute', 'negs', project],
    queryFn: async () => {
      const res = await gcpFetch<{ items?: Record<string, { networkEndpointGroups?: NEG[] }> }>(
        `/compute/v1/projects/${project}/aggregated/networkEndpointGroups`,
      )
      return Object.values(res.items ?? {}).flatMap((s) => s.networkEndpointGroups ?? [])
    },
    enabled,
  })
