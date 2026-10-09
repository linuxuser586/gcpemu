import { useQuery } from '@tanstack/react-query'
import { Outlet, useParams } from 'react-router'

import { infoQuery } from '@/api/queries'
import { Link, NavLink } from '@/components/Link'
import { Card } from '@/components/ui/card'
import { useViewState } from '@/lib/viewState'
import { cn } from '@/lib/utils'

import { NotEnabled } from '../services/ServicePage'
import { KINDS, kindInfo, parseRef, type Coll, type Ref } from './api'

/**
 * LbLayout gates the Load Balancer view (SRS 4.8.3): the Service must be
 * enabled, and its resources belong to a Project, so one must be chosen.
 */
export function LbLayout() {
  const { data: info } = useQuery(infoQuery())
  const [view] = useViewState()
  if (info && !info.services.includes('lb')) {
    return <NotEnabled id="lb" enabled={info.services} />
  }
  if (!view.project) {
    return (
      <div className="flex flex-col gap-6">
        <h1 className="text-2xl font-semibold">Cloud Load Balancing</h1>
        <Card role="status">
          <p className="text-sm text-muted-foreground">
            Choose a Project with the Project switcher to see its load balancers.
          </p>
        </Card>
      </div>
    )
  }
  return <Outlet />
}

/** listPath is the console path of a kind's list. */
export const listPath = (coll: Coll) => (coll === 'forwardingRules' ? '/lb' : `/lb/${coll}`)

/** lbPath is the console path of a resource, or of a page under it. */
export const lbPath = (r: Ref, sub = '') =>
  `/lb/${r.region ? `regions/${r.region}` : 'global'}/${r.coll}/${encodeURIComponent(r.name)}${sub ? `/${sub}` : ''}`

/** useLbRef is the resource the route names, in the chosen Project. */
export function useLbRef(): Ref {
  const { region = '', coll = '', name = '' } = useParams()
  const [view] = useViewState()
  return { project: view.project ?? '', region, coll: coll as Coll, name }
}

/** RefLink links a reference to its page when it is a load-balancing resource. */
export function RefLink({ link }: { link?: string }) {
  if (!link) return <>—</>
  const r = parseRef(link)
  if (!r) return <span className="font-mono break-all">{link}</span>
  return (
    <Link
      className="font-mono text-primary underline-offset-4 hover:underline"
      to={lbPath(r)}
      title={`${kindInfo(r.coll)?.singular ?? r.coll} ${r.name}`}
    >
      {r.name}
    </Link>
  )
}

/** KindTabs switches between the kinds' lists. */
export function KindTabs({ coll }: { coll: Coll }) {
  return (
    <nav aria-label="Resource types" className="flex flex-wrap gap-1 border-b">
      {KINDS.map((k) => (
        <NavLink
          key={k.coll}
          to={listPath(k.coll)}
          end
          className={() =>
            cn(
              '-mb-px border-b-2 px-3 py-2 text-sm font-medium outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50',
              k.coll === coll
                ? 'border-primary text-foreground'
                : 'border-transparent text-muted-foreground hover:text-foreground',
            )
          }
          aria-current={k.coll === coll ? 'page' : undefined}
        >
          {k.title}
        </NavLink>
      ))}
    </nav>
  )
}
