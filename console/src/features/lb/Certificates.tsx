import { DetailList, Mono } from '@/components/resource/DetailList'
import { Card, CardTitle } from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'

import { StatusBadge } from '../gke/format'
import type { Resource } from './api'

interface Managed {
  domains?: string[]
  status?: string
  domainStatus?: Record<string, string>
}

const TONES: Record<string, 'success' | 'warning' | 'destructive' | 'default'> = {
  ACTIVE: 'success',
  PROVISIONING: 'warning',
  RENEWAL_FAILED: 'destructive',
  PROVISIONING_FAILED: 'destructive',
  PROVISIONING_FAILED_PERMANENTLY: 'destructive',
  FAILED_NOT_VISIBLE: 'destructive',
  FAILED_CAA_CHECKING: 'destructive',
  FAILED_CAA_FORBIDDEN: 'destructive',
  FAILED_RATE_LIMITED: 'destructive',
  EXPIRED: 'destructive',
}

const managedOf = (c: Resource) => (c.managed ?? {}) as Managed

/**
 * certStatus is a Google-managed certificate's status; a self-managed one
 * is ACTIVE until it expires (by the browser's clock).
 */
export function certStatus(c: Resource): string {
  if (c.type === 'MANAGED') return managedOf(c).status ?? 'PROVISIONING'
  const exp = typeof c.expireTime === 'string' ? Date.parse(c.expireTime) : NaN
  return exp < Date.now() ? 'EXPIRED' : 'ACTIVE'
}

/** certDomains are the names a certificate covers, or will. */
export const certDomains = (c: Resource) =>
  (c.subjectAlternativeNames as string[] | undefined)?.length
    ? (c.subjectAlternativeNames as string[])
    : (managedOf(c).domains ?? [])

export function CertStatusBadge({ cert }: { cert: Resource }) {
  const s = certStatus(cert)
  return <StatusBadge status={s} tone={TONES[s] ?? 'default'} />
}

/** CertificateStatus shows a certificate's status and each domain's. */
export function CertificateStatus({ cert }: { cert: Resource }) {
  const m = managedOf(cert)
  const domains = Object.entries(m.domainStatus ?? {})
  return (
    <Card aria-labelledby="cert-title">
      <CardTitle id="cert-title">Certificate status</CardTitle>
      <DetailList
        label="Certificate status"
        rows={[
          ['Status', <CertStatusBadge key="s" cert={cert} />],
          ['Type', cert.type === 'MANAGED' ? 'Google-managed' : 'Self-managed'],
          [
            'Subject alternative names',
            certDomains(cert).length > 0 && <Mono>{certDomains(cert).join(', ')}</Mono>,
          ],
          [
            'Expires',
            typeof cert.expireTime === 'string' && new Date(cert.expireTime).toLocaleString(),
          ],
        ]}
      />
      {cert.type === 'MANAGED' && (
        <>
          <p className="text-sm text-muted-foreground">
            The emulator’s CA issues the certificate once emulated DNS for every domain points at a
            forwarding rule whose HTTPS proxy uses it (PROVISIONING → ACTIVE).
          </p>
          {domains.length > 0 && (
            <Table aria-label="Domain status">
              <TableHeader>
                <TableRow>
                  <TableHead>Domain</TableHead>
                  <TableHead>Status</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {domains.map(([d, s]) => (
                  <TableRow key={d}>
                    <TableCell className="font-mono">{d}</TableCell>
                    <TableCell>
                      <StatusBadge status={s} tone={TONES[s] ?? 'default'} />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </>
      )}
    </Card>
  )
}
