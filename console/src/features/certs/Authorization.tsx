import { useQuery } from '@tanstack/react-query'
import type { ReactNode } from 'react'

import { infoQuery } from '@/api/queries'
import { Link } from '@/components/Link'
import { Mono } from '@/components/resource/DetailList'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'

import { zonesQuery } from '../dns/api'
import { recordSetPath, zoneFor } from '../dns/DnsLayout'
import { StatusBadge } from '../gke/format'
import { time } from '../gcs/format'
import type { AuthorizationAttemptInfo, Certificate } from './api'

// The per-domain authorization status of a Google-managed certificate
// (FR-LB-004): what the Service expected to find against what it
// resolved, and a link to the fix of each failing domain. A DNS
// authorization's CNAME is added in the Cloud DNS zone that holds it; a
// load balancer authorization needs the domain to resolve to one of the
// Project's forwarding rules.

const TONES: Record<string, 'success' | 'warning' | 'destructive' | 'default'> = {
  AUTHORIZED: 'success',
  AUTHORIZING: 'warning',
  FAILED: 'destructive',
}

const failing = (a: AuthorizationAttemptInfo) => a.state !== 'AUTHORIZED'

const list = (v?: string[]) => (v?.length ? v.join(', ') : '—')

/** AuthorizationTable shows each domain of a Google-managed certificate. */
export function AuthorizationTable({ cert, project }: { cert: Certificate; project: string }) {
  const attempts = cert.managed?.authorizationAttemptInfo ?? []
  const { data: info } = useQuery(infoQuery())
  const needsZone = attempts.some((a) => failing(a) && a.troubleshooting?.cname?.name)
  const dnsOn = !!info?.services.includes('dns')
  const zones = useQuery({ ...zonesQuery(project), enabled: needsZone && dnsOn && !!project })
  const lbOn = !!info?.services.includes('lb')

  const fix = (a: AuthorizationAttemptInfo): ReactNode => {
    if (!failing(a)) return undefined
    const t = a.troubleshooting
    if (t?.cname?.name && t.cname.expectedData) {
      if (!dnsOn) return 'Add the CNAME record to the DNS zone of the domain.'
      if (!zones.data) return undefined
      const zone = zoneFor(t.cname.name, zones.data)
      return zone ? (
        <Link
          className="text-primary underline-offset-4 hover:underline"
          to={recordSetPath(zone, {
            name: t.cname.name,
            type: 'CNAME',
            data: t.cname.expectedData,
          })}
        >
          Create the CNAME record
        </Link>
      ) : (
        <Link className="text-primary underline-offset-4 hover:underline" to="/dns/create">
          Create a public zone for it
        </Link>
      )
    }
    if (t?.ips) {
      if (!lbOn) return 'Point the domain at a forwarding rule of the Project.'
      return (
        <Link className="text-primary underline-offset-4 hover:underline" to="/lb">
          See the forwarding rules
        </Link>
      )
    }
    return undefined
  }

  return (
    <Table aria-label="Domain authorization">
      <TableHeader>
        <TableRow>
          <TableHead>Domain</TableHead>
          <TableHead>State</TableHead>
          <TableHead>Reason</TableHead>
          <TableHead>Expected</TableHead>
          <TableHead>Resolved</TableHead>
          <TableHead>Fix</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {attempts.map((a) => {
          const t = a.troubleshooting
          const s = a.state ?? 'AUTHORIZING'
          return (
            <TableRow key={a.domain} data-testid="authorization">
              <TableCell>
                <Mono>{a.domain}</Mono>
                {a.attemptTime && (
                  <p className="text-xs text-muted-foreground">Checked {time(a.attemptTime)}</p>
                )}
              </TableCell>
              <TableCell>
                <StatusBadge status={s} tone={TONES[s] ?? 'default'} />
              </TableCell>
              <TableCell className="text-sm">
                {a.failureReason && <p className="font-mono text-xs">{a.failureReason}</p>}
                {a.details}
                {t?.issues?.length ? (
                  <p className="font-mono text-xs text-muted-foreground">{t.issues.join(', ')}</p>
                ) : null}
              </TableCell>
              <TableCell className="font-mono text-xs">
                {t?.cname && (
                  <>
                    CNAME {t.cname.name} → {t.cname.expectedData}
                  </>
                )}
                {t?.ips && <>IP {list(t.ips.serving)}</>}
              </TableCell>
              <TableCell className="font-mono text-xs">
                {t?.cname && list(t.cname.resolvedData)}
                {t?.ips && list(t.ips.resolved)}
              </TableCell>
              <TableCell className="text-sm">{fix(a)}</TableCell>
            </TableRow>
          )
        })}
      </TableBody>
    </Table>
  )
}
