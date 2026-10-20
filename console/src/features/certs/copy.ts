import { block, expr, gcloud, keyValues, resource, type Recipe } from '@/lib/copyAs'

import {
  lastSegment,
  locationOf,
  type Certificate,
  type CertificateMap,
  type CertificateMapEntry,
} from './api'

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
