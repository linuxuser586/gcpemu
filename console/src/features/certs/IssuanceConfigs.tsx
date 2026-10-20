import {
  caPoolOf,
  issuanceConfigsQuery,
  keyAlgorithmLabel,
  lastSegment,
  lifetimeLabel,
  type IssuanceConfig,
} from './api'
import { issuancePath } from './CertsLayout'
import { ResourceList } from './ResourceList'

/** issuanceConfigsAll lists the certificate issuance configs of every location. */
export const issuanceConfigsAll = (project: string) => issuanceConfigsQuery(project, '-')

/**
 * IssuanceConfigs lists the chosen Project's certificate issuance
 * configs, with the CA pool, lifetime and key algorithm of each.
 */
export function IssuanceConfigs() {
  return (
    <ResourceList<IssuanceConfig>
      title="Certificate issuance configs"
      singular="certificate issuance config"
      query={issuanceConfigsAll}
      columns={[
        [
          'CA pool',
          (c) => (
            <span className="font-mono" title={caPoolOf(c)}>
              {lastSegment(caPoolOf(c))}
            </span>
          ),
        ],
        ['Lifetime', (c) => lifetimeLabel(c.lifetime)],
        ['Key algorithm', keyAlgorithmLabel],
      ]}
      createPath="/certs/issuanceConfigs/create"
      pathOf={(r) => issuancePath({ location: r.location, config: r.id })}
      testId="issuance-config"
    />
  )
}
