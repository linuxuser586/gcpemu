import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Pencil, Search, Trash2 } from 'lucide-react'
import { Outlet, useLocation, useOutletContext } from 'react-router'

import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import { toast } from '@/lib/toast'
import { cn } from '@/lib/utils'

import { time } from '../gcs/format'
import {
  applyChange,
  deleteZone,
  isApexRecord,
  networkLabel,
  rrsetsQuery,
  zoneQuery,
  zoneNetworks,
  type ManagedZone,
} from './api'
import { ChooseProject, queryPath, useZoneRef, zonePath } from './DnsLayout'
import { VisibilityBadge } from './Zones'

const TABS = [
  ['', 'Record sets'],
  ['changes', 'Changes'],
  ['overview', 'Overview'],
] as const

/**
 * ZonePage is one managed zone: edit, delete and query above tabs for its
 * record sets, its change history and its configuration.
 */
export function ZonePage() {
  const ref = useZoneRef()
  const query = useQuery({ ...zoneQuery(ref), enabled: !!ref.project })
  const z = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const { pathname } = useLocation()
  const del = useMutation({
    // A zone must be empty: its record sets other than the apex SOA and
    // NS go first, in one change.
    mutationFn: async () => {
      const rrsets = await qc.fetchQuery(rrsetsQuery(ref))
      const deletions = rrsets.filter((rs) => !isApexRecord(rs, z!))
      if (deletions.length > 0) await applyChange(ref, { deletions })
      await deleteZone(ref)
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['dns'] })
      toast.success(`Deleted zone ${ref.zone}.`)
      navigate('/dns')
    },
  })
  if (!ref.project) return <ChooseProject what="see its managed zones" />
  const base = zonePath(ref.zone)
  const rest = pathname.slice(pathname.indexOf(base) + base.length).split('/')[1] ?? ''
  const active = TABS.some(([p]) => p === rest) ? rest : ''

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to="/dns" className="underline-offset-4 hover:underline">
              Zones
            </Link>
          </p>
          <h1 className="flex flex-wrap items-center gap-2 text-2xl font-semibold">
            <span className="font-mono">{ref.zone}</span>
            {z && <VisibilityBadge z={z} />}
          </h1>
          {z && <p className="font-mono text-sm text-muted-foreground">{z.dnsName}</p>}
        </div>
        {z && (
          <div className="flex flex-wrap gap-2">
            <Button variant="outline" asChild>
              <Link to={queryPath(z.dnsName, 'SOA', zoneNetworks(z)[0])}>
                <Search aria-hidden />
                Query
              </Link>
            </Button>
            <Button variant="outline" asChild>
              <Link to={`${base}/edit`}>
                <Pencil aria-hidden />
                Edit zone
              </Link>
            </Button>
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Delete zone
                </Button>
              }
              title={`Delete zone ${ref.zone}?`}
              description="Its record sets are deleted first, in one change, then the zone. This cannot be undone."
              confirmLabel="Delete zone"
              onConfirm={() => del.mutateAsync()}
            />
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {z && (
        <>
          <nav aria-label="Zone" className="flex flex-wrap gap-1 border-b">
            {TABS.map(([path, label]) => (
              <Link
                key={path}
                to={path ? `${base}/${path}` : base}
                aria-current={active === path ? 'page' : undefined}
                className={cn(
                  '-mb-px border-b-2 px-3 py-2 text-sm font-medium outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50',
                  active === path
                    ? 'border-primary text-foreground'
                    : 'border-transparent text-muted-foreground hover:text-foreground',
                )}
              >
                {label}
              </Link>
            ))}
          </nav>
          <Outlet context={z} />
        </>
      )}
    </div>
  )
}

/** useZone is the zone a ZonePage tab belongs to. */
export const useZone = () => useOutletContext<ManagedZone>()

/** ZoneOverview shows the zone's configuration, network bindings and JSON. */
export function ZoneOverview() {
  const z = useZone()
  const { project } = useZoneRef()
  const nets = z.privateVisibilityConfig?.networks ?? []
  const labels = Object.entries(z.labels ?? {})
  return (
    <div className="flex flex-col gap-6">
      <Card>
        <CardTitle>Configuration</CardTitle>
        <DetailList
          label="Zone details"
          rows={[
            ['Zone name', <Mono key="n">{z.name}</Mono>],
            ['DNS name', <Mono key="d">{z.dnsName}</Mono>],
            ['ID', z.id && <Mono>{z.id}</Mono>],
            ['Visibility', z.visibility === 'private' ? 'Private' : 'Public'],
            ['Description', z.description],
            [
              'Name servers',
              z.nameServers?.length ? <Mono>{z.nameServers.join(', ')}</Mono> : undefined,
            ],
            [
              'Labels',
              labels.length > 0 ? (
                <Mono>{labels.map(([k, v]) => `${k}=${v}`).join(', ')}</Mono>
              ) : undefined,
            ],
            ['Created', time(z.creationTime)],
          ]}
        />
      </Card>
      {z.visibility === 'private' && (
        <Card>
          <CardTitle>Network bindings</CardTitle>
          {nets.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              Bound to no network: no query sees this zone. Edit the zone to bind one.
            </p>
          ) : (
            <ul aria-label="Network bindings" className="flex flex-col gap-1 text-sm">
              {nets.map((n) => (
                <li key={n.networkUrl} className="flex flex-wrap items-baseline gap-2">
                  <span className="font-medium">{networkLabel(n.networkUrl, project)}</span>
                  <span className="font-mono text-xs break-all text-muted-foreground">
                    {n.networkUrl}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </Card>
      )}
      <Card>
        <CardTitle>Managed zone resource</CardTitle>
        <JsonView value={z} label="Zone JSON" />
      </Card>
    </div>
  )
}
