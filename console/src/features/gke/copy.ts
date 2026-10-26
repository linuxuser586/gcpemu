import {
  block,
  expr,
  gcloud,
  keyValues,
  omit,
  resource,
  tofuName,
  type HclValue,
  type Recipe,
} from '@/lib/copyAs'

import {
  clusterName,
  isAutopilot,
  parentName,
  type Cluster,
  type ClusterRef,
  type NodePool,
} from './api'
import { formatTaints } from './taints'

// Copy as (FR-UI-015) for clusters and node pools: gcloud container, the
// container v1 create request and the google_container_* resource.

const API = '/container/v1'

/** POOL_OUTPUT are a node pool's output-only fields. */
const POOL_OUTPUT = [
  'status',
  'statusMessage',
  'instanceGroupUrls',
  'managedInstanceGroupUrls',
  'podIpv4CidrSize',
  'conditions',
  'selfLink',
  'etag',
]

/**
 * CLUSTER_OUTPUT are a cluster's output-only fields, its location (in the
 * path) and the default pool's legacy fields, which the API refuses
 * alongside nodePools.
 */
const CLUSTER_OUTPUT = [
  'initialNodeCount',
  'nodeConfig',
  'id',
  'location',
  'zone',
  'status',
  'statusMessage',
  'endpoint',
  'currentMasterVersion',
  'currentNodeVersion',
  'currentNodeCount',
  'createTime',
  'expireTime',
  'nodeIpv4CidrSize',
  'servicesIpv4Cidr',
  'instanceGroupUrls',
  'conditions',
  'masterAuth',
  'labelFingerprint',
  'selfLink',
  'etag',
]

const poolBody = (np: NodePool) => omit(np, POOL_OUTPUT)

/** same reports whether two zone lists match, as a pool inherits its cluster's. */
const same = (a?: string[], b?: string[]) => (a ?? []).join() === (b ?? []).join()

/** nodeFlags are the gcloud flags for a pool's nodes, shared by clusters and node-pools create. */
function nodeFlags(np?: NodePool) {
  const cfg = np?.config ?? {}
  const auto = np?.autoscaling?.enabled ? np.autoscaling : undefined
  return {
    'num-nodes': np?.initialNodeCount,
    'machine-type': cfg.machineType,
    'disk-size': cfg.diskSizeGb,
    'image-type': cfg.imageType,
    'service-account': cfg.serviceAccount,
    'node-labels': keyValues(cfg.labels),
    'node-taints': formatTaints(cfg.taints).split('\n').filter(Boolean),
    'enable-autoscaling': !!auto,
    'min-nodes': auto?.minNodeCount,
    'max-nodes': auto?.maxNodeCount,
  }
}

/** poolGcloud is gcloud container node-pools create for np in cluster r. */
function poolGcloud(r: ClusterRef, np: NodePool, clusterZones?: string[]) {
  return gcloud(['container', 'node-pools', 'create', np.name], {
    project: r.project,
    cluster: r.cluster,
    location: r.location,
    'node-locations': same(np.locations, clusterZones) ? undefined : np.locations,
    'node-version': np.version,
    ...nodeFlags(np),
  })
}

/** poolTofu is a google_container_node_pool for np in the cluster written as cluster. */
function poolTofu(r: ClusterRef, np: NodePool, cluster: HclValue, clusterZones?: string[]) {
  const cfg = np.config ?? {}
  const auto = np.autoscaling?.enabled ? np.autoscaling : undefined
  return resource('google_container_node_pool', np.name, {
    project: r.project,
    name: np.name,
    cluster,
    location: r.location,
    node_locations: same(np.locations, clusterZones) ? undefined : np.locations,
    version: np.version,
    initial_node_count: np.initialNodeCount,
    autoscaling: auto
      ? block({ min_node_count: auto.minNodeCount ?? 0, max_node_count: auto.maxNodeCount ?? 0 })
      : undefined,
    node_config: np.config
      ? block({
          machine_type: cfg.machineType,
          disk_size_gb: cfg.diskSizeGb,
          image_type: cfg.imageType,
          service_account: cfg.serviceAccount,
          labels: cfg.labels,
          taint: (cfg.taints ?? []).map((t) =>
            block({ key: t.key, value: t.value, effect: t.effect }),
          ),
        })
      : undefined,
  })
}

export function nodePoolRecipe(r: ClusterRef, np: NodePool, clusterZones?: string[]): Recipe {
  return {
    gcloud: poolGcloud(r, np, clusterZones),
    rest: {
      method: 'POST',
      path: `${API}/${clusterName(r)}/nodePools`,
      body: { nodePool: poolBody(np) },
    },
    tofu: poolTofu(r, np, r.cluster, clusterZones),
  }
}

/**
 * clusterRecipe creates the cluster with its first node pool as the
 * default pool, then adds the others. OpenTofu manages every pool as a
 * google_container_node_pool, removing the default one, as the provider
 * recommends. An Autopilot cluster is created without node pools, as GKE
 * manages them.
 */
export function clusterRecipe(project: string, c: Cluster): Recipe {
  const r: ClusterRef = { project, location: c.location, cluster: c.name }
  const autopilot = isAutopilot(c)
  const pools = autopilot ? [] : (c.nodePools ?? [])
  const [first, ...rest] = pools
  const zones = autopilot || same(c.locations, [c.location]) ? undefined : c.locations
  const channel = c.releaseChannel?.channel
  // gcloud's create-auto keeps nodes and endpoint private in networkConfig
  // and controlPlaneEndpointsConfig rather than privateClusterConfig.
  const privateNodes =
    c.privateClusterConfig?.enablePrivateNodes || c.networkConfig?.defaultEnablePrivateNodes
  const privateEndpoint =
    c.privateClusterConfig?.enablePrivateEndpoint ||
    c.controlPlaneEndpointsConfig?.ipEndpointsConfig?.enablePublicEndpoint === false
  const netAdmin = c.autopilot?.workloadPolicyConfig?.allowNetAdmin
  const ref = expr(`google_container_cluster.${tofuName(c.name)}.name`)
  const cluster = resource('google_container_cluster', c.name, {
    project,
    name: c.name,
    location: c.location,
    description: c.description,
    node_locations: zones,
    min_master_version: c.initialClusterVersion,
    network: c.network,
    subnetwork: c.subnetwork,
    resource_labels: c.resourceLabels,
    ...(autopilot
      ? { enable_autopilot: true, allow_net_admin: netAdmin }
      : { remove_default_node_pool: true, initial_node_count: 1 }),
    release_channel: channel ? block({ channel }) : undefined,
    // Autopilot sets the project's workload pool itself.
    workload_identity_config:
      !autopilot && c.workloadIdentityConfig?.workloadPool
        ? block({ workload_pool: c.workloadIdentityConfig.workloadPool })
        : undefined,
    private_cluster_config: privateNodes
      ? block({ enable_private_nodes: true, enable_private_endpoint: privateEndpoint })
      : undefined,
  })
  const flags = {
    project,
    location: c.location,
    'node-locations': zones,
    'cluster-version': c.initialClusterVersion,
    'release-channel': channel === 'UNSPECIFIED' ? 'None' : channel?.toLowerCase(),
    network: c.network,
    subnetwork: c.subnetwork,
    labels: keyValues(c.resourceLabels),
    'workload-pool': autopilot ? undefined : c.workloadIdentityConfig?.workloadPool,
    'enable-private-nodes': privateNodes,
    'enable-private-endpoint': privateEndpoint,
  }
  const body = omit(c, CLUSTER_OUTPUT)
  return {
    gcloud: autopilot
      ? [
          gcloud(['container', 'clusters', 'create-auto', c.name], {
            ...flags,
            'workload-policies': netAdmin ? 'allow-net-admin' : undefined,
          }),
        ]
      : [
          gcloud(['container', 'clusters', 'create', c.name], { ...flags, ...nodeFlags(first) }),
          ...rest.map((np) => poolGcloud(r, np, c.locations)),
        ],
    rest: {
      method: 'POST',
      path: `${API}/${parentName(project, c.location)}/clusters`,
      body: {
        cluster: autopilot
          ? omit(body, ['nodePools'])
          : { ...body, nodePools: c.nodePools?.map(poolBody) },
      },
    },
    tofu: [cluster, ...pools.map((np) => poolTofu(r, np, ref, c.locations))].join('\n\n'),
  }
}
