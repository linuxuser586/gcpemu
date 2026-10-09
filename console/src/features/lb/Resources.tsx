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

import {
  kindInfo,
  kindQuery,
  lastSeg,
  port,
  listenersQuery,
  refOf,
  relPath,
  scopeOf,
  type Coll,
  type LbListener,
  type Resource,
  text,
} from './api'
import { CertStatusBadge, certDomains } from './Certificates'
import { KindTabs, lbPath, RefLink } from './LbLayout'

type Column = [
  header: string,
  cell: (r: Resource, ctx: { listeners?: Map<string, LbListener> }) => ReactNode,
]

const mono = (v: unknown) =>
  v === undefined || v === '' ? '—' : <span className="font-mono">{text(v)}</span>
const ref = (field: string) => (r: Resource) => <RefLink link={r[field] as string | undefined} />
const yes = (v: unknown) => (v ? 'On' : 'Off')
const len = (v: unknown) => (Array.isArray(v) ? v.length : 0)

/** COLUMNS are each kind's list columns after its name and scope. */
const COLUMNS: Record<Coll, Column[]> = {
  forwardingRules: [
    ['IP:port', (r) => mono(`${text(r.IPAddress)}:${port(r)}`)],
    ['Local listener', (r, { listeners }) => mono(listeners?.get(relPath(r.selfLink))?.listener)],
    ['Target', ref('target')],
    ['Scheme', (r) => mono(r.loadBalancingScheme)],
  ],
  targetHttpProxies: [['URL map', ref('urlMap')]],
  targetHttpsProxies: [
    ['URL map', ref('urlMap')],
    [
      'Certificates',
      (r) =>
        len(r.sslCertificates) ? (
          <span className="flex flex-wrap gap-x-2">
            {(r.sslCertificates as string[]).map((c) => (
              <RefLink key={c} link={c} />
            ))}
          </span>
        ) : r.certificateMap ? (
          mono(lastSeg(r.certificateMap as string))
        ) : (
          '—'
        ),
    ],
    ['SSL policy', ref('sslPolicy')],
  ],
  urlMaps: [
    ['Default backend', ref('defaultService')],
    ['Host rules', (r) => len(r.hostRules)],
    ['Path matchers', (r) => len(r.pathMatchers)],
  ],
  backendServices: [
    ['Protocol', (r) => mono(r.protocol)],
    ['Backends', (r) => len(r.backends)],
    ['Health check', (r) => <RefLink link={(r.healthChecks as string[] | undefined)?.[0]} />],
    ['Cloud CDN', (r) => yes(r.enableCdn)],
  ],
  backendBuckets: [
    ['Bucket', (r) => mono(r.bucketName)],
    ['Cloud CDN', (r) => yes(r.enableCdn)],
  ],
  healthChecks: [
    ['Protocol', (r) => mono(r.type)],
    [
      'Port',
      (r) => {
        const block = Object.entries(r).find(([k]) => k.endsWith('HealthCheck'))?.[1] as
          { port?: number; portSpecification?: string } | undefined
        return mono(block?.portSpecification === 'USE_SERVING_PORT' ? 'serving port' : block?.port)
      },
    ],
    ['Interval', (r) => (r.checkIntervalSec ? `${text(r.checkIntervalSec)} s` : '—')],
  ],
  sslCertificates: [
    ['Type', (r) => mono(r.type)],
    ['Status', (r) => <CertStatusBadge cert={r} />],
    ['Domains', (r) => mono(certDomains(r).join(', '))],
    [
      'Expires',
      (r) => (r.expireTime ? new Date(r.expireTime as string).toLocaleDateString() : '—'),
    ],
  ],
  sslPolicies: [
    ['Profile', (r) => mono(r.profile)],
    ['Minimum TLS', (r) => mono(r.minTlsVersion)],
  ],
}

/**
 * Resources lists the chosen Project's resources of one kind, global and
 * regional. ?location= keeps those in a region (or "global"), ?q= those
 * whose name contains it.
 */
export function Resources() {
  const { coll: param } = useParams()
  const coll = kindInfo(param ?? '')?.coll ?? 'forwardingRules'
  const k = kindInfo(coll)!
  const [view, setView] = useViewState()
  const project = view.project ?? ''
  const [text, setText] = useState(view.q ?? '')
  const [loc, setLoc] = useState(view.location ?? '')
  const query = useQuery(kindQuery(project, coll))
  const listeners = useQuery({ ...listenersQuery(project), enabled: coll === 'forwardingRules' })
  const all = [...(query.data ?? [])].sort(
    (a, b) => a.name.localeCompare(b.name) || scopeOf(a).localeCompare(scopeOf(b)),
  )
  const shown = all.filter((r) => (!loc || scopeOf(r) === loc) && r.name.includes(text))
  const columns = COLUMNS[coll]

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Cloud Load Balancing</h1>
          <p className="text-sm text-muted-foreground">
            {k.title} of Project <span className="font-mono">{project}</span>
          </p>
        </div>
        <Button asChild>
          <Link to={`/lb/create/${coll}`}>
            <Plus aria-hidden />
            Create {k.singular}
          </Link>
        </Button>
      </div>
      <KindTabs coll={coll} />
      <Card>
        <form
          role="search"
          aria-label={`Filter ${k.title.toLowerCase()}`}
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => e.preventDefault()}
        >
          {k.regional && (
            <label htmlFor="lb-location" className="flex flex-col gap-1 text-sm">
              Scope
              <Input
                id="lb-location"
                className="w-48"
                placeholder="global or a region"
                value={loc}
                onChange={(e) => {
                  setLoc(e.target.value.trim())
                  setView({ location: e.target.value.trim() })
                }}
              />
            </label>
          )}
          <label htmlFor="lb-filter" className="flex min-w-48 flex-1 flex-col gap-1 text-sm">
            Filter
            <Input
              id="lb-filter"
              type="search"
              placeholder="Name"
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
          <p className="text-sm text-muted-foreground">
            No {k.title.toLowerCase()} in this Project yet.
          </p>
        )}
        {query.data && all.length > 0 && shown.length === 0 && (
          <p className="text-sm text-muted-foreground">
            No {k.title.toLowerCase()} match the filters.
          </p>
        )}
        {shown.length > 0 && (
          <Table aria-label={k.title}>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Scope</TableHead>
                {columns.map(([h]) => (
                  <TableHead key={h}>{h}</TableHead>
                ))}
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((r) => (
                <TableRow key={r.selfLink ?? r.name} data-testid="resource">
                  <TableCell>
                    <Link
                      className="font-mono text-primary underline-offset-4 hover:underline"
                      to={lbPath(refOf(r, coll, project))}
                    >
                      {r.name}
                    </Link>
                  </TableCell>
                  <TableCell className="font-mono">{scopeOf(r)}</TableCell>
                  {columns.map(([h, cell]) => (
                    <TableCell key={h}>{cell(r, { listeners: listeners.data })}</TableCell>
                  ))}
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  )
}
