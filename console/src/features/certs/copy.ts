import { block, expr, gcloud, keyValues, resource, type Block, type Recipe } from '@/lib/copyAs'

import {
  lastSegment,
  locationOf,
  type Certificate,
  type CertificateMap,
  type CertificateMapEntry,
  type DnsAuthorization,
  type IssuanceConfig,
  type TrustConfig,
} from './api'
import { NS_API, TLS_KINDS, tlsRefOf, type CertificateProvider, type TlsResource } from './tls'

// Copy as (FR-UI-015) for certificates, certificate maps and map
// entries: gcloud certificate-manager, the certificatemanager/v1 create
// request and the google_certificate_manager_* resource. The API never returns
// a self-managed certificate's private key, so each form leaves it to the
// reader, as the Load Balancer view does for classic certificates.

/** PRIVATE_KEY stands in for a certificate's private key, which the API never returns. */
const PRIVATE_KEY = 'PRIVATE_KEY_PEM (the API does not return the private key)'
const SAVE_KEY = '# The API does not return the private key: save it as private-key.pem.'

/**
 * certificateRecipe is certificate c. project, when given, is the ID the
 * commands use instead of the one in its name.
 */
export function certificateRecipe(c: Certificate, project = c.name.split('/')[1] ?? ''): Recipe {
  const id = lastSegment(c.name)
  const location = locationOf(c.name) || 'global'
  // global is every client's default location.
  const loc = location === 'global' ? undefined : location
  const scope = c.scope && c.scope !== 'DEFAULT' ? c.scope : undefined
  const m = c.managed
  const managed = m && {
    domains: m.domains,
    ...(m.dnsAuthorizations?.length ? { dnsAuthorizations: m.dnsAuthorizations } : {}),
    ...(m.issuanceConfig ? { issuanceConfig: m.issuanceConfig } : {}),
  }
  const pem = (c.pemCertificate ?? '').trimEnd()
  const create = gcloud(['certificate-manager', 'certificates', 'create', id], {
    project,
    location: loc,
    description: c.description,
    labels: keyValues(c.labels),
    // gcloud spells the enum in lowercase with hyphens, e.g. edge-cache.
    scope: scope?.toLowerCase().replaceAll('_', '-'),
    domains: m?.domains,
    'dns-authorizations': m?.dnsAuthorizations,
    'issuance-config': m?.issuanceConfig,
    'certificate-file': m ? undefined : 'certificate.pem',
    'private-key-file': m ? undefined : 'private-key.pem',
  })
  const tofu = resource('google_certificate_manager_certificate', id, {
    project,
    name: id,
    location: loc,
    description: c.description,
    labels: c.labels,
    scope,
    self_managed: m
      ? undefined
      : block({ pem_certificate: pem, pem_private_key: expr('file("private-key.pem")') }),
    managed: m
      ? block({
          domains: m.domains,
          dns_authorizations: m.dnsAuthorizations,
          issuance_config: m.issuanceConfig,
        })
      : undefined,
  })
  return {
    gcloud: m ? create : [`cat > certificate.pem <<'EOF'\n${pem}\nEOF`, `${SAVE_KEY}\n${create}`],
    rest: {
      method: 'POST',
      path: `/certificatemanager/v1/projects/${project}/locations/${location}/certificates?${new URLSearchParams({ certificateId: id })}`,
      body: {
        ...(c.description ? { description: c.description } : {}),
        ...(c.labels && Object.keys(c.labels).length ? { labels: c.labels } : {}),
        ...(scope ? { scope } : {}),
        ...(managed
          ? { managed }
          : {
              selfManaged: { pemCertificate: c.pemCertificate ?? '', pemPrivateKey: PRIVATE_KEY },
            }),
      },
    },
    tofu: m ? tofu : `${SAVE_KEY}\n${tofu}`,
  }
}

/** projectOf is the Project ID in a resource name. */
const projectOf = (name: string) => name.split('/')[1] ?? ''

/** mapRecipe is certificate map m, in project when given. */
export function mapRecipe(m: CertificateMap, project = projectOf(m.name)): Recipe {
  const id = lastSegment(m.name)
  return {
    gcloud: gcloud(['certificate-manager', 'maps', 'create', id], {
      project,
      description: m.description,
      labels: keyValues(m.labels),
    }),
    rest: {
      method: 'POST',
      path: `/certificatemanager/v1/projects/${project}/locations/global/certificateMaps?${new URLSearchParams({ certificateMapId: id })}`,
      body: {
        ...(m.description ? { description: m.description } : {}),
        ...(m.labels && Object.keys(m.labels).length ? { labels: m.labels } : {}),
      },
    },
    tofu: resource('google_certificate_manager_certificate_map', id, {
      project,
      name: id,
      description: m.description,
      labels: m.labels,
    }),
  }
}

/**
 * entryRecipe is certificate map entry e, in project when given. gcloud
 * names its certificates by ID, as they are global.
 */
export function entryRecipe(e: CertificateMapEntry, project = projectOf(e.name)): Recipe {
  const id = lastSegment(e.name)
  const map = e.name.split('/')[5] ?? ''
  const primary = e.matcher === 'PRIMARY'
  const certificates = e.certificates ?? []
  return {
    gcloud: gcloud(['certificate-manager', 'maps', 'entries', 'create', id], {
      project,
      map,
      description: e.description,
      labels: keyValues(e.labels),
      hostname: primary ? undefined : e.hostname,
      'set-primary': primary,
      certificates: certificates.map(lastSegment),
    }),
    rest: {
      method: 'POST',
      path: `/certificatemanager/v1/projects/${project}/locations/global/certificateMaps/${map}/certificateMapEntries?${new URLSearchParams({ certificateMapEntryId: id })}`,
      body: {
        ...(e.description ? { description: e.description } : {}),
        ...(e.labels && Object.keys(e.labels).length ? { labels: e.labels } : {}),
        ...(primary ? { matcher: 'PRIMARY' } : { hostname: e.hostname }),
        certificates,
      },
    },
    tofu: resource('google_certificate_manager_certificate_map_entry', id, {
      project,
      name: id,
      map,
      description: e.description,
      labels: e.labels,
      hostname: primary ? undefined : e.hostname,
      matcher: primary ? 'PRIMARY' : undefined,
      certificates,
    }),
  }
}

/** labelled is the description and labels of a create request body, when set. */
const labelled = (r: { description?: string; labels?: Record<string, string> }) => ({
  ...(r.description ? { description: r.description } : {}),
  ...(r.labels && Object.keys(r.labels).length ? { labels: r.labels } : {}),
})

/**
 * dnsAuthorizationRecipe is DNS authorization a, in project when given.
 * Its type is left to the default unless it is a per-project record; the
 * record it asks for is computed, so it is left out.
 */
export function dnsAuthorizationRecipe(a: DnsAuthorization, project = projectOf(a.name)): Recipe {
  const id = lastSegment(a.name)
  const location = locationOf(a.name) || 'global'
  const loc = location === 'global' ? undefined : location
  const type = a.type === 'PER_PROJECT_RECORD' ? a.type : undefined
  return {
    gcloud: gcloud(['certificate-manager', 'dns-authorizations', 'create', id], {
      project,
      location: loc,
      domain: a.domain,
      type,
      description: a.description,
      labels: keyValues(a.labels),
    }),
    rest: {
      method: 'POST',
      path: `/certificatemanager/v1/projects/${project}/locations/${location}/dnsAuthorizations?${new URLSearchParams({ dnsAuthorizationId: id })}`,
      body: { domain: a.domain, ...(type ? { type } : {}), ...labelled(a) },
    },
    tofu: resource('google_certificate_manager_dns_authorization', id, {
      project,
      name: id,
      location: loc,
      domain: a.domain,
      type,
      description: a.description,
      labels: a.labels,
    }),
  }
}

/** pemBlocks are PEM certificates as repeated OpenTofu blocks. */
const pemBlocks = (pems?: { pemCertificate?: string }[]) =>
  (pems ?? []).map((p) => block({ pem_certificate: p.pemCertificate }))

/**
 * trustConfigRecipe is trust config t, in project when given. gcloud
 * imports it from YAML (here JSON, which YAML reads) on stdin; the etag is
 * left out, as a new trust config has none.
 */
export function trustConfigRecipe(t: TrustConfig, project = projectOf(t.name)): Recipe {
  const id = lastSegment(t.name)
  const location = locationOf(t.name) || 'global'
  const body = {
    ...labelled(t),
    ...(t.trustStores?.length ? { trustStores: t.trustStores } : {}),
    ...(t.allowlistedCertificates?.length
      ? { allowlistedCertificates: t.allowlistedCertificates }
      : {}),
  }
  const cmd = gcloud(['certificate-manager', 'trust-configs', 'import', id], { project, location })
  return {
    gcloud: `${cmd} <<'EOF'\n${JSON.stringify(body, null, 2)}\nEOF`,
    rest: {
      method: 'POST',
      path: `/certificatemanager/v1/projects/${project}/locations/${location}/trustConfigs?${new URLSearchParams({ trustConfigId: id })}`,
      body,
    },
    tofu: resource('google_certificate_manager_trust_config', id, {
      project,
      name: id,
      location,
      description: t.description,
      labels: t.labels,
      trust_stores: (t.trustStores ?? []).map((s) =>
        block({
          trust_anchors: pemBlocks(s.trustAnchors),
          intermediate_cas: pemBlocks(s.intermediateCas),
        }),
      ),
      allowlisted_certificates: pemBlocks(t.allowlistedCertificates),
    }),
  }
}

/**
 * issuanceConfigRecipe is certificate issuance config c, in project when
 * given.
 */
export function issuanceConfigRecipe(c: IssuanceConfig, project = projectOf(c.name)): Recipe {
  const id = lastSegment(c.name)
  const location = locationOf(c.name) || 'global'
  const loc = location === 'global' ? undefined : location
  const caPool = c.certificateAuthorityConfig?.certificateAuthorityServiceConfig?.caPool
  return {
    gcloud: gcloud(['certificate-manager', 'issuance-configs', 'create', id], {
      project,
      location: loc,
      'ca-pool': caPool,
      lifetime: c.lifetime,
      'rotation-window-percentage': c.rotationWindowPercentage,
      // gcloud spells the enum in lowercase with hyphens, e.g. ecdsa-p256.
      'key-algorithm': c.keyAlgorithm?.toLowerCase().replaceAll('_', '-'),
      description: c.description,
      labels: keyValues(c.labels),
    }),
    rest: {
      method: 'POST',
      path: `/certificatemanager/v1/projects/${project}/locations/${location}/certificateIssuanceConfigs?${new URLSearchParams({ certificateIssuanceConfigId: id })}`,
      body: {
        ...labelled(c),
        certificateAuthorityConfig: { certificateAuthorityServiceConfig: { caPool } },
        lifetime: c.lifetime,
        rotationWindowPercentage: c.rotationWindowPercentage,
        ...(c.keyAlgorithm ? { keyAlgorithm: c.keyAlgorithm } : {}),
      },
    },
    tofu: resource('google_certificate_manager_certificate_issuance_config', id, {
      project,
      name: id,
      location: loc,
      description: c.description,
      labels: c.labels,
      lifetime: c.lifetime,
      rotation_window_percentage: c.rotationWindowPercentage,
      key_algorithm: c.keyAlgorithm,
      certificate_authority_config: block({
        certificate_authority_service_config: block({ ca_pool: caPool }),
      }),
    }),
  }
}

/** providerBlock is a certificate provider as an OpenTofu block. */
function providerBlock(p?: CertificateProvider): Block | undefined {
  if (!p) return undefined
  return block({
    grpc_endpoint: p.grpcEndpoint && block({ target_uri: p.grpcEndpoint.targetUri }),
    certificate_provider_instance:
      p.certificateProviderInstance &&
      block({ plugin_instance: p.certificateProviderInstance.pluginInstance }),
  })
}

/** TLS_INPUT are the fields the TLS policies kinds' create requests take. */
const TLS_INPUT = [
  'description',
  'labels',
  'allowOpen',
  'serverCertificate',
  'mtlsPolicy',
  'sni',
  'clientCertificate',
  'serverValidationCa',
  'trustConfig',
  'wellKnownRoots',
] as const

/**
 * tlsRecipe is server TLS policy, client TLS policy or backend
 * authentication config r, in project when given. gcloud imports a server
 * or client TLS policy from YAML (here JSON, which YAML reads) on stdin, and creates a
 * backend authentication config with flags.
 */
export function tlsRecipe(r: TlsResource, project = projectOf(r.name)): Recipe {
  const ref = tlsRefOf(r.name)
  if (!ref) return {}
  const { coll, id, location } = ref
  const kind = TLS_KINDS[coll]
  const body: Record<string, unknown> = {}
  for (const k of TLS_INPUT) {
    const v = r[k]
    if (v === undefined || v === '' || v === false) continue
    if (typeof v === 'object' && Object.keys(v).length === 0) continue
    body[k] = v
  }
  const common = { project, name: id, location, description: r.description, labels: r.labels }
  const scope = { project, location }
  let cmd: string
  let tofu: string
  switch (coll) {
    case 'backendAuthenticationConfigs': {
      const cert = typeof r.clientCertificate === 'string' ? r.clientCertificate : undefined
      cmd = gcloud(['network-security', kind.gcloud, 'create', id], {
        ...scope,
        description: r.description,
        labels: keyValues(r.labels),
        'trust-config': r.trustConfig,
        // gcloud spells the enum in lowercase with hyphens, e.g. public-roots.
        'well-known-roots': r.wellKnownRoots?.toLowerCase().replaceAll('_', '-'),
        'client-certificate': cert,
      })
      tofu = resource(kind.tofu, id, {
        ...common,
        client_certificate: cert,
        trust_config: r.trustConfig,
        well_known_roots: r.wellKnownRoots,
      })
      break
    }
    case 'serverTlsPolicies': {
      const m = r.mtlsPolicy
      cmd = gcloud(['network-security', kind.gcloud, 'import', id], scope)
      tofu = resource(kind.tofu, id, {
        ...common,
        allow_open: r.allowOpen || undefined,
        server_certificate: providerBlock(r.serverCertificate),
        mtls_policy:
          m &&
          block({
            client_validation_mode: m.clientValidationMode,
            client_validation_trust_config: m.clientValidationTrustConfig,
            client_validation_ca: (m.clientValidationCa ?? []).map((c) => providerBlock(c)!),
          }),
      })
      break
    }
    case 'clientTlsPolicies': {
      const cert = typeof r.clientCertificate === 'object' ? r.clientCertificate : undefined
      cmd = gcloud(['network-security', kind.gcloud, 'import', id], scope)
      tofu = resource(kind.tofu, id, {
        ...common,
        sni: r.sni,
        client_certificate: providerBlock(cert),
        server_validation_ca: (r.serverValidationCa ?? []).map((c) => providerBlock(c)!),
      })
      break
    }
  }
  return {
    gcloud:
      coll === 'backendAuthenticationConfigs'
        ? cmd
        : `${cmd} <<'EOF'\n${JSON.stringify(body, null, 2)}\nEOF`,
    rest: {
      method: 'POST',
      path: `${NS_API}/projects/${project}/locations/${location}/${coll}?${new URLSearchParams({ [kind.idParam]: id })}`,
      body,
    },
    tofu,
  }
}
