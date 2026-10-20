import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Pencil, Trash2 } from 'lucide-react'

import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { CopyAs } from '@/components/resource/CopyAs'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import { toast } from '@/lib/toast'

import { time } from '../gcs/format'
import {
  certDomains,
  certificateQuery,
  certKind,
  deleteCertificate,
  KIND_LABELS,
  scopeLabel,
  type Certificate,
} from './api'
import { AuthorizationTable } from './Authorization'
import { CertStateBadge } from './Certificates'
import { certPath, ChooseProject, useCertRef } from './CertsLayout'
import { certificateRecipe } from './copy'

const pairs = (m?: Record<string, string>) => {
  const e = Object.entries(m ?? {})
  return e.length > 0 ? <Mono>{e.map(([k, v]) => `${k}=${v}`).join(', ')}</Mono> : undefined
}

/**
 * CertificatePage is one certificate: Copy as, edit and delete above its
 * authorization status (a Google-managed certificate), its configuration
 * and what uses it. It follows a provisioning certificate until it is
 * ACTIVE. The API never returns a private key, so none is shown.
 */
export function CertificatePage() {
  const ref = useCertRef()
  const query = useQuery({ ...certificateQuery(ref), enabled: !!ref.project })
  const c = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const del = useMutation({
    mutationFn: () => deleteCertificate(ref),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['certs'] })
      toast.success(`Deleted certificate ${ref.cert}.`)
      navigate('/certs')
    },
  })
  if (!ref.project) return <ChooseProject what="see its certificates" />

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to="/certs" className="underline-offset-4 hover:underline">
              Certificates
            </Link>{' '}
            / <span className="font-mono">{ref.location}</span>
          </p>
          <h1 className="text-2xl font-semibold">
            <span className="font-mono">{ref.cert}</span>
          </h1>
        </div>
        {c && (
          <div className="flex flex-wrap gap-2">
            <CopyAs what="certificate" recipe={() => certificateRecipe(c, ref.project)} />
            <Button variant="outline" asChild>
              <Link to={certPath(ref, 'edit')}>
                <Pencil aria-hidden />
                Edit certificate
              </Link>
            </Button>
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Delete certificate
                </Button>
              }
              title={`Delete certificate ${ref.cert}?`}
              description="The certificate and its private key are deleted. A certificate map entry that uses it must be deleted first. This cannot be undone."
              confirmLabel="Delete certificate"
              onConfirm={() => del.mutateAsync()}
            />
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {c && <CertificateDetails c={c} project={ref.project} />}
    </div>
  )
}

function CertificateDetails({ c, project }: { c: Certificate; project: string }) {
  const m = c.managed
  const issue = m?.state !== 'ACTIVE' ? m?.provisioningIssue : undefined
  return (
    <>
      {issue && (
        <Card role="alert" className="border-destructive/50">
          <p className="text-sm">
            <span className="font-mono">{issue.reason}</span>: {issue.details}
          </p>
        </Card>
      )}
      {m && (
        <Card>
          <CardTitle>Domain authorization</CardTitle>
          {m.issuanceConfig ? (
            <p className="text-sm text-muted-foreground">
              Issued through the certificate issuance config{' '}
              <span className="font-mono">{m.issuanceConfig}</span>, without domain authorization.
            </p>
          ) : (
            <AuthorizationTable cert={c} project={project} />
          )}
        </Card>
      )}
      <Card>
        <CardTitle>Configuration</CardTitle>
        <DetailList
          label="Certificate details"
          rows={[
            ['Name', <Mono key="n">{c.name}</Mono>],
            ['State', <CertStateBadge key="s" cert={c} />],
            ['Type', KIND_LABELS[certKind(c)]],
            ['Scope', scopeLabel(c.scope)],
            ['Description', c.description],
            ['Labels', pairs(c.labels)],
            ['Domains', <Mono key="d">{certDomains(c).join(', ')}</Mono>],
            [
              'DNS authorizations',
              m?.dnsAuthorizations?.length ? (
                <Mono>{m.dnsAuthorizations.join(', ')}</Mono>
              ) : undefined,
            ],
            ['Expires', time(c.expireTime)],
            ['Created', time(c.createTime)],
            ['Updated', time(c.updateTime)],
          ]}
        />
      </Card>
      <Card>
        <CardTitle>Used by</CardTitle>
        {c.usedBy?.length ? (
          <ul aria-label="Used by" className="flex flex-col gap-1 text-sm">
            {c.usedBy.map((u) => (
              <li key={u.name}>
                <Mono>{u.name}</Mono>
              </li>
            ))}
          </ul>
        ) : (
          <p className="text-sm text-muted-foreground">
            No certificate map entry uses this certificate.
          </p>
        )}
      </Card>
      {c.pemCertificate && (
        <Card>
          <CardTitle>PEM certificate</CardTitle>
          <pre
            aria-label="PEM certificate"
            className="max-h-64 overflow-auto rounded-md bg-muted p-3 font-mono text-xs"
          >
            {c.pemCertificate}
          </pre>
        </Card>
      )}
      <Card>
        <CardTitle>Certificate resource</CardTitle>
        <JsonView value={c} label="Certificate JSON" />
      </Card>
    </>
  )
}
