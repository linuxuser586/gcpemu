import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Trash2 } from 'lucide-react'

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

import { StatusBadge } from '../gke/format'
import { time } from '../gcs/format'
import {
  certificatesQuery,
  certState,
  deleteIssuanceConfig,
  issuanceConfigQuery,
  issuanceName,
  locationOf,
  type IssuanceRef,
} from './api'
import { ChooseProject, useIssuanceRef } from './CertsLayout'
import { issuanceConfigRecipe } from './copy'
import { caPoolOf, DEFAULT_KEY_ALGORITHM, lifetimeLabel } from './IssuanceConfigs'
import { CertLink } from './MapPage'

const pairs = (m?: Record<string, string>) => {
  const e = Object.entries(m ?? {})
  return e.length > 0 ? <Mono>{e.map(([k, v]) => `${k}=${v}`).join(', ')}</Mono> : undefined
}

/**
 * IssuanceConfigPage is one certificate issuance config: delete (its
 * settings cannot be changed), the certificates it issues, its
 * configuration and the resource as the API returns it.
 */
export function IssuanceConfigPage() {
  const ref = useIssuanceRef()
  const query = useQuery({ ...issuanceConfigQuery(ref), enabled: !!ref.project })
  const c = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const del = useMutation({
    mutationFn: () => deleteIssuanceConfig(ref),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['certs'] })
      toast.success(`Deleted certificate issuance config ${ref.config}.`)
      navigate('/certs/issuanceConfigs')
    },
  })
  if (!ref.project) return <ChooseProject what="see its certificate issuance configs" />

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to="/certs/issuanceConfigs" className="underline-offset-4 hover:underline">
              Certificate issuance configs
            </Link>{' '}
            / <span className="font-mono">{ref.location}</span>
          </p>
          <h1 className="text-2xl font-semibold">
            <span className="font-mono">{ref.config}</span>
          </h1>
        </div>
        {c && (
          <div className="flex flex-wrap gap-2">
            <CopyAs
              what="certificate issuance config"
              recipe={() => issuanceConfigRecipe(c, ref.project)}
            />
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Delete certificate issuance config
                </Button>
              }
              title={`Delete certificate issuance config ${ref.config}?`}
              description="The API refuses while a certificate uses it. This cannot be undone."
              confirmLabel="Delete certificate issuance config"
              onConfirm={() => del.mutateAsync()}
            />
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {c && (
        <>
          <Issued r={ref} />
          <Card>
            <CardTitle>Configuration</CardTitle>
            <p className="text-sm text-muted-foreground">
              The CA pool, lifetime, rotation window and key algorithm cannot be changed: create
              another certificate issuance config instead.
            </p>
            <DetailList
              label="Certificate issuance config details"
              rows={[
                ['Name', <Mono key="n">{c.name}</Mono>],
                ['Description', c.description],
                ['CA pool', <Mono key="p">{caPoolOf(c)}</Mono>],
                ['Lifetime', lifetimeLabel(c.lifetime)],
                [
                  'Rotation window',
                  c.rotationWindowPercentage !== undefined
                    ? `${c.rotationWindowPercentage}%`
                    : undefined,
                ],
                ['Key algorithm', c.keyAlgorithm ?? DEFAULT_KEY_ALGORITHM],
                ['Labels', pairs(c.labels)],
                ['Created', time(c.createTime)],
                ['Updated', time(c.updateTime)],
              ]}
            />
          </Card>
          <Card>
            <CardTitle>Certificate issuance config resource</CardTitle>
            <JsonView value={c} label="Certificate issuance config JSON" />
          </Card>
        </>
      )}
    </div>
  )
}

/** Issued are the certificates that name the config. */
function Issued({ r }: { r: IssuanceRef }) {
  const query = useQuery(certificatesQuery(r.project))
  const name = issuanceName(r)
  const certs = (query.data ?? []).filter((c) => c.managed?.issuanceConfig === name)
  return (
    <Card>
      <CardTitle>Certificates</CardTitle>
      <QueryStatus query={query} />
      {query.data && certs.length === 0 && (
        <p className="text-sm text-muted-foreground">
          No certificate is issued through this config yet.
        </p>
      )}
      {certs.length > 0 && (
        <Table aria-label="Certificates">
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Location</TableHead>
              <TableHead>State</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {certs.map((c) => {
              const state = certState(c)
              return (
                <TableRow key={c.name} data-testid="issued-certificate">
                  <TableCell>
                    <CertLink name={c.name} />
                  </TableCell>
                  <TableCell className="font-mono">{locationOf(c.name)}</TableCell>
                  <TableCell>
                    <StatusBadge status={state} tone={state === 'ACTIVE' ? 'success' : 'warning'} />
                  </TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}
