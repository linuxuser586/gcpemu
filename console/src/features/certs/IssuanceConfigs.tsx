import { issuanceConfigsQuery, lastSegment, type IssuanceConfig } from './api'
import { issuancePath } from './CertsLayout'
import { ResourceList } from './ResourceList'

/** issuanceConfigsAll lists the certificate issuance configs of every location. */
export const issuanceConfigsAll = (project: string) => issuanceConfigsQuery(project, '-')

/** DEFAULT_KEY_ALGORITHM is the key algorithm of a config that names none. */
export const DEFAULT_KEY_ALGORITHM = 'RSA_2048'

/** caPoolOf is the CA Service CA pool a config issues from. */
export const caPoolOf = (c: IssuanceConfig) =>
  c.certificateAuthorityConfig?.certificateAuthorityServiceConfig?.caPool ?? ''

const DAY = 86400

/** lifetimeLabel is a duration such as "2592000s" in days, when it is whole days. */
export function lifetimeLabel(d?: string): string | undefined {
  if (!d) return undefined
  const s = Number(d.replace(/s$/, ''))
  if (!Number.isFinite(s) || s <= 0 || s % DAY !== 0) return d
  const days = s / DAY
  return `${days} ${days === 1 ? 'day' : 'days'}`
}

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
        ['Key algorithm', (c) => c.keyAlgorithm ?? DEFAULT_KEY_ALGORITHM],
      ]}
      createPath="/certs/issuanceConfigs/create"
      pathOf={(r) => issuancePath({ location: r.location, config: r.id })}
      testId="issuance-config"
    />
  )
}
