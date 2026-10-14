import { block, gcloud, keyValues, omit, resource, type Recipe } from '@/lib/copyAs'

import { networkPath, type ManagedZone, type ResourceRecordSet, type ZoneRef } from './api'

// Copy as (FR-UI-015) for managed zones and record sets: gcloud dns, the
// dns/v1 create request and the google_dns_* resource.

const enc = encodeURIComponent
const zonesPath = (project: string) => `/dns/v1/projects/${enc(project)}/managedZones`

/** keySpec finds a zone's DNSSEC key spec of one type. */
const keySpec = (z: ManagedZone, type: 'keySigning' | 'zoneSigning') =>
  z.dnssecConfig?.defaultKeySpecs?.find((k) => k.keyType === type)

/** targets are the forwarding targets that take one path, as gcloud lists them. */
const targets = (z: ManagedZone, priv: boolean) =>
  (z.forwardingConfig?.targetNameServers ?? [])
    .filter((t) => (t.forwardingPath === 'private') === priv)
    .map((t) => t.ipv4Address ?? t.ipv6Address ?? '')

export function zoneRecipe(z: ManagedZone, project: string): Recipe {
  const dnssec =
    z.dnssecConfig?.state && z.dnssecConfig.state !== 'off' ? z.dnssecConfig : undefined
  const ksk = dnssec && keySpec(z, 'keySigning')
  const zsk = dnssec && keySpec(z, 'zoneSigning')
  const priv = z.visibility === 'private'
  const pv = priv ? (z.privateVisibilityConfig ?? {}) : undefined
  const peer = /^projects\/([^/]+)\/global\/networks\/([^/]+)$/.exec(
    networkPath(z.peeringConfig?.targetNetwork?.networkUrl ?? ''),
  )
  let cmd = gcloud(['dns', 'managed-zones', 'create', z.name], {
    project,
    'dns-name': z.dnsName,
    description: z.description,
    labels: keyValues(z.labels),
    visibility: priv ? 'private' : undefined,
    networks: pv?.networks?.map((n) => n.networkUrl),
    gkeclusters: pv?.gkeClusters?.map((c) => c.gkeClusterName),
    'dnssec-state': dnssec?.state,
    'denial-of-existence': dnssec?.nonExistence,
    'ksk-algorithm': ksk?.algorithm,
    'ksk-key-length': ksk?.keyLength,
    'zsk-algorithm': zsk?.algorithm,
    'zsk-key-length': zsk?.keyLength,
    'forwarding-targets': targets(z, false),
    'private-forwarding-targets': targets(z, true),
    'target-network': peer?.[2],
    'target-project': peer?.[1],
    'log-dns-queries': z.cloudLoggingConfig?.enableLogging,
  })
  // gcloud requires --description, even an empty one.
  if (!z.description) cmd = cmd.replace(/--dns-name=\S+/, `$& \\\n    --description=''`)
  return {
    gcloud: cmd,
    rest: {
      method: 'POST',
      path: zonesPath(project),
      body: omit(z, ['kind', 'id', 'nameServers', 'creationTime']),
    },
    tofu: resource('google_dns_managed_zone', z.name, {
      project,
      name: z.name,
      dns_name: z.dnsName,
      // Unset, the provider would describe the zone as "Managed by Terraform".
      description: z.description ?? '',
      labels: z.labels,
      visibility: priv ? 'private' : undefined,
      private_visibility_config: pv
        ? block({
            networks: (pv.networks ?? []).map((n) => block({ network_url: n.networkUrl })),
            gke_clusters: (pv.gkeClusters ?? []).map((c) =>
              block({ gke_cluster_name: c.gkeClusterName }),
            ),
          })
        : undefined,
      dnssec_config: dnssec
        ? block({
            state: dnssec.state,
            non_existence: dnssec.nonExistence,
            default_key_specs: (dnssec.defaultKeySpecs ?? []).map((k) =>
              block({ algorithm: k.algorithm, key_length: k.keyLength, key_type: k.keyType }),
            ),
          })
        : undefined,
      forwarding_config: z.forwardingConfig?.targetNameServers?.length
        ? block({
            target_name_servers: z.forwardingConfig.targetNameServers.map((t) =>
              block({
                ipv4_address: t.ipv4Address,
                ipv6_address: t.ipv6Address,
                forwarding_path: t.forwardingPath,
              }),
            ),
          })
        : undefined,
      peering_config: z.peeringConfig?.targetNetwork?.networkUrl
        ? block({
            target_network: block({ network_url: z.peeringConfig.targetNetwork.networkUrl }),
          })
        : undefined,
      cloud_logging_config: z.cloudLoggingConfig
        ? block({ enable_logging: !!z.cloudLoggingConfig.enableLogging })
        : undefined,
    }),
  }
}

/** routingData is a routing policy as gcloud's --routing-policy-data. */
function routingData(rs: ResourceRecordSet): [type?: string, data?: string] {
  const p = rs.routingPolicy
  const data = (items: { key?: string | number; rrdatas?: string[] }[]) =>
    items.map((i) => `${i.key ?? ''}=${(i.rrdatas ?? []).join(',')}`).join(';')
  if (p?.wrr) return ['WRR', data((p.wrr.items ?? []).map((i) => ({ ...i, key: i.weight })))]
  if (p?.geo) return ['GEO', data((p.geo.items ?? []).map((i) => ({ ...i, key: i.location })))]
  return []
}

export function rrsetRecipe(rs: ResourceRecordSet, r: ZoneRef): Recipe {
  const [policyType, policyData] = routingData(rs)
  const p = rs.routingPolicy
  return {
    gcloud: gcloud(['dns', 'record-sets', 'create', rs.name], {
      project: r.project,
      zone: r.zone,
      type: rs.type,
      ttl: rs.ttl,
      rrdatas: rs.rrdatas,
      'routing-policy-type': policyType,
      'routing-policy-data': policyData,
    }),
    rest: {
      method: 'POST',
      path: `${zonesPath(r.project)}/${enc(r.zone)}/rrsets`,
      body: omit(rs, ['kind']),
    },
    tofu: resource('google_dns_record_set', `${rs.name.replace(/\.$/, '')}-${rs.type}`, {
      project: r.project,
      managed_zone: r.zone,
      name: rs.name,
      type: rs.type,
      ttl: rs.ttl,
      rrdatas: rs.rrdatas,
      routing_policy: p
        ? block({
            wrr: (p.wrr?.items ?? []).map((i) => block({ weight: i.weight, rrdatas: i.rrdatas })),
            geo: (p.geo?.items ?? []).map((i) =>
              block({ location: i.location, rrdatas: i.rrdatas }),
            ),
          })
        : undefined,
    }),
  }
}
