import { block, gcloud, omit, resource, type Recipe } from '@/lib/copyAs'

import { lastSeg, relPath } from '../lb/api'
import { routerPath, type Router, type RouterNat, type RouterRef } from './api'

// Copy as (FR-UI-015) for Cloud Routers and their NAT gateways: gcloud
// compute routers (nats) create, the compute/v1 requests and the
// google_compute_router(_nat) resources.

/** Bgp is a router's bgp as the recipes read it. */
interface Bgp {
  asn?: number
  advertiseMode?: string
  advertisedGroups?: string[]
  advertisedIpRanges?: { range: string; description?: string }[]
  keepaliveInterval?: number
}

const API = '/compute/v1/'

/** routerRecipe is a router without its NAT gateways, which have their own. */
export function routerRecipe(r: Router, ref: RouterRef): Recipe {
  const { project, region } = ref
  const bgp = r.bgp as Bgp | undefined
  const custom = bgp?.advertiseMode === 'CUSTOM'
  return {
    gcloud: gcloud(['compute', 'routers', 'create', r.name], {
      project,
      region,
      network: lastSeg(r.network),
      description: r.description,
      asn: bgp?.asn,
      'advertisement-mode': custom ? 'CUSTOM' : undefined,
      'set-advertisement-groups': custom ? bgp?.advertisedGroups : undefined,
      'set-advertisement-ranges': custom ? bgp?.advertisedIpRanges?.map((x) => x.range) : undefined,
      'keepalive-interval': bgp?.keepaliveInterval,
    }),
    rest: {
      method: 'POST',
      path: `${API}projects/${project}/regions/${region}/routers`,
      body: omit(r, ['id', 'kind', 'selfLink', 'creationTimestamp', 'fingerprint', 'nats']),
    },
    tofu: resource('google_compute_router', r.name, {
      project,
      name: r.name,
      region,
      network: lastSeg(r.network),
      description: r.description,
      bgp: bgp?.asn
        ? block({
            asn: bgp.asn,
            advertise_mode: custom ? 'CUSTOM' : undefined,
            advertised_groups: custom ? bgp?.advertisedGroups : undefined,
            advertised_ip_ranges: custom
              ? bgp.advertisedIpRanges?.map((x) =>
                  block({ range: x.range, description: x.description }),
                )
              : undefined,
            keepalive_interval: bgp.keepaliveInterval,
          })
        : undefined,
    }),
  }
}

/**
 * subnetFlag writes a NAT's subnetwork as gcloud's
 * --nat-custom-subnet-ip-ranges entries: SUBNET (primary range),
 * SUBNET:ALL or SUBNET:RANGE_NAME.
 */
function subnetFlag(s: NonNullable<RouterNat['subnetworks']>[number]): string[] {
  const name = lastSeg(s.name)
  const kinds = s.sourceIpRangesToNat ?? []
  if (kinds.includes('ALL_IP_RANGES')) return [`${name}:ALL`]
  return [
    ...(kinds.includes('PRIMARY_IP_RANGE') ? [name] : []),
    ...(kinds.includes('LIST_OF_SECONDARY_IP_RANGES')
      ? (s.secondaryIpRangeNames ?? []).map((r) => `${name}:${r}`)
      : []),
  ]
}

const num = (v: unknown) => (typeof v === 'number' ? v : undefined)
const secs = (v: unknown) => (typeof v === 'number' ? `${v}s` : undefined)

/**
 * natRecipe is NAT gateway n of router r. The API has no NAT of its own:
 * the request patches the router's nats[], which it replaces as a whole,
 * so it carries every NAT of the router as it is.
 */
export function natRecipe(r: Router, n: RouterNat, ref: RouterRef): Recipe {
  const { project, region } = ref
  const ranges = n.sourceSubnetworkIpRangesToNat
  const manual = n.natIpAllocateOption === 'MANUAL_ONLY'
  const log = n.logConfig?.enable ? n.logConfig : undefined
  return {
    gcloud: gcloud(['compute', 'routers', 'nats', 'create', n.name], {
      project,
      region,
      router: r.name,
      'auto-allocate-nat-external-ips': !manual,
      'nat-external-ip-pool': manual ? n.natIps?.map((ip) => lastSeg(ip)) : undefined,
      'nat-all-subnet-ip-ranges': ranges === 'ALL_SUBNETWORKS_ALL_IP_RANGES',
      'nat-primary-subnet-ip-ranges': ranges === 'ALL_SUBNETWORKS_ALL_PRIMARY_IP_RANGES',
      'nat-custom-subnet-ip-ranges':
        ranges === 'LIST_OF_SUBNETWORKS' ? n.subnetworks?.flatMap(subnetFlag) : undefined,
      'min-ports-per-vm': n.minPortsPerVm,
      'max-ports-per-vm': n.maxPortsPerVm,
      'enable-dynamic-port-allocation': n.enableDynamicPortAllocation,
      'enable-endpoint-independent-mapping': n.enableEndpointIndependentMapping,
      'udp-idle-timeout': secs(n.udpIdleTimeoutSec),
      'icmp-idle-timeout': secs(n.icmpIdleTimeoutSec),
      'tcp-established-idle-timeout': secs(n.tcpEstablishedIdleTimeoutSec),
      'tcp-transitory-idle-timeout': secs(n.tcpTransitoryIdleTimeoutSec),
      'tcp-time-wait-timeout': secs(n.tcpTimeWaitTimeoutSec),
      'enable-logging': !!log,
      'log-filter': log?.filter,
    }),
    rest: {
      method: 'PATCH',
      path: API + routerPath(ref),
      body: { nats: r.nats?.some((x) => x.name === n.name) ? r.nats : [...(r.nats ?? []), n] },
    },
    tofu: resource('google_compute_router_nat', n.name, {
      project,
      name: n.name,
      router: r.name,
      region,
      nat_ip_allocate_option: n.natIpAllocateOption,
      nat_ips: manual ? n.natIps?.map(relPath) : undefined,
      drain_nat_ips: n.drainNatIps?.map(relPath),
      source_subnetwork_ip_ranges_to_nat: ranges,
      min_ports_per_vm: n.minPortsPerVm,
      max_ports_per_vm: n.maxPortsPerVm,
      enable_dynamic_port_allocation: n.enableDynamicPortAllocation,
      enable_endpoint_independent_mapping: n.enableEndpointIndependentMapping,
      udp_idle_timeout_sec: num(n.udpIdleTimeoutSec),
      icmp_idle_timeout_sec: num(n.icmpIdleTimeoutSec),
      tcp_established_idle_timeout_sec: num(n.tcpEstablishedIdleTimeoutSec),
      tcp_transitory_idle_timeout_sec: num(n.tcpTransitoryIdleTimeoutSec),
      tcp_time_wait_timeout_sec: num(n.tcpTimeWaitTimeoutSec),
      subnetwork:
        ranges === 'LIST_OF_SUBNETWORKS'
          ? n.subnetworks?.map((s) =>
              block({
                name: relPath(s.name),
                source_ip_ranges_to_nat: s.sourceIpRangesToNat,
                secondary_ip_range_names: s.secondaryIpRangeNames,
              }),
            )
          : undefined,
      log_config: log ? block({ enable: true, filter: log.filter ?? 'ALL' }) : undefined,
    }),
  }
}
