import { useQueries, useQuery } from '@tanstack/react-query'

import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Card, CardTitle } from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'

import { time } from '../gcs/format'
import { lastSegment, locationOf, trustConfigQuery, trustName, type TrustRef } from './api'
import { ChooseProject, tlsListPath, tlsPath, useTrustRef } from './CertsLayout'
import { referencedTrustConfig, TLS_KINDS, tlsListQuery, tlsRefOf, type TlsColl } from './tls'

// The kinds that verify with a trust config.
const USERS: TlsColl[] = ['serverTlsPolicies', 'backendAuthenticationConfigs']

/**
 * TrustConfigPage is one trust config, read-only: what it holds and the
 * server TLS policies and backend authentication configs that verify
 * with it.
 */
export function TrustConfigPage() {
  const ref = useTrustRef()
  const query = useQuery({ ...trustConfigQuery(ref), enabled: !!ref.project })
  const t = query.data
  if (!ref.project) return <ChooseProject what="see its trust configs" />
  const stores = t?.trustStores ?? []
  const count = (f: 'trustAnchors' | 'intermediateCas') =>
    String(stores.reduce((n, s) => n + (s[f]?.length ?? 0), 0))

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-col gap-1">
        <p className="text-sm text-muted-foreground">
          <Link
            to={tlsListPath('serverTlsPolicies')}
            className="underline-offset-4 hover:underline"
          >
            TLS policies
          </Link>{' '}
          / Trust configs / <span className="font-mono">{ref.location}</span>
        </p>
        <h1 className="text-2xl font-semibold">
          <span className="font-mono">{ref.trust}</span>
        </h1>
      </div>
      <QueryStatus query={query} />
      {t && (
        <>
          <Users r={ref} />
          <Card>
            <CardTitle>Configuration</CardTitle>
            <DetailList
              label="Trust config details"
              rows={[
                ['Name', <Mono key="n">{t.name}</Mono>],
                ['Description', t.description],
                ['Trust anchors', count('trustAnchors')],
                ['Intermediate CAs', count('intermediateCas')],
                ['Allowlisted certificates', String(t.allowlistedCertificates?.length ?? 0)],
                ['Created', time(t.createTime)],
                ['Updated', time(t.updateTime)],
              ]}
            />
          </Card>
          <Card>
            <CardTitle>Trust config resource</CardTitle>
            <JsonView value={t} label="Trust config JSON" />
          </Card>
        </>
      )}
    </div>
  )
}

/** Users are the TLS policies that verify with the trust config. */
function Users({ r }: { r: TrustRef }) {
  const name = trustName(r)
  const lists = useQueries({ queries: USERS.map((coll) => tlsListQuery(r.project, coll)) })
  const users = lists.flatMap((q) => q.data ?? []).filter((x) => referencedTrustConfig(x) === name)
  return (
    <Card>
      <CardTitle>Used by</CardTitle>
      {lists.map((q, i) => (
        <QueryStatus key={USERS[i]} query={q} />
      ))}
      {lists.every((q) => q.data) && users.length === 0 && (
        <p className="text-sm text-muted-foreground">
          No server TLS policy or backend authentication config uses this trust config.
        </p>
      )}
      {users.length > 0 && (
        <Table aria-label="Used by">
          <TableHeader>
            <TableRow>
              <TableHead>Type</TableHead>
              <TableHead>Name</TableHead>
              <TableHead>Location</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {users.map((u) => {
              const ref = tlsRefOf(u.name)!
              return (
                <TableRow key={u.name} data-testid="trust-user">
                  <TableCell>{TLS_KINDS[ref.coll].label}</TableCell>
                  <TableCell>
                    <Link
                      className="font-mono text-primary underline-offset-4 hover:underline"
                      to={tlsPath(ref)}
                    >
                      {lastSegment(u.name)}
                    </Link>
                  </TableCell>
                  <TableCell className="font-mono">{locationOf(u.name)}</TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}
