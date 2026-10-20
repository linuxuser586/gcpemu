import { trustConfigsQuery, type TrustConfig } from './api'
import { trustPath } from './CertsLayout'
import { ResourceList } from './ResourceList'

/** trustConfigsAll lists the trust configs of every location. */
export const trustConfigsAll = (project: string) => trustConfigsQuery(project, '-')

/** trustCount is how many certificates of one role a trust config holds. */
export function trustCount(
  t: TrustConfig,
  role: 'trustAnchors' | 'intermediateCas' | 'allowlistedCertificates',
): number {
  if (role === 'allowlistedCertificates') return t.allowlistedCertificates?.length ?? 0
  return (t.trustStores ?? []).reduce((n, s) => n + (s[role]?.length ?? 0), 0)
}

/**
 * TrustConfigs lists the chosen Project's trust configs, with how many
 * certificates of each role each one holds.
 */
export function TrustConfigs() {
  return (
    <ResourceList<TrustConfig>
      title="Trust configs"
      singular="trust config"
      query={trustConfigsAll}
      columns={[
        ['Trust anchors', (t) => trustCount(t, 'trustAnchors')],
        ['Intermediate CAs', (t) => trustCount(t, 'intermediateCas')],
        ['Allowlisted', (t) => trustCount(t, 'allowlistedCertificates')],
      ]}
      createPath="/certs/trustConfigs/create"
      pathOf={(r) => trustPath({ location: r.location, trust: r.id })}
      testId="trust-config"
    />
  )
}
