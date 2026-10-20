import { trustConfigsQuery, trustCount, type TrustConfig } from './api'
import { trustPath } from './CertsLayout'
import { ResourceList } from './ResourceList'

/** trustConfigsAll lists the trust configs of every location. */
export const trustConfigsAll = (project: string) => trustConfigsQuery(project, '-')

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
