import { block, gcloud, keyValues, omit, resource, type Recipe } from '@/lib/copyAs'

import { lastSeg, relPath } from '../lb/api'
import type { Address, Firewall, Network, Route, Subnetwork } from './api'
import { formatRules } from './form'

// Copy as (FR-UI-015) for the VPC view's resources: gcloud compute
// networks (subnets) create, firewall-rules create, routes create and
// addresses create, the compute/v1 requests, and the google_compute_network,
// _subnetwork, _firewall, _route, _address and _global_address resources.

const API = '/compute/v1/'

/** OUTPUT are the fields the API sets, which a request leaves out. */
const OUTPUT = ['id', 'kind', 'selfLink', 'selfLinkWithId', 'creationTimestamp']

export function networkRecipe(n: Network, project: string): Recipe {
  const auto = !!n.autoCreateSubnetworks
  const mode = n.routingConfig?.routingMode
  return {
    gcloud: gcloud(['compute', 'networks', 'create', n.name], {
      project,
      'subnet-mode': auto ? 'auto' : 'custom',
      description: n.description,
      mtu: n.mtu,
      'bgp-routing-mode': mode?.toLowerCase(),
    }),
    rest: {
      method: 'POST',
      path: `${API}projects/${project}/global/networks`,
      body: {
        ...omit(n, [...OUTPUT, 'subnetworks', 'gatewayIPv4', 'peerings']),
        autoCreateSubnetworks: auto,
      },
    },
    tofu: resource('google_compute_network', n.name, {
      project,
      name: n.name,
      description: n.description,
      auto_create_subnetworks: auto,
      mtu: n.mtu,
      routing_mode: mode,
    }),
  }
}

export function subnetRecipe(s: Subnetwork, project: string): Recipe {
  const region = lastSeg(s.region)
  const network = relPath(s.network)
  const secondary = s.secondaryIpRanges ?? []
  return {
    gcloud: gcloud(['compute', 'networks', 'subnets', 'create', s.name], {
      project,
      region,
      network: lastSeg(s.network),
      range: s.ipCidrRange,
      description: s.description,
      'secondary-range': secondary.map((r) => `${r.rangeName}=${r.ipCidrRange}`),
      'enable-private-ip-google-access': s.privateIpGoogleAccess,
    }),
    rest: {
      method: 'POST',
      path: `${API}projects/${project}/regions/${region}/subnetworks`,
      body: {
        ...omit(s, [...OUTPUT, 'region', 'fingerprint', 'gatewayAddress', 'state']),
        network,
      },
    },
    tofu: resource('google_compute_subnetwork', s.name, {
      project,
      name: s.name,
      region,
      network,
      ip_cidr_range: s.ipCidrRange,
      description: s.description,
      private_ip_google_access: s.privateIpGoogleAccess,
      secondary_ip_range: secondary.map((r) =>
        block({ range_name: r.rangeName, ip_cidr_range: r.ipCidrRange }),
      ),
    }),
  }
}

/** LOGGING_METADATA is gcloud's --logging-metadata for a logConfig's metadata. */
const LOGGING_METADATA: Record<string, string | undefined> = {
  INCLUDE_ALL_METADATA: 'include-all',
  EXCLUDE_ALL_METADATA: 'exclude-all',
}

export function firewallRecipe(f: Firewall, project: string): Recipe {
  const deny = !!f.denied?.length
  const rules = (deny ? f.denied : f.allowed) ?? []
  const logged = !!f.logConfig?.enable
  const metadata = typeof f.logConfig?.metadata === 'string' ? f.logConfig.metadata : ''
  return {
    gcloud: gcloud(['compute', 'firewall-rules', 'create', f.name], {
      project,
      network: lastSeg(f.network),
      direction: f.direction,
      priority: f.priority,
      action: deny ? 'DENY' : 'ALLOW',
      rules: formatRules(rules),
      'source-ranges': f.sourceRanges,
      'destination-ranges': f.destinationRanges,
      'source-tags': f.sourceTags,
      'target-tags': f.targetTags,
      'source-service-accounts': f.sourceServiceAccounts,
      'target-service-accounts': f.targetServiceAccounts,
      description: f.description,
      disabled: f.disabled,
      'enable-logging': logged,
      'logging-metadata': logged ? LOGGING_METADATA[metadata] : undefined,
    }),
    rest: {
      method: 'POST',
      path: `${API}projects/${project}/global/firewalls`,
      body: { ...omit(f, OUTPUT), network: relPath(f.network) },
    },
    tofu: resource('google_compute_firewall', f.name, {
      project,
      name: f.name,
      network: relPath(f.network),
      description: f.description,
      direction: f.direction,
      priority: f.priority,
      source_ranges: f.sourceRanges,
      destination_ranges: f.destinationRanges,
      source_tags: f.sourceTags,
      target_tags: f.targetTags,
      source_service_accounts: f.sourceServiceAccounts,
      target_service_accounts: f.targetServiceAccounts,
      disabled: f.disabled || undefined,
      [deny ? 'deny' : 'allow']: rules.map((r) =>
        block({ protocol: r.IPProtocol, ports: r.ports }),
      ),
      log_config: logged ? block({ metadata: metadata || 'INCLUDE_ALL_METADATA' }) : undefined,
    }),
  }
}

/**
 * addressRecipe copies an address. An external one leaves its IP to the
 * API, which picks one from its pool; an internal one keeps it. Only a
 * regional address has a network tier, and gcloud sets labels after the
 * address is created.
 */
export function addressRecipe(a: Address, project: string): Recipe {
  const region = lastSeg(a.region)
  const internal = a.addressType === 'INTERNAL'
  const scope = region ? { region } : { global: true }
  const create = gcloud(['compute', 'addresses', 'create', a.name], {
    project,
    ...scope,
    description: a.description,
    addresses: internal ? a.address : undefined,
    'prefix-length': a.prefixLength,
    subnet: a.subnetwork && lastSeg(a.subnetwork),
    network: a.network && lastSeg(a.network),
    purpose: a.purpose,
    'network-tier': region ? a.networkTier : undefined,
    'ip-version': region ? undefined : a.ipVersion,
  })
  const labels = keyValues(a.labels)
  return {
    gcloud: labels.length
      ? [
          create,
          gcloud(['compute', 'addresses', 'update', a.name], {
            project,
            ...scope,
            'update-labels': labels,
          }),
        ]
      : create,
    rest: {
      method: 'POST',
      path: `${API}projects/${project}/${region ? `regions/${region}` : 'global'}/addresses`,
      body: {
        ...omit(a, [
          ...OUTPUT,
          'region',
          'status',
          'users',
          'labelFingerprint',
          ...(internal ? [] : ['address']),
          ...(region ? [] : ['networkTier']),
        ]),
        ...(a.subnetwork ? { subnetwork: relPath(a.subnetwork) } : {}),
        ...(a.network ? { network: relPath(a.network) } : {}),
      },
    },
    tofu: resource(region ? 'google_compute_address' : 'google_compute_global_address', a.name, {
      project,
      name: a.name,
      region: region || undefined,
      description: a.description,
      address: internal ? a.address : undefined,
      address_type: a.addressType,
      purpose: a.purpose,
      prefix_length: a.prefixLength,
      subnetwork: a.subnetwork && relPath(a.subnetwork),
      network: a.network && relPath(a.network),
      network_tier: region ? a.networkTier : undefined,
      ip_version: region ? undefined : a.ipVersion,
      labels: a.labels,
    }),
  }
}

/** scopeOfHop is the zone or region a next hop's path names. */
const scopeOfHop = (link?: string) => /\/(?:zones|regions)\/([^/]+)\//.exec(relPath(link))?.[1]

/** hop is a next hop resource's path, or an ILB's address as it is. */
const hop = (link?: string) => (link && link.includes('/') ? relPath(link) : link)

export function routeRecipe(r: Route, project: string): Recipe {
  const ilbByName = !!r.nextHopIlb?.includes('/')
  return {
    gcloud: gcloud(['compute', 'routes', 'create', r.name], {
      project,
      network: lastSeg(r.network),
      'destination-range': r.destRange,
      priority: r.priority,
      'next-hop-gateway': r.nextHopGateway && lastSeg(r.nextHopGateway),
      'next-hop-address': r.nextHopIp,
      'next-hop-instance': r.nextHopInstance && lastSeg(r.nextHopInstance),
      'next-hop-instance-zone': scopeOfHop(r.nextHopInstance),
      'next-hop-ilb': ilbByName ? lastSeg(r.nextHopIlb) : r.nextHopIlb,
      'next-hop-ilb-region': ilbByName ? scopeOfHop(r.nextHopIlb) : undefined,
      'next-hop-vpn-tunnel': r.nextHopVpnTunnel && lastSeg(r.nextHopVpnTunnel),
      'next-hop-vpn-tunnel-region': scopeOfHop(r.nextHopVpnTunnel),
      tags: r.tags,
      description: r.description,
    }),
    rest: {
      method: 'POST',
      path: `${API}projects/${project}/global/routes`,
      body: {
        ...omit(r, [...OUTPUT, 'routeType', 'routeStatus', 'warnings']),
        network: relPath(r.network),
        ...(r.nextHopGateway ? { nextHopGateway: relPath(r.nextHopGateway) } : {}),
      },
    },
    tofu: resource('google_compute_route', r.name, {
      project,
      name: r.name,
      network: relPath(r.network),
      description: r.description,
      dest_range: r.destRange,
      priority: r.priority,
      next_hop_gateway: r.nextHopGateway && lastSeg(r.nextHopGateway),
      next_hop_ip: r.nextHopIp,
      next_hop_instance: hop(r.nextHopInstance),
      next_hop_ilb: hop(r.nextHopIlb),
      next_hop_vpn_tunnel: hop(r.nextHopVpnTunnel),
      tags: r.tags,
    }),
  }
}
