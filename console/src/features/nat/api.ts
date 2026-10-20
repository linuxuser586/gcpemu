import { queryOptions } from '@tanstack/react-query'

import { admin, unwrap, type RequestEntry } from '@/api/admin'
import type { components } from '@/api/admin.gen'
import { gcpFetch } from '@/api/fetch'

import { lastSeg, relPath, waitOperation, type Operation, type Resource } from '../lb/api'
import { aggregated, type Subnetwork } from '../vpc/api'

// The Cloud NAT view's reads and writes. Cloud NAT's control plane is
// compute v1 (FR-NAT-001): Cloud Routers and their nats[], with
// routers.getRouterStatus and routers.getNatMappingInfo for NAT IPs and
// port usage (FR-NAT-003). A NAT has no API of its own: adding, editing
// or deleting one patches its router's nats[], which the API replaces
// as a whole. The translation log is the Request log's nat entries, and
// the offline sink (FR-NAT-005) is switched on the admin API: GCP has
// neither.

export type NatTrace = components['schemas']['NatTrace']
export type NatSink = components['schemas']['NatSink']

export type RangesToNat =
  'ALL_SUBNETWORKS_ALL_IP_RANGES' | 'ALL_SUBNETWORKS_ALL_PRIMARY_IP_RANGES' | 'LIST_OF_SUBNETWORKS'

export type SubnetRanges = 'ALL_IP_RANGES' | 'PRIMARY_IP_RANGE' | 'LIST_OF_SECONDARY_IP_RANGES'

export interface RouterNatSubnetwork {
  name: string
  sourceIpRangesToNat?: SubnetRanges[]
  secondaryIpRangeNames?: string[]
}

export type LogFilter = 'ALL' | 'ERRORS_ONLY' | 'TRANSLATIONS_ONLY'

/** RouterNat is one NAT config; the raw JSON editor covers the rest. */
export interface RouterNat {
  name: string
  type?: string
  natIpAllocateOption?: 'AUTO_ONLY' | 'MANUAL_ONLY'
  natIps?: string[]
  drainNatIps?: string[]
  sourceSubnetworkIpRangesToNat?: RangesToNat
  subnetworks?: RouterNatSubnetwork[]
  minPortsPerVm?: number
  maxPortsPerVm?: number
  enableDynamicPortAllocation?: boolean
  enableEndpointIndependentMapping?: boolean
  logConfig?: { enable?: boolean; filter?: LogFilter }
  [field: string]: unknown
}

export interface Router extends Resource {
  network?: string
  bgp?: { asn?: number; [field: string]: unknown }
  nats?: RouterNat[]
}

/** RouterRef names a router: a Project, its region and its name. */
export interface RouterRef {
  project: string
  region: string
  router: string
}

const API = '/compute/v1/'
const enc = encodeURIComponent

/** routerPath is a router's relative path. */
export const routerPath = (r: RouterRef) =>
  `projects/${r.project}/regions/${r.region}/routers/${enc(r.router)}`

/** routerRefOf is the RouterRef of a router's selfLink. */
export function routerRefOf(link: string | undefined): RouterRef | undefined {
  const m = /^projects\/([^/]+)\/regions\/([^/]+)\/routers\/([^/]+)/.exec(relPath(link))
  return m ? { project: m[1]!, region: m[2]!, router: decodeURIComponent(m[3]!) } : undefined
}

/** gatewayOf reads a Cloud NAT gateway path: ".../routers/R/nats/N". */
export function gatewayOf(path: string | undefined): (RouterRef & { nat: string }) | undefined {
  const m = /^(.*)\/nats\/([^/]+)$/.exec(path ?? '')
  const r = m && routerRefOf(m[1])
  return r ? { ...r, nat: m[2]! } : undefined
}

export const listRouters = (project: string) => aggregated<'routers', Router>(project, 'routers')

export const getRouter = (r: RouterRef) => gcpFetch<Router>(API + routerPath(r))

const write = async (path: string, method: string, body?: unknown) =>
  waitOperation(
    await gcpFetch<Operation>(API + path, {
      method,
      body: body === undefined ? undefined : JSON.stringify(body),
    }),
  )

export const insertRouter = (project: string, region: string, body: unknown) =>
  write(`projects/${project}/regions/${region}/routers`, 'POST', body)

/** patchRouter sends a JSON merge patch; nats replaces the list. */
export const patchRouter = (r: RouterRef, body: unknown) => write(routerPath(r), 'PATCH', body)

export const deleteRouter = (r: RouterRef) => write(routerPath(r), 'DELETE')

/** withNat is the router's nats with nat added, or replacing the one named was. */
export function withNat(router: Router, nat: RouterNat, was?: string): RouterNat[] {
  const nats = router.nats ?? []
  return was === undefined ? [...nats, nat] : nats.map((n) => (n.name === was ? nat : n))
}

export const withoutNat = (router: Router, name: string) =>
  (router.nats ?? []).filter((n) => n.name !== name)

export interface NatStatus {
  name: string
  autoAllocatedNatIps?: string[]
  userAllocatedNatIps?: string[]
  numVmEndpointsWithNatMappings?: number
}

export const routerStatus = async (r: RouterRef) =>
  (
    await gcpFetch<{ result?: { natStatus?: NatStatus[] } }>(
      `${API}${routerPath(r)}/getRouterStatus`,
    )
  ).result?.natStatus ?? []

export interface NatMapping {
  instanceName?: string
  interfaceNatMappings?: {
    sourceVirtualIp?: string
    natIpPortRanges?: string[]
    numTotalNatPorts?: number
  }[]
}

/** natMappings is getNatMappingInfo of one NAT, every page. */
export async function natMappings(r: RouterRef, nat: string): Promise<NatMapping[]> {
  const out: NatMapping[] = []
  let token = ''
  do {
    const q = new URLSearchParams({ natName: nat })
    if (token) q.set('pageToken', token)
    const res = await gcpFetch<{ result?: NatMapping[]; nextPageToken?: string }>(
      `${API}${routerPath(r)}/getNatMappingInfo?${q}`,
    )
    out.push(...(res.result ?? []))
    token = res.nextPageToken ?? ''
  } while (token)
  return out
}

// ---- coverage ----

/** Covered is a subnetwork a NAT translates, with the ranges it covers. */
export interface Covered {
  subnetwork: Subnetwork
  ranges: { name: string; cidr: string }[]
}

/**
 * natRanges are the ranges of subnetwork sn a NAT covers (FR-NAT-002),
 * as the egress gateway applies them: the primary range is "primary",
 * secondary ranges by their names.
 */
export function natRanges(nat: RouterNat, sn: Subnetwork): { name: string; cidr: string }[] {
  const primary = sn.ipCidrRange ? [{ name: 'primary', cidr: sn.ipCidrRange }] : []
  const secondary = (sn.secondaryIpRanges ?? []).map((r) => ({
    name: r.rangeName,
    cidr: r.ipCidrRange,
  }))
  switch (nat.sourceSubnetworkIpRangesToNat) {
    case 'ALL_SUBNETWORKS_ALL_IP_RANGES':
      return [...primary, ...secondary]
    case 'ALL_SUBNETWORKS_ALL_PRIMARY_IP_RANGES':
      return primary
    case 'LIST_OF_SUBNETWORKS': {
      const sub = nat.subnetworks?.find((s) => relPath(s.name) === relPath(sn.selfLink))
      if (!sub) return []
      const kinds = sub.sourceIpRangesToNat ?? []
      if (kinds.includes('ALL_IP_RANGES')) return [...primary, ...secondary]
      return [
        ...(kinds.includes('PRIMARY_IP_RANGE') ? primary : []),
        ...(kinds.includes('LIST_OF_SECONDARY_IP_RANGES')
          ? secondary.filter((r) => sub.secondaryIpRangeNames?.includes(r.name))
          : []),
      ]
    }
    default:
      return []
  }
}

/** regionSubnets are the subnetworks of a router's network in its region. */
export const regionSubnets = (router: Router, subnets: Subnetwork[]) =>
  subnets
    .filter(
      (s) =>
        relPath(s.network) === relPath(router.network) &&
        lastSeg(s.region) === lastSeg(router.region),
    )
    .sort((a, b) => a.name.localeCompare(b.name))

/** coverage lists the subnetworks a NAT of router covers, and their ranges. */
export const coverage = (router: Router, nat: RouterNat, subnets: Subnetwork[]): Covered[] =>
  regionSubnets(router, subnets)
    .map((subnetwork) => ({ subnetwork, ranges: natRanges(nat, subnetwork) }))
    .filter((c) => c.ranges.length > 0)

// ---- the translation log ----

/** Egress is a nat Request log entry; the Instance always sets these. */
export type Egress = RequestEntry & {
  nat: NatTrace
  time: string
  code: string
  resource: string
}

export const isEgress = (e: RequestEntry): e is Egress => !!e.nat && !!e.time && !!e.resource

/** inProject keeps a Project's egress entries. */
export const inProject = (e: Egress, project: string) =>
  e.resource.startsWith(`projects/${project}/`)

// ---- the offline sink ----

export const setSink = (enabled: boolean) =>
  unwrap(admin.PUT('/_emu/v1/nat/sink', { body: { enabled } }))

// ---- queries ----

// Keys are under "compute": the events bridge marks them stale when a
// router, NAT IP or workload address changes (FR-UI-006).

export const routersQuery = (project: string) =>
  queryOptions({
    queryKey: ['compute', 'nat', 'routers', project],
    queryFn: () => listRouters(project),
    enabled: !!project,
  })

export const routerQuery = (r: RouterRef) =>
  queryOptions({
    queryKey: ['compute', 'nat', 'router', r],
    queryFn: () => getRouter(r),
  })

export const statusQuery = (r: RouterRef) =>
  queryOptions({
    queryKey: ['compute', 'nat', 'status', r],
    queryFn: () => routerStatus(r),
  })

export const mappingsQuery = (r: RouterRef, nat: string) =>
  queryOptions({
    queryKey: ['compute', 'nat', 'mappings', r, nat],
    queryFn: () => natMappings(r, nat),
  })

export const sinkQuery = () =>
  queryOptions({
    queryKey: ['nat', 'sink'],
    queryFn: () => unwrap(admin.GET('/_emu/v1/nat/sink')),
  })
