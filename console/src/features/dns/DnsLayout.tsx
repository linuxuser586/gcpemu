import { useQuery } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { Outlet, useLocation, useParams } from 'react-router'

import { infoQuery } from '@/api/queries'
import { Link } from '@/components/Link'
import { Card } from '@/components/ui/card'
import { cn } from '@/lib/utils'
import { useViewState } from '@/lib/viewState'

import { NotEnabled } from '../services/ServicePage'
import type { ManagedZone, ZoneRef } from './api'

/** DnsLayout gates the Cloud DNS view (SRS 4.8.3) on the Service being enabled. */
export function DnsLayout() {
  const { data: info } = useQuery(infoQuery())
  if (info && !info.services.includes('dns')) {
    return <NotEnabled id="dns" enabled={info.services} />
  }
  return <Outlet />
}

/** ChooseProject asks for a Project before anything else. */
export function ChooseProject({ what }: { what: ReactNode }) {
  return (
    <div className="flex flex-col gap-6">
      <h1 className="text-2xl font-semibold">Cloud DNS</h1>
      <Card role="status">
        <p className="text-sm text-muted-foreground">
          Choose a Project with the Project switcher to {what}.
        </p>
      </Card>
    </div>
  )
}

const SECTIONS = [
  ['/dns', 'Zones'],
  ['/dns/query', 'Query'],
] as const

/** DnsNav switches between the zone list and the query box. */
export function DnsNav() {
  const { pathname } = useLocation()
  const active = pathname.endsWith('/dns/query') ? '/dns/query' : '/dns'
  return (
    <nav aria-label="Cloud DNS" className="flex flex-wrap gap-1 border-b">
      {SECTIONS.map(([path, label]) => (
        <Link
          key={path}
          to={path}
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
  )
}

/** useZoneRef is the zone the route names, in the chosen Project. */
export function useZoneRef(): ZoneRef {
  const [view] = useViewState()
  const { zone = '' } = useParams()
  return { project: view.project ?? '', zone }
}

const enc = encodeURIComponent

/** zonePath is the console path of a zone, or of a page under it. */
export const zonePath = (zone: string, sub = '') => `/dns/zones/${enc(zone)}${sub ? `/${sub}` : ''}`

/** rrsetPath is the console path of a record set's page under its zone. */
export const rrsetPath = (zone: string, name: string, type: string, sub = '') =>
  zonePath(zone, `rrsets/${enc(name)}/${enc(type)}${sub ? `/${sub}` : ''}`)

/** NewRecord is a record set another view links to the add form for. */
export interface NewRecord {
  name: string
  type: string
  data: string
}

/**
 * recordSetPath is the add form of zone pre-filled with record, read back
 * by CreateRecordSet from ?name=, ?type= and ?data=.
 */
export const recordSetPath = (zone: Pick<ManagedZone, 'name'>, record: NewRecord) =>
  `${zonePath(zone.name, 'rrsets/create')}?${new URLSearchParams({ ...record })}`

const fqdn = (name: string) => (name.endsWith('.') ? name : `${name}.`).toLowerCase()

/**
 * zoneFor is the public zone that would hold a record named name: the
 * one with the longest DNS name that name is in.
 */
export function zoneFor<Z extends ManagedZone>(name: string, zones: Z[]): Z | undefined {
  const n = fqdn(name)
  return zones
    .filter((z) => z.visibility !== 'private')
    .filter((z) => n === fqdn(z.dnsName) || n.endsWith(`.${fqdn(z.dnsName)}`))
    .sort((a, b) => b.dnsName.length - a.dnsName.length)[0]
}

/** queryPath is the query box asking name (type A unless given). */
export const queryPath = (name: string, type = '', network = '') => {
  const q = new URLSearchParams({ name })
  if (type) q.set('type', type)
  if (network) q.set('network', network)
  return `/dns/query?${q}`
}
