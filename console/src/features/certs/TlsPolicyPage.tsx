import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Pencil, Trash2 } from 'lucide-react'
import type { ReactNode } from 'react'

import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { CopyAs } from '@/components/resource/CopyAs'
import { DetailList, JsonView, Mono, type DetailRow } from '@/components/resource/DetailList'
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
import { kindQuery, relPath } from '../lb/api'
import { RefLink } from '../lb/LbLayout'
import { ChooseProject, tlsListPath, tlsPath, TrustConfigLink, useTlsRef } from './CertsLayout'
import { tlsRecipe } from './copy'
import { CertLink } from './MapPage'
import {
  CLIENT_VALIDATION_LABELS,
  deleteTls,
  TLS_KINDS,
  tlsName,
  tlsQuery,
  type CertificateProvider,
  type TlsColl,
  type TlsRef,
  type TlsResource,
} from './tls'

const pairs = (m?: Record<string, string>) => {
  const e = Object.entries(m ?? {})
  return e.length > 0 ? <Mono>{e.map(([k, v]) => `${k}=${v}`).join(', ')}</Mono> : undefined
}

/** providerCell is where a certificate comes from, as the API names it. */
function providerCell(p?: CertificateProvider | string): ReactNode {
  if (!p) return undefined
  if (typeof p === 'string') return <CertLink name={p} />
  const s = p.grpcEndpoint?.targetUri ?? p.certificateProviderInstance?.pluginInstance
  return s ? <Mono>{s}</Mono> : undefined
}

const trust = (name?: string) => (name ? <TrustConfigLink name={name} /> : undefined)

/** kindRows are the rows of r's configuration that its kind has. */
function kindRows(coll: TlsColl, r: TlsResource): DetailRow[] {
  switch (coll) {
    case 'serverTlsPolicies': {
      const mode = r.mtlsPolicy?.clientValidationMode ?? ''
      return [
        ['Client validation', CLIENT_VALIDATION_LABELS[mode] ?? mode],
        ['Trust config', trust(r.mtlsPolicy?.clientValidationTrustConfig)],
        ['Allow open', r.allowOpen ? 'Allow open (plaintext accepted)' : undefined],
        ['Server certificate', providerCell(r.serverCertificate)],
      ]
    }
    case 'clientTlsPolicies':
      return [
        ['SNI', r.sni ? <Mono key="sni">{r.sni}</Mono> : undefined],
        ['Client certificate', providerCell(r.clientCertificate)],
        [
          'Server validation CAs',
          r.serverValidationCa?.length ? String(r.serverValidationCa.length) : undefined,
        ],
      ]
    case 'backendAuthenticationConfigs':
      return [
        ['Trust config', trust(r.trustConfig)],
        ['Well-known roots', r.wellKnownRoots],
        ['Client certificate', providerCell(r.clientCertificate)],
      ]
  }
}

/**
 * TlsPolicyPage is one resource of the TLS policies section: edit and delete, its
 * configuration with links to the trust config and certificate it uses,
 * and, for a server TLS policy, the target HTTPS proxies that use it.
 */
export function TlsPolicyPage({ coll }: { coll: TlsColl }) {
  const ref = useTlsRef(coll)
  const kind = TLS_KINDS[coll]
  const query = useQuery({ ...tlsQuery(ref), enabled: !!ref.project })
  const r = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const del = useMutation({
    mutationFn: () => deleteTls(ref),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['certs'] })
      toast.success(`Deleted ${kind.singular} ${ref.id}.`)
      navigate(tlsListPath(coll))
    },
  })
  if (!ref.project) return <ChooseProject what={`see its ${kind.title.toLowerCase()}`} />

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to={tlsListPath(coll)} className="underline-offset-4 hover:underline">
              {kind.title}
            </Link>{' '}
            / <span className="font-mono">{ref.location}</span>
          </p>
          <h1 className="text-2xl font-semibold">
            <span className="font-mono">{ref.id}</span>
          </h1>
        </div>
        {r && (
          <div className="flex flex-wrap gap-2">
            <CopyAs what={kind.singular} recipe={() => tlsRecipe(r, ref.project)} />
            <Button variant="outline" asChild>
              <Link to={tlsPath(ref, 'edit')}>
                <Pencil aria-hidden />
                Edit {kind.singular}
              </Link>
            </Button>
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Delete {kind.singular}
                </Button>
              }
              title={`Delete ${kind.singular} ${ref.id}?`}
              description={
                coll === 'clientTlsPolicies'
                  ? 'This cannot be undone.'
                  : 'The API refuses while a load balancer uses it. This cannot be undone.'
              }
              confirmLabel={`Delete ${kind.singular}`}
              onConfirm={() => del.mutateAsync()}
            />
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {r && <Details r={r} tref={ref} />}
    </div>
  )
}

function Details({ r, tref }: { r: TlsResource; tref: TlsRef }) {
  const kind = TLS_KINDS[tref.coll]
  const { label } = kind
  return (
    <>
      {tref.coll === 'serverTlsPolicies' && <Proxies tref={tref} />}
      <Card>
        <CardTitle>Configuration</CardTitle>
        <DetailList
          label={`${label} details`}
          rows={[
            ['Name', <Mono key="n">{r.name}</Mono>],
            ['Description', r.description],
            ...kindRows(tref.coll, r),
            ['Labels', pairs(r.labels)],
            ['Created', time(r.createTime)],
            ['Updated', time(r.updateTime)],
          ]}
        />
      </Card>
      <Card>
        <CardTitle>{label} resource</CardTitle>
        <JsonView value={r} label={`${label} JSON`} />
      </Card>
    </>
  )
}

/**
 * Proxies are the target HTTPS proxies whose serverTlsPolicy is this one,
 * in any of the forms a proxy names it.
 */
function Proxies({ tref }: { tref: TlsRef }) {
  const query = useQuery(kindQuery(tref.project, 'targetHttpsProxies'))
  const name = tlsName(tref)
  const users = (query.data ?? []).filter(
    (p) => relPath(p.serverTlsPolicy as string | undefined) === name,
  )
  return (
    <Card>
      <CardTitle>Target HTTPS proxies</CardTitle>
      <QueryStatus query={query} />
      {query.data && users.length === 0 && (
        <p className="text-sm text-muted-foreground">
          No target HTTPS proxy uses this server TLS policy yet.
        </p>
      )}
      {users.length > 0 && (
        <Table aria-label="Target HTTPS proxies">
          <TableHeader>
            <TableRow>
              <TableHead>Target proxy</TableHead>
              <TableHead>URL map</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {users.map((p) => (
              <TableRow key={p.selfLink as string} data-testid="tls-proxy">
                <TableCell>
                  <RefLink link={p.selfLink as string} />
                </TableCell>
                <TableCell>
                  <RefLink link={p.urlMap as string | undefined} />
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}
