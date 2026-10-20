import { block, gcloud, omit, resource, type Recipe } from '@/lib/copyAs'

import { lastSeg, relPath } from '../lb/api'
import type { Network, Subnetwork } from './api'

// Copy as (FR-UI-015) for VPC networks and subnetworks: gcloud compute
// networks (subnets) create, the compute/v1 requests and the
// google_compute_network and google_compute_subnetwork resources.

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
