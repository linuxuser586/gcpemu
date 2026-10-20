import { useMutation, useQueries, useQuery, useQueryClient } from '@tanstack/react-query'
import { Pencil, Trash2 } from 'lucide-react'

import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { CopyAs } from '@/components/resource/CopyAs'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { toast } from '@/lib/toast'

import { time } from '../gcs/format'
import {
  deleteTrustConfig,
  lastSegment,
  locationOf,
  trustConfigQuery,
  trustName,
  type TrustConfig,
  type TrustRef,
} from './api'
import { ChooseProject, tlsPath, trustPath, useTrustRef } from './CertsLayout'
import { trustConfigRecipe } from './copy'
import { PemSummary } from './PemSummary'
import { referencedTrustConfig, TLS_KINDS, tlsListQuery, tlsRefOf, type TlsColl } from './tls'
import { trustCount } from './TrustConfigs'

// The kinds that verify with a trust config.
const USERS: TlsColl[] = ['serverTlsPolicies', 'backendAuthenticationConfigs']

const pairs = (m?: Record<string, string>) => {
  const e = Object.entries(m ?? {})
  return e.length > 0 ? <Mono>{e.map(([k, v]) => `${k}=${v}`).join(', ')}</Mono> : undefined
}

/**
 * TrustConfigPage is one trust config: edit and delete, each certificate
 * it holds with its subject and expiry, the server TLS policies and
 * backend authentication configs that verify with it, its configuration
 * and the resource as the API returns it.
 */
export function TrustConfigPage() {
  const ref = useTrustRef()
  const query = useQuery({ ...trustConfigQuery(ref), enabled: !!ref.project })
  const t = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const del = useMutation({
    mutationFn: () => deleteTrustConfig(ref),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['certs'] })
      toast.success(`Deleted trust config ${ref.trust}.`)
      navigate('/certs/trustConfigs')
    },
  })
  if (!ref.project) return <ChooseProject what="see its trust configs" />

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to="/certs/trustConfigs" className="underline-offset-4 hover:underline">
              Trust configs
            </Link>{' '}
            / <span className="font-mono">{ref.location}</span>
          </p>
          <h1 className="text-2xl font-semibold">
            <span className="font-mono">{ref.trust}</span>
          </h1>
        </div>
        {t && (
          <div className="flex flex-wrap gap-2">
            <CopyAs what="trust config" recipe={() => trustConfigRecipe(t, ref.project)} />
            <Button variant="outline" asChild>
              <Link to={trustPath(ref, 'edit')}>
                <Pencil aria-hidden />
                Edit trust config
              </Link>
            </Button>
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Delete trust config
                </Button>
              }
              title={`Delete trust config ${ref.trust}?`}
              description="The API refuses while a server TLS policy or backend authentication config uses it. This cannot be undone."
              confirmLabel="Delete trust config"
              onConfirm={() => del.mutateAsync()}
            />
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {t && (
        <>
          <Certificates t={t} />
          <Users r={ref} />
          <Card>
            <CardTitle>Configuration</CardTitle>
            <DetailList
              label="Trust config details"
              rows={[
                ['Name', <Mono key="n">{t.name}</Mono>],
                ['Description', t.description],
                ['Trust anchors', String(trustCount(t, 'trustAnchors'))],
                ['Intermediate CAs', String(trustCount(t, 'intermediateCas'))],
                ['Allowlisted certificates', String(trustCount(t, 'allowlistedCertificates'))],
                ['Labels', pairs(t.labels)],
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

/** trustPems are a trust config's PEM certificates, each with its role. */
function trustPems(t: TrustConfig): [role: string, pem: string][] {
  const out: [string, string][] = []
  for (const s of t.trustStores ?? []) {
    for (const a of s.trustAnchors ?? []) out.push(['Trust anchor', a.pemCertificate ?? ''])
    for (const a of s.intermediateCas ?? []) out.push(['Intermediate CA', a.pemCertificate ?? ''])
  }
  for (const a of t.allowlistedCertificates ?? []) out.push(['Allowlisted', a.pemCertificate ?? ''])
  return out
}

/** Certificates are each PEM certificate a trust config holds, by role. */
function Certificates({ t }: { t: TrustConfig }) {
  const pems = trustPems(t)
  return (
    <Card>
      <CardTitle>Certificates</CardTitle>
      {pems.length === 0 ? (
        <p className="text-sm text-muted-foreground">This trust config holds no certificates.</p>
      ) : (
        <Table aria-label="Certificates">
          <TableHeader>
            <TableRow>
              <TableHead>Role</TableHead>
              <TableHead>Certificate</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {pems.map(([role, pem], i) => (
              <TableRow key={i} data-testid="trust-certificate">
                <TableCell>{role}</TableCell>
                <TableCell>
                  <PemSummary pem={pem} />
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Card>
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
