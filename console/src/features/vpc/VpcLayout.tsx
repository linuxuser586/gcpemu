import { useQuery } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { Outlet, useLocation, useParams } from 'react-router'

import { infoQuery } from '@/api/queries'
import { Link } from '@/components/Link'
import { Card } from '@/components/ui/card'
import { cn } from '@/lib/utils'
import { useViewState } from '@/lib/viewState'

import { NotEnabled } from '../services/ServicePage'
import type { NetworkRef, SubnetRef } from './api'

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
            Choose a Project with the Project switcher to see its VPC networks, subnetworks,
            firewall rules and routes.
          </p>
        </Card>
      </div>
    )
  }
  return <Outlet />
}

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

const TABS = [
  ['/compute', 'Networks'],
  ['/compute/subnetworks', 'Subnetworks'],
  ['/compute/firewalls', 'Firewall rules'],
  ['/compute/routes', 'Routes'],
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
              Networks, subnetworks, firewall rules and routes of Project{' '}
              <span className="font-mono">{view.project}</span>
            </p>
          )}
        </div>
        {action}
      </div>
      <VpcTabs />
    </>
  )
}
