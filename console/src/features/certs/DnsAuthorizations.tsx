import { DNS_AUTH_TYPES, dnsAuthorizationsQuery, type DnsAuthorization } from './api'
import { dnsAuthPath } from './CertsLayout'
import { ResourceList } from './ResourceList'

/** dnsAuthorizationsAll lists the DNS authorizations of every location. */
export const dnsAuthorizationsAll = (project: string) => dnsAuthorizationsQuery(project, '-')

/**
 * DnsAuthorizations lists the chosen Project's DNS authorizations, with
 * the domain each proves control of.
 */
export function DnsAuthorizations() {
  return (
    <ResourceList<DnsAuthorization>
      title="DNS authorizations"
      singular="DNS authorization"
      query={dnsAuthorizationsAll}
      columns={[
        ['Domain', (a) => <span className="font-mono">{a.domain}</span>],
        ['Type', (a) => DNS_AUTH_TYPES[a.type ?? ''] ?? a.type],
      ]}
      createPath="/certs/dnsAuthorizations/create"
      pathOf={(r) => dnsAuthPath({ location: r.location, auth: r.id })}
      testId="dns-authorization"
    />
  )
}
