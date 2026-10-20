import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Pencil, Trash2 } from 'lucide-react'
import type { ReactNode } from 'react'

import { infoQuery } from '@/api/queries'
import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { CopyAs } from '@/components/resource/CopyAs'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import { toast } from '@/lib/toast'

import { dnsQuery, zonesQuery, type DnsQueryResult } from '../dns/api'
import { recordSetPath, rrsetPath, zoneFor } from '../dns/DnsLayout'
import { StatusBadge } from '../gke/format'
import { time } from '../gcs/format'
import {
  DNS_AUTH_TYPES,
  deleteDnsAuthorization,
  dnsAuthorizationQuery,
  type DnsAuthorization,
} from './api'
import { ChooseProject, dnsAuthPath, useDnsAuthRef } from './CertsLayout'
import { dnsAuthorizationRecipe } from './copy'

const pairs = (m?: Record<string, string>) => {
  const e = Object.entries(m ?? {})
  return e.length > 0 ? <Mono>{e.map(([k, v]) => `${k}=${v}`).join(', ')}</Mono> : undefined
}

const fqdn = (s: string) => (s.endsWith('.') ? s : `${s}.`).toLowerCase()

/** Resolution is what the Instance's DNS answers for a DNS authorization's record. */
export type Resolution =
  { state: 'resolves' } | { state: 'other'; data: string } | { state: 'none'; rcode: string }

/** resolution compares a CNAME query's answer with the data the record asks for. */
export function resolution(q: DnsQueryResult, data: string): Resolution {
  const cname = q.answer.find((a) => a.type === 'CNAME' && fqdn(a.name) === fqdn(q.name))
  if (!cname) return { state: 'none', rcode: q.rcode === 'NOERROR' ? 'no CNAME record' : q.rcode }
  return fqdn(cname.data) === fqdn(data)
    ? { state: 'resolves' }
    : { state: 'other', data: cname.data }
}

/**
 * DnsAuthorizationPage is one DNS authorization: edit and delete, the
 * CNAME record it asks for with whether the Instance's DNS resolves it and
 * a link that creates it in the Cloud DNS zone that holds it, its
 * configuration and the resource as the API returns it.
 */
export function DnsAuthorizationPage() {
  const ref = useDnsAuthRef()
  const query = useQuery({ ...dnsAuthorizationQuery(ref), enabled: !!ref.project })
  const a = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const del = useMutation({
    mutationFn: () => deleteDnsAuthorization(ref),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['certs'] })
      toast.success(`Deleted DNS authorization ${ref.auth}.`)
      navigate('/certs/dnsAuthorizations')
    },
  })
  if (!ref.project) return <ChooseProject what="see its DNS authorizations" />

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to="/certs/dnsAuthorizations" className="underline-offset-4 hover:underline">
              DNS authorizations
            </Link>{' '}
            / <span className="font-mono">{ref.location}</span>
          </p>
          <h1 className="text-2xl font-semibold">
            <span className="font-mono">{ref.auth}</span>
          </h1>
        </div>
        {a && (
          <div className="flex flex-wrap gap-2">
            <CopyAs
              what="DNS authorization"
              recipe={() => dnsAuthorizationRecipe(a, ref.project)}
            />
            <Button variant="outline" asChild>
              <Link to={dnsAuthPath(ref, 'edit')}>
                <Pencil aria-hidden />
                Edit DNS authorization
              </Link>
            </Button>
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Delete DNS authorization
                </Button>
              }
              title={`Delete DNS authorization ${ref.auth}?`}
              description="The API refuses while a certificate uses it. This cannot be undone."
              confirmLabel="Delete DNS authorization"
              onConfirm={() => del.mutateAsync()}
            />
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {a && (
        <>
          <Record a={a} project={ref.project} />
          <Card>
            <CardTitle>Configuration</CardTitle>
            <DetailList
              label="DNS authorization details"
              rows={[
                ['Name', <Mono key="n">{a.name}</Mono>],
                ['Domain', a.domain ? <Mono key="d">{a.domain}</Mono> : undefined],
                ['Type', DNS_AUTH_TYPES[a.type ?? ''] ?? a.type],
                ['Description', a.description],
                ['Labels', pairs(a.labels)],
                ['Created', time(a.createTime)],
                ['Updated', time(a.updateTime)],
              ]}
            />
          </Card>
          <Card>
            <CardTitle>DNS authorization resource</CardTitle>
            <JsonView value={a} label="DNS authorization JSON" />
          </Card>
        </>
      )}
    </div>
  )
}

/**
 * Record is the CNAME record a DNS authorization asks for: whether the
 * Instance's DNS resolves it, and how to fix it when it does not.
 */
function Record({ a, project }: { a: DnsAuthorization; project: string }) {
  const rec = a.dnsResourceRecord
  const name = rec?.name ?? ''
  const data = rec?.data ?? ''
  const type = rec?.type ?? 'CNAME'
  const { data: info } = useQuery(infoQuery())
  const dnsOn = !!info?.services.includes('dns')
  const answer = useQuery({
    ...dnsQuery({ name, type, network: '' }),
    enabled: dnsOn && !!name,
  })
  const r = answer.data && resolution(answer.data, data)
  const zones = useQuery({
    ...zonesQuery(project),
    enabled: dnsOn && !!r && r.state !== 'resolves',
  })
  const zone = zones.data && zoneFor(name, zones.data)

  let status: ReactNode
  let fix: ReactNode
  if (!dnsOn) {
    fix = 'Add the record to the DNS zone of the domain.'
  } else if (r?.state === 'resolves') {
    status = <StatusBadge status="Resolves" tone="success" />
  } else if (r) {
    status =
      r.state === 'other' ? (
        <StatusBadge status={`Resolves to ${r.data} instead`} tone="destructive" />
      ) : (
        <StatusBadge status={`Does not resolve (${r.rcode})`} tone="default" />
      )
    const link = 'text-primary underline-offset-4 hover:underline'
    if (zones.data && !zone) {
      fix = (
        <Link className={link} to="/dns/create">
          Create a public zone for it
        </Link>
      )
    } else if (zone && r.state === 'other') {
      fix = (
        <Link className={link} to={rrsetPath(zone.name, name, type, 'edit')}>
          Edit the CNAME record
        </Link>
      )
    } else if (zone) {
      fix = (
        <Link className={link} to={recordSetPath(zone, { name, type, data })}>
          Create the CNAME record
        </Link>
      )
    }
  }

  return (
    <Card>
      <CardTitle>DNS record</CardTitle>
      <p className="text-sm text-muted-foreground">
        Certificates that name this DNS authorization are issued once this record is in the
        domain&apos;s DNS zone.
      </p>
      <QueryStatus query={answer} />
      <DetailList
        label="DNS record"
        rows={[
          ['Name', <Mono key="n">{name}</Mono>],
          ['Type', <Mono key="t">{type}</Mono>],
          ['Data', <Mono key="d">{data}</Mono>],
          ['Status', status],
          ['Fix', fix],
        ]}
      />
    </Card>
  )
}
