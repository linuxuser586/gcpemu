import { useQuery } from '@tanstack/react-query'
import { Fragment, type ReactNode } from 'react'
import { Outlet, useLocation, useParams } from 'react-router'

import { infoQuery } from '@/api/queries'
import { Link } from '@/components/Link'
import { Card } from '@/components/ui/card'
import { cn } from '@/lib/utils'
import { useViewState } from '@/lib/viewState'

import { lastSeg } from '../lb/api'
import { NotEnabled } from '../services/ServicePage'
import type { NetworkRef, SubnetRef, Subnetwork } from './api'

/**
 * VpcLayout gates the VPC network view (SRS 4.8.3): the compute Service
 * must be enabled, and networks belong to a Project, so one must be
 * chosen. Offered ports are the Instance's, so they need none.
 */
export function VpcLayout() {
  const { data: info } = useQuery(infoQuery())
  const [view] = useViewState()
  const { pathname } = useLocation()
  if (info && !info.services.includes('compute')) {
    return <NotEnabled id="compute" enabled={info.services} />
  }
  if (!view.project && !/\/compute\/offers$/.test(pathname)) {
    return (
      <div className="flex flex-col gap-6">
        <h1 className="text-2xl font-semibold">VPC network</h1>
        <Card role="status">
          <p className="text-sm text-muted-foreground">
            Choose a Project with the Project switcher to see its VPC networks and subnetworks.
          </p>
        </Card>
      </div>
    )
  }
  return <Outlet />
}

const enc = encodeURIComponent

/** networkPage is the console path of a network, or of a page under it. */
export const networkPage = (network: string, sub = '') =>
  `/compute/global/networks/${enc(network)}${sub ? `/${sub}` : ''}`

/** subnetPage is the console path of a subnetwork, or of a page under it. */
export const subnetPage = (r: Pick<SubnetRef, 'region' | 'subnetwork'>, sub = '') =>
  `/compute/regions/${r.region}/subnetworks/${enc(r.subnetwork)}${sub ? `/${sub}` : ''}`

/** useNetworkRef is the network the route names, in the chosen Project. */
export function useNetworkRef(): NetworkRef {
  const { network = '' } = useParams()
  const [view] = useViewState()
  return { project: view.project ?? '', network }
}

/** useSubnetRef is the subnetwork the route names, in the chosen Project. */
export function useSubnetRef(): SubnetRef {
  const { region = '', subnetwork = '' } = useParams()
  const [view] = useViewState()
  return { project: view.project ?? '', region, subnetwork }
}

const linkClass = 'font-mono text-primary underline-offset-4 hover:underline'

/** NetworkLink links a network's selfLink or name to its page. */
export function NetworkLink({ link }: { link?: string }) {
  const name = lastSeg(link)
  if (!name) return <>—</>
  return (
    <Link className={linkClass} to={networkPage(name)}>
      {name}
    </Link>
  )
}

/** SubnetLink links a subnetwork to its page. */
export function SubnetLink({ region, name }: { region: string; name: string }) {
  return (
    <Link className={linkClass} to={subnetPage({ region, subnetwork: name })}>
      {name}
    </Link>
  )
}

/** SubnetList links subnetworks to their pages, comma-separated, or "—" for none. */
export function SubnetList({ subnets }: { subnets: Pick<Subnetwork, 'name' | 'region'>[] }) {
  if (!subnets.length) return <span className="font-mono">—</span>
  return (
    <span>
      {subnets.map((s, i) => (
        <Fragment key={`${s.region}/${s.name}`}>
          {i > 0 && ', '}
          <SubnetLink region={lastSeg(s.region)} name={s.name} />
        </Fragment>
      ))}
    </span>
  )
}

const TABS = [
  ['/compute', 'Networks'],
  ['/compute/subnetworks', 'Subnetworks'],
  ['/compute/offers', 'Offered ports'],
] as const

/** VpcTabs switches between the view's lists. */
export function VpcTabs() {
  const { pathname } = useLocation()
  const active = TABS.find(([to]) => to !== '/compute' && pathname.endsWith(to))?.[0] ?? '/compute'
  return (
    <nav aria-label="VPC network" className="flex flex-wrap gap-1 border-b">
      {TABS.map(([to, label]) => (
        <Link
          key={to}
          to={to}
          aria-current={active === to ? 'page' : undefined}
          className={cn(
            '-mb-px border-b-2 px-3 py-2 text-sm font-medium outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50',
            active === to
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

/** VpcHeader is the heading of the view's lists, with their tabs. */
export function VpcHeader({ action }: { action?: ReactNode }) {
  const [view] = useViewState()
  return (
    <>
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">VPC network</h1>
          {view.project && (
            <p className="text-sm text-muted-foreground">
              Networks and subnetworks of Project <span className="font-mono">{view.project}</span>
            </p>
          )}
        </div>
        {action}
      </div>
      <VpcTabs />
    </>
  )
}
