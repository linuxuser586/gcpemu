import { queryOptions } from '@tanstack/react-query'

import { admin, unwrap } from '@/api/admin'
import type { components } from '@/api/admin.gen'
import { gcpFetch } from '@/api/fetch'

import { waitOperation as waitLong, type Operation as LongOperation } from '../certs/api'
import { relPath, waitOperation, type Operation, type Resource } from '../lb/api'

// The VPC network view's reads and writes: networks and subnetworks on
// compute v1, with their custom methods (switchToCustomMode,
// expandIpCidrRange, setPrivateIpGoogleAccess), firewall rules, routes,
// regional and global addresses and network peerings, and private
// services access connections on servicenetworking v1. The Host
// attachment (ADR 0014) is not modelled on GCP: its Offered ports are on
// the admin API only. Other views (Cloud NAT, Cloud DNS) read networks,
// subnetworks and addresses from here.

export type HostOffers = components['schemas']['HostOffers']
export type ContainerNetwork = components['schemas']['ContainerNetwork']

export interface Network extends Resource {
  autoCreateSubnetworks?: boolean
  mtu?: number
  routingConfig?: { routingMode?: 'REGIONAL' | 'GLOBAL'; [field: string]: unknown }
  subnetworks?: string[]
  gatewayIPv4?: string
  peerings?: NetworkPeering[]
}

/**
 * NetworkPeering is one of a network's VPC Network Peerings. It is ACTIVE
 * once the peer network peers back. Its route exchange settings are
 * Recorded: no routes are exchanged.
 */
export interface NetworkPeering {
  name: string
  network: string
  state?: 'ACTIVE' | 'INACTIVE'
  stateDetails?: string
  autoCreateRoutes?: boolean
  exchangeSubnetRoutes?: boolean
  exportCustomRoutes?: boolean
  importCustomRoutes?: boolean
  exportSubnetRoutesWithPublicIp?: boolean
  importSubnetRoutesWithPublicIp?: boolean
  stackType?: string
  peerMtu?: number
  [field: string]: unknown
}

/** PSA_PEERING is the peering a private services access connection adds to its network. */
export const PSA_PEERING = 'servicenetworking-googleapis-com'

/** PSA_SERVICE is the only service producer a private services access connection reaches. */
export const PSA_SERVICE = 'servicenetworking.googleapis.com'

/** Connection is a network's private services access connection (servicenetworking v1). */
export interface Connection {
  network: string
  reservedPeeringRanges?: string[]
  peering?: string
  service?: string
}

export interface SecondaryRange {
  rangeName: string
  ipCidrRange: string
}

export interface Subnetwork extends Resource {
  network?: string
  ipCidrRange?: string
  gatewayAddress?: string
  secondaryIpRanges?: SecondaryRange[]
  privateIpGoogleAccess?: boolean
  purpose?: string
  stackType?: string
  state?: string
}

/** FirewallRule is a firewall rule's allowed or denied protocol and ports. */
export interface FirewallRule {
  IPProtocol: string
  ports?: string[]
}

/** Firewall is a firewall rule. Its Fidelity is Recorded: stored and returned, not enforced. */
export interface Firewall extends Resource {
  network?: string
  direction?: 'INGRESS' | 'EGRESS'
  priority?: number
  allowed?: FirewallRule[]
  denied?: FirewallRule[]
  sourceRanges?: string[]
  destinationRanges?: string[]
  sourceTags?: string[]
  targetTags?: string[]
  sourceServiceAccounts?: string[]
  targetServiceAccounts?: string[]
  disabled?: boolean
  logConfig?: { enable?: boolean; [field: string]: unknown }
}

/** Route is a route. Its Fidelity is Recorded: stored and returned, not enforced. */
export interface Route extends Resource {
  network?: string
  destRange?: string
  priority?: number
  tags?: string[]
  nextHopGateway?: string
  nextHopIp?: string
  nextHopInstance?: string
  nextHopIlb?: string
  nextHopVpnTunnel?: string
  routeType?: string
}

/**
 * Address is a regional or global address: an external IP, an internal
 * IP of a subnetwork, or a global range reserved for private services
 * access (purpose VPC_PEERING, with prefixLength).
 */
export interface Address extends Resource {
  address?: string
  addressType?: 'EXTERNAL' | 'INTERNAL'
  purpose?: string
  network?: string
  subnetwork?: string
  prefixLength?: number
  networkTier?: string
  ipVersion?: string
  status?: string
  users?: string[]
  labels?: Record<string, string>
  labelFingerprint?: string
}

/** DEFAULT_PRIORITY is a firewall rule's or route's priority when it is sent none. */
export const DEFAULT_PRIORITY = 1000

/** priorityOf is a firewall rule's or route's priority. */
export const priorityOf = (r: { priority?: number }) => r.priority ?? DEFAULT_PRIORITY

/** byPriority orders firewall rules or routes as GCP weighs them: by priority, then name. */
export const byPriority = (a: Firewall | Route, b: Firewall | Route) =>
  priorityOf(a) - priorityOf(b) || a.name.localeCompare(b.name)

/** NetworkRef names a network: a Project and its name. */
export interface NetworkRef {
  project: string
  network: string
}

/** SubnetRef names a subnetwork: a Project, its region and its name. */
export interface SubnetRef {
  project: string
  region: string
  subnetwork: string
}

/** AddressRef names an address: a Project, its region ("" for global) and its name. */
export interface AddressRef {
  project: string
  region: string
  address: string
}

const API = '/compute/v1/'
const enc = encodeURIComponent

export const networkPath = (r: NetworkRef) =>
  `projects/${r.project}/global/networks/${enc(r.network)}`

export const subnetPath = (r: SubnetRef) =>
  `projects/${r.project}/regions/${r.region}/subnetworks/${enc(r.subnetwork)}`

type Aggregated<K extends string, T> = { items?: Record<string, Partial<Record<K, T[]>>> }

/** aggregated lists a regional collection of every region of a Project. */
export async function aggregated<K extends string, T>(project: string, coll: K): Promise<T[]> {
  const res = await gcpFetch<Aggregated<K, T>>(`${API}projects/${enc(project)}/aggregated/${coll}`)
  return Object.values(res.items ?? {}).flatMap((s) => s[coll] ?? [])
}

const write = async (path: string, method: string, body?: unknown) =>
  waitOperation(
    await gcpFetch<Operation>(API + path, {
      method,
      body: body === undefined ? undefined : JSON.stringify(body),
    }),
  )

// ---- networks ----

export const listNetworks = async (project: string) =>
  (await gcpFetch<{ items?: Network[] }>(`${API}projects/${enc(project)}/global/networks`)).items ??
  []

export const getNetwork = (r: NetworkRef) => gcpFetch<Network>(API + networkPath(r))

export const insertNetwork = (project: string, body: unknown) =>
  write(`projects/${project}/global/networks`, 'POST', body)

/** patchNetwork sends a JSON merge patch. */
export const patchNetwork = (r: NetworkRef, body: unknown) => write(networkPath(r), 'PATCH', body)

export const deleteNetwork = (r: NetworkRef) => write(networkPath(r), 'DELETE')

/** switchToCustomMode turns an auto mode network into a custom mode one, for good. */
export const switchToCustomMode = (r: NetworkRef) =>
  write(`${networkPath(r)}/switchToCustomMode`, 'POST')

// ---- subnetworks ----

export const listSubnetworks = (project: string) =>
  aggregated<'subnetworks', Subnetwork>(project, 'subnetworks')

export const getSubnetwork = (r: SubnetRef) => gcpFetch<Subnetwork>(API + subnetPath(r))

export const insertSubnetwork = (project: string, region: string, body: unknown) =>
  write(`projects/${project}/regions/${region}/subnetworks`, 'POST', body)

/** patchSubnetwork sends a JSON merge patch, which must carry the fingerprint. */
export const patchSubnetwork = (r: SubnetRef, body: unknown) => write(subnetPath(r), 'PATCH', body)

export const deleteSubnetwork = (r: SubnetRef) => write(subnetPath(r), 'DELETE')

export const expandIpCidrRange = (r: SubnetRef, ipCidrRange: string) =>
  write(`${subnetPath(r)}/expandIpCidrRange`, 'POST', { ipCidrRange })

export const setPrivateIpGoogleAccess = (r: SubnetRef, privateIpGoogleAccess: boolean) =>
  write(`${subnetPath(r)}/setPrivateIpGoogleAccess`, 'POST', { privateIpGoogleAccess })

// ---- firewall rules ----

export const firewallPath = (project: string, name: string) =>
  `projects/${project}/global/firewalls/${enc(name)}`

export const listFirewalls = async (project: string) =>
  (await gcpFetch<{ items?: Firewall[] }>(`${API}projects/${enc(project)}/global/firewalls`))
    .items ?? []

export const getFirewall = (project: string, name: string) =>
  gcpFetch<Firewall>(API + firewallPath(project, name))

export const insertFirewall = (project: string, body: unknown) =>
  write(`projects/${project}/global/firewalls`, 'POST', body)

/** patchFirewall sends a JSON merge patch: allowed replaces denied, and denied allowed. */
export const patchFirewall = (project: string, name: string, body: unknown) =>
  write(firewallPath(project, name), 'PATCH', body)

export const deleteFirewall = (project: string, name: string) =>
  write(firewallPath(project, name), 'DELETE')

// ---- routes ----

export const routePath = (project: string, name: string) =>
  `projects/${project}/global/routes/${enc(name)}`

export const listRoutes = async (project: string) =>
  (await gcpFetch<{ items?: Route[] }>(`${API}projects/${enc(project)}/global/routes`)).items ?? []

export const getRoute = (project: string, name: string) =>
  gcpFetch<Route>(API + routePath(project, name))

export const insertRoute = (project: string, body: unknown) =>
  write(`projects/${project}/global/routes`, 'POST', body)

/** deleteRoute deletes a route; routes are immutable, so there is no patch. */
export const deleteRoute = (project: string, name: string) =>
  write(routePath(project, name), 'DELETE')

// ---- peerings ----

/** addPeering peers network r with peering's network. */
export const addPeering = (r: NetworkRef, networkPeering: NetworkPeering) =>
  write(`${networkPath(r)}/addPeering`, 'POST', { networkPeering })

/** updatePeering changes a peering's route exchange; the peer network cannot change. */
export const updatePeering = (r: NetworkRef, networkPeering: NetworkPeering) =>
  write(`${networkPath(r)}/updatePeering`, 'PATCH', { networkPeering })

export const removePeering = (r: NetworkRef, name: string) =>
  write(`${networkPath(r)}/removePeering`, 'POST', { name })

// ---- private services access ----

const SERVICENETWORKING = '/servicenetworking/v1'

/** CONNECTIONS is the path of the service producer's private services access connections. */
export const CONNECTIONS = `${SERVICENETWORKING}/services/${PSA_SERVICE}/connections`

/** waitConnection waits for a servicenetworking Operation, a google.longrunning one. */
const waitConnection = async (res: Promise<LongOperation>) => waitLong(await res, SERVICENETWORKING)

export const listConnections = async (r: NetworkRef) =>
  (
    await gcpFetch<{ connections?: Connection[] }>(
      `${CONNECTIONS}?${new URLSearchParams({ network: networkPath(r) })}`,
    )
  ).connections ?? []

/** createConnection connects network r to the service producer over its allocated ranges. */
export const createConnection = (r: NetworkRef, reservedPeeringRanges: string[]) =>
  waitConnection(
    gcpFetch<LongOperation>(CONNECTIONS, {
      method: 'POST',
      body: JSON.stringify({ network: networkPath(r), reservedPeeringRanges }),
    }),
  )

/**
 * patchConnection replaces a connection's allocated ranges. force removes
 * a range even while producer services use it.
 */
export const patchConnection = (r: NetworkRef, reservedPeeringRanges: string[], force = false) =>
  waitConnection(
    gcpFetch<LongOperation>(
      `${CONNECTIONS}/${PSA_PEERING}?${new URLSearchParams({
        updateMask: 'reservedPeeringRanges',
        force: String(force),
      })}`,
      {
        method: 'PATCH',
        body: JSON.stringify({ network: networkPath(r), reservedPeeringRanges }),
      },
    ),
  )

export const deleteConnection = (r: NetworkRef) =>
  waitConnection(
    gcpFetch<LongOperation>(`${CONNECTIONS}/${PSA_PEERING}:deleteConnection`, {
      method: 'POST',
      body: JSON.stringify({ consumerNetwork: networkPath(r) }),
    }),
  )

// ---- addresses ----

/** addressesPath is the path of a region's addresses, or the global ones. */
const addressesPath = (project: string, region: string) =>
  `projects/${project}/${region ? `regions/${region}` : 'global'}/addresses`

export const addressPath = (r: AddressRef) =>
  `${addressesPath(r.project, r.region)}/${enc(r.address)}`

/** addressRefOf is the AddressRef of an address's selfLink. */
export function addressRefOf(link: string | undefined): AddressRef | undefined {
  const m = /^projects\/([^/]+)\/(?:global|regions\/([^/]+))\/addresses\/([^/]+)$/.exec(
    relPath(link),
  )
  return m ? { project: m[1]!, region: m[2] ?? '', address: decodeURIComponent(m[3]!) } : undefined
}

/**
 * listAddresses lists a Project's regional and global addresses, each
 * once: the aggregated list, which has the global ones too, and
 * global/addresses, so that they are listed even where it has not.
 */
export async function listAddresses(project: string): Promise<Address[]> {
  const [regional, global] = await Promise.all([
    aggregated<'addresses', Address>(project, 'addresses'),
    gcpFetch<{ items?: Address[] }>(`${API}projects/${enc(project)}/global/addresses`),
  ])
  const seen = new Set<string>()
  return [...regional, ...(global.items ?? [])].filter((a) => {
    const key = relPath(a.selfLink) || a.name
    if (seen.has(key)) return false
    seen.add(key)
    return true
  })
}

export const getAddress = (r: AddressRef) => gcpFetch<Address>(API + addressPath(r))

/** insertAddress reserves an address in region, or a global one for "". */
export const insertAddress = (project: string, region: string, body: unknown) =>
  write(addressesPath(project, region), 'POST', body)

export const deleteAddress = (r: AddressRef) => write(addressPath(r), 'DELETE')

/** setAddressLabels replaces an address's labels; the fingerprint must be its current one. */
export const setAddressLabels = (
  r: AddressRef,
  labels: Record<string, string>,
  labelFingerprint?: string,
) => write(`${addressPath(r)}/setLabels`, 'POST', { labels, labelFingerprint })

// ---- queries ----

// Keys are under "compute": the events bridge marks them stale when a
// network or subnetwork changes (FR-UI-006).

export const networksQuery = (project: string) =>
  queryOptions({
    queryKey: ['compute', 'vpc', 'networks', project],
    queryFn: () => listNetworks(project),
    enabled: !!project,
  })

export const networkQuery = (r: NetworkRef) =>
  queryOptions({
    queryKey: ['compute', 'vpc', 'network', r],
    queryFn: () => getNetwork(r),
  })

export const subnetsQuery = (project: string) =>
  queryOptions({
    queryKey: ['compute', 'vpc', 'subnetworks', project],
    queryFn: () => listSubnetworks(project),
    enabled: !!project,
  })

export const subnetQuery = (r: SubnetRef) =>
  queryOptions({
    queryKey: ['compute', 'vpc', 'subnetwork', r],
    queryFn: () => getSubnetwork(r),
  })

export const firewallsQuery = (project: string) =>
  queryOptions({
    queryKey: ['compute', 'vpc', 'firewalls', project],
    queryFn: () => listFirewalls(project),
    enabled: !!project,
  })

export const firewallQuery = (project: string, name: string) =>
  queryOptions({
    queryKey: ['compute', 'vpc', 'firewall', project, name],
    queryFn: () => getFirewall(project, name),
  })

export const routesQuery = (project: string) =>
  queryOptions({
    queryKey: ['compute', 'vpc', 'routes', project],
    queryFn: () => listRoutes(project),
    enabled: !!project,
  })

export const routeQuery = (project: string, name: string) =>
  queryOptions({
    queryKey: ['compute', 'vpc', 'route', project, name],
    queryFn: () => getRoute(project, name),
  })

export const connectionsQuery = (r: NetworkRef) =>
  queryOptions({
    queryKey: ['compute', 'vpc', 'connections', r],
    queryFn: () => listConnections(r),
    enabled: !!r.project,
  })

export const allAddressesQuery = (project: string) =>
  queryOptions({
    queryKey: ['compute', 'vpc', 'addresses', project],
    queryFn: () => listAddresses(project),
    enabled: !!project,
  })

export const addressQuery = (r: AddressRef) =>
  queryOptions({
    queryKey: ['compute', 'vpc', 'address', r],
    queryFn: () => getAddress(r),
  })

/** externalAddressesQuery lists a region's external addresses, which can be NAT IPs. */
export const externalAddressesQuery = (project: string, region: string) =>
  queryOptions({
    queryKey: ['compute', 'vpc', 'regionAddresses', project, region],
    queryFn: async () =>
      (
        (
          await gcpFetch<{ items?: Address[] }>(
            `${API}projects/${enc(project)}/regions/${enc(region)}/addresses`,
          )
        ).items ?? []
      ).filter((a) => (a.addressType ?? 'EXTERNAL') === 'EXTERNAL'),
    enabled: !!project && !!region,
  })

/** OFFERS_POLL_MS: offers change from the CLI, outside the console. */
const OFFERS_POLL_MS = 5_000

/** hostOffersQuery lists the subnetworks with Offered ports. */
export const hostOffersQuery = () =>
  queryOptions({
    queryKey: ['compute', 'hostOffers'],
    queryFn: async () => (await unwrap(admin.GET('/_emu/v1/host/offers'))).subnetworks,
    refetchInterval: OFFERS_POLL_MS,
  })

/**
 * CONTAINER_NETS_POLL_MS: Managed containers join and leave VPC container
 * networks as other Services (Cloud SQL, GKE) create and delete them,
 * which the compute events do not cover.
 */
const CONTAINER_NETS_POLL_MS = 10_000

/**
 * containerNetworksQuery lists a Project's VPC container networks, or one
 * network's. They are the emulator's own, on the admin API only.
 */
export const containerNetworksQuery = (project: string, network?: string) =>
  queryOptions({
    queryKey: ['compute', 'vpc', 'containerNetworks', project, network ?? ''],
    queryFn: async () =>
      (
        await unwrap(
          admin.GET('/_emu/v1/vpc/containerNetworks', {
            params: { query: { project, ...(network ? { network } : {}) } },
          }),
        )
      ).containerNetworks,
    enabled: !!project,
    refetchInterval: CONTAINER_NETS_POLL_MS,
  })

// ---- errors ----

/**
 * resourceField is the field a compute error message names, without
 * "resource.": "nats[0].minPortsPerVm" for "Invalid value for field
 * 'resource.nats[0].minPortsPerVm': ...".
 */
export function resourceField(message: string): string | undefined {
  return /field '?resource\.([^':]+)'?/.exec(message)?.[1]
}
