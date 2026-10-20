import { block, expr, gcloud, keyValues, resource, type Recipe } from '@/lib/copyAs'

import { lastSegment, locationOf, type Certificate } from './api'

// Copy as (FR-UI-015) for certificates: gcloud certificate-manager, the
// certificatemanager/v1 create request and the
// google_certificate_manager_certificate resource. The API never returns
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
