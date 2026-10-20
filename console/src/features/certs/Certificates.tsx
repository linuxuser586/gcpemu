import { useQuery } from '@tanstack/react-query'
import { Plus } from 'lucide-react'
import { useState } from 'react'

import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { useViewState } from '@/lib/viewState'

import { StatusBadge } from '../gke/format'
import { time } from '../gcs/format'
import {
  certDomains,
  certificatesQuery,
  certKind,
  certState,
  KIND_LABELS,
  lastSegment,
  locationOf,
  scopeLabel,
  type Certificate,
} from './api'
import { certPath, CertsNav, ChooseProject } from './CertsLayout'
import { RotateCa } from './RotateCa'

const TONES: Record<string, 'success' | 'warning' | 'destructive' | 'default'> = {
  ACTIVE: 'success',
  PROVISIONING: 'warning',
  FAILED: 'destructive',
  EXPIRED: 'destructive',
}

export function CertStateBadge({ cert }: { cert: Certificate }) {
  const s = certState(cert)
  return <StatusBadge status={s} tone={TONES[s] ?? 'default'} />
}

/**
 * Certificates lists the chosen Project's certificates in every location.
 * ?q= keeps those whose ID or domains contain it.
 */
export function Certificates() {
  const [view, setView] = useViewState()
  const project = view.project ?? ''
  const [text, setText] = useState(view.q ?? '')
  const query = useQuery({ ...certificatesQuery(project), enabled: !!project })
  // Rotate CA is the Instance's, so it needs no Project.
  if (!project) return <ChooseProject what="see its certificates" actions={<RotateCa />} />
  const all = query.data ?? []
  const shown = all.filter(
    (c) => lastSegment(c.name).includes(text) || certDomains(c).some((d) => d.includes(text)),
  )

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Certificate Manager</h1>
          <p className="text-sm text-muted-foreground">
            Certificates of Project <span className="font-mono">{project}</span>
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          <RotateCa />
          <Button asChild>
            <Link to="/certs/create">
              <Plus aria-hidden />
              Create certificate
            </Link>
          </Button>
        </div>
      </div>
      <CertsNav />
      <Card>
        <form
          role="search"
          aria-label="Filter certificates"
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => e.preventDefault()}
        >
          <label htmlFor="certs-filter" className="flex min-w-48 flex-1 flex-col gap-1 text-sm">
            Filter
            <Input
              id="certs-filter"
              type="search"
              placeholder="Certificate ID or domain"
              value={text}
              onChange={(e) => {
                setText(e.target.value)
                setView({ q: e.target.value })
              }}
            />
          </label>
        </form>
        <QueryStatus query={query} />
        {query.data && all.length === 0 && (
          <p className="text-sm text-muted-foreground">No certificates in this Project yet.</p>
        )}
        {query.data && all.length > 0 && shown.length === 0 && (
          <p className="text-sm text-muted-foreground">No certificates match the filter.</p>
        )}
        {shown.length > 0 && (
          <Table aria-label="Certificates">
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Location</TableHead>
                <TableHead>Scope</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Domains</TableHead>
                <TableHead>State</TableHead>
                <TableHead>Expires</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((c) => {
                const id = lastSegment(c.name)
                const location = locationOf(c.name)
                return (
                  <TableRow key={c.name} data-testid="certificate">
                    <TableCell>
                      <Link
                        className="font-mono text-primary underline-offset-4 hover:underline"
                        to={certPath({ location, cert: id })}
                      >
                        {id}
                      </Link>
                    </TableCell>
                    <TableCell className="font-mono">{location}</TableCell>
                    <TableCell>{scopeLabel(c.scope)}</TableCell>
                    <TableCell>{KIND_LABELS[certKind(c)]}</TableCell>
                    <TableCell className="font-mono text-xs">{certDomains(c).join(', ')}</TableCell>
                    <TableCell>
                      <CertStateBadge cert={c} />
                    </TableCell>
                    <TableCell>{time(c.expireTime) ?? '—'}</TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  )
}
