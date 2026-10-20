import { queryOptions } from '@tanstack/react-query'

import { admin, unwrap } from '@/api/admin'
import type { components } from '@/api/admin.gen'
import { gcpFetch } from '@/api/fetch'

import { waitOperation, type Operation, type Resource } from '../lb/api'

// The VPC network view's reads and writes: networks and subnetworks on
// compute v1, with their custom methods (switchToCustomMode,
// expandIpCidrRange, setPrivateIpGoogleAccess). The Host attachment
// (ADR 0014) is not modelled on GCP: its Offered ports are on the admin
// API only. Other views (Cloud NAT, Cloud DNS) read networks and
// subnetworks from here.

export type HostOffers = components['schemas']['HostOffers']
export type ContainerNetwork = components['schemas']['ContainerNetwork']

export interface Network extends Resource {
  autoCreateSubnetworks?: boolean
  mtu?: number
  routingConfig?: { routingMode?: 'REGIONAL' | 'GLOBAL'; [field: string]: unknown }
  subnetworks?: string[]
  gatewayIPv4?: string
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
