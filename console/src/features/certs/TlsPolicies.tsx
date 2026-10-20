import { useQuery } from '@tanstack/react-query'
import { Plus } from 'lucide-react'
import { useState, type ReactNode } from 'react'
import { useParams } from 'react-router'

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

import { time } from '../gcs/format'
import { lastSegment, locationOf } from './api'
import {
  CertsNav,
  ChooseProject,
  TlsNav,
  tlsListPath,
  tlsPath,
  TrustConfigLink,
} from './CertsLayout'
import {
  clientValidation,
  collOf,
  referencedTrustConfig,
  TLS_KINDS,
  tlsListQuery,
  type TlsColl,
  type TlsResource,
} from './tls'

/** summary is what a resource does, in a word or two, for its kind's list. */
function summary(coll: TlsColl, r: TlsResource): ReactNode {
  switch (coll) {
    case 'serverTlsPolicies':
      return clientValidation(r) || (r.allowOpen ? 'Allow open' : 'TLS only')
    case 'clientTlsPolicies':
      return r.sni ? <span className="font-mono">SNI {r.sni}</span> : undefined
    case 'backendAuthenticationConfigs':
      return r.wellKnownRoots === 'PUBLIC_ROOTS' ? 'Public roots' : undefined
  }
}

const SUMMARY_HEADS: Record<TlsColl, string> = {
  serverTlsPolicies: 'Client validation',
  clientTlsPolicies: 'SNI',
  backendAuthenticationConfigs: 'Roots',
}

/**
 * TlsPolicies lists the chosen Project's resources of one kind
 * (the route's :coll, server TLS policies by default) in every location,
 * with the trust config each verifies with. ?q= keeps those whose ID
 * contains it.
 */
export function TlsPolicies() {
  const params = useParams()
  const coll = collOf(params.coll)
  const kind = TLS_KINDS[coll]
  const [view, setView] = useViewState()
  const project = view.project ?? ''
  const [text, setText] = useState(view.q ?? '')
  const query = useQuery({ ...tlsListQuery(project, coll), enabled: !!project })
  if (!project) return <ChooseProject what={`see its ${kind.title.toLowerCase()}`} />
  const all = query.data ?? []
  const shown = all.filter((r) => lastSegment(r.name).includes(text))
  const plural = kind.title.toLowerCase()

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Certificate Manager</h1>
          <p className="text-sm text-muted-foreground">
            {kind.title} of Project <span className="font-mono">{project}</span>
          </p>
        </div>
        <Button asChild>
          <Link to={`${tlsListPath(coll)}/create`}>
            <Plus aria-hidden />
            Create {kind.singular}
          </Link>
        </Button>
      </div>
      <CertsNav />
      <TlsNav />
      <Card>
        <form
          role="search"
          aria-label={`Filter ${plural}`}
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => e.preventDefault()}
        >
          <label htmlFor="tls-filter" className="flex min-w-48 flex-1 flex-col gap-1 text-sm">
            Filter
            <Input
              id="tls-filter"
              type="search"
              placeholder={`${kind.label} ID`}
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
          <p className="text-sm text-muted-foreground">No {plural} in this Project yet.</p>
        )}
        {query.data && all.length > 0 && shown.length === 0 && (
          <p className="text-sm text-muted-foreground">No {plural} match the filter.</p>
        )}
        {shown.length > 0 && (
          <Table aria-label={kind.title}>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Location</TableHead>
                <TableHead>Description</TableHead>
                <TableHead>{SUMMARY_HEADS[coll]}</TableHead>
                <TableHead>Trust config</TableHead>
                <TableHead>Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((r) => {
                const id = lastSegment(r.name)
                const location = locationOf(r.name)
                const trust = referencedTrustConfig(r)
                return (
                  <TableRow key={r.name} data-testid="tls-resource">
                    <TableCell>
                      <Link
                        className="font-mono text-primary underline-offset-4 hover:underline"
                        to={tlsPath({ location, coll, id })}
                      >
                        {id}
                      </Link>
                    </TableCell>
                    <TableCell className="font-mono">{location}</TableCell>
                    <TableCell>{r.description}</TableCell>
                    <TableCell>{summary(coll, r)}</TableCell>
                    <TableCell>{trust ? <TrustConfigLink name={trust} /> : '—'}</TableCell>
                    <TableCell>{time(r.createTime) ?? '—'}</TableCell>
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
