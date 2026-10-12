import { useQuery } from '@tanstack/react-query'
import { Outlet, useLocation, useParams } from 'react-router'

import { infoQuery } from '@/api/queries'
import { Link } from '@/components/Link'
import { Card } from '@/components/ui/card'
import { cn } from '@/lib/utils'
import { useViewState } from '@/lib/viewState'

import { NotEnabled } from '../services/ServicePage'
import { gatewayOf, routerRefOf, type RouterRef } from './api'

/**
 * NatLayout gates the Cloud NAT view (SRS 4.8.3): the Service must be
 * enabled, and routers belong to a Project, so one must be chosen.
 */
export function NatLayout() {
  const { data: info } = useQuery(infoQuery())
  const [view] = useViewState()
  if (info && !info.services.includes('nat')) {
    return <NotEnabled id="nat" enabled={info.services} />
  }
  if (!view.project) {
    return (
      <div className="flex flex-col gap-6">
        <h1 className="text-2xl font-semibold">Cloud NAT</h1>
        <Card role="status">
          <p className="text-sm text-muted-foreground">
            Choose a Project with the Project switcher to see its Cloud Routers and NAT gateways.
          </p>
        </Card>
      </div>
    )
  }
  return <Outlet />
}

/** routerPage is the console path of a router, or of a page under it. */
export const routerPage = (r: Pick<RouterRef, 'region' | 'router'>, sub = '') =>
  `/nat/regions/${r.region}/routers/${encodeURIComponent(r.router)}${sub ? `/${sub}` : ''}`

/** natPage is the console path of a NAT, or of a page under it. */
export const natPage = (r: Pick<RouterRef, 'region' | 'router'>, nat: string, sub = '') =>
  routerPage(r, `nats/${encodeURIComponent(nat)}${sub ? `/${sub}` : ''}`)

/** useRouterRef is the router the route names, in the chosen Project. */
export function useRouterRef(): RouterRef {
  const { region = '', router = '' } = useParams()
  const [view] = useViewState()
  return { project: view.project ?? '', region, router }
}

/**
 * GatewayLink links a NAT gateway path (".../routers/R/nats/N") or a
 * router to its page; other resources, e.g. a network, are shown as text.
 */
export function GatewayLink({ path }: { path?: string }) {
  if (!path) return <>—</>
  const g = gatewayOf(path)
  const r = g ?? routerRefOf(path)
  if (!r)
    return <span className="font-mono break-all">{path.replace(/^projects\/[^/]+\//, '')}</span>
  return (
    <Link
      className="font-mono text-primary underline-offset-4 hover:underline"
      to={g ? natPage(g, g.nat) : routerPage(r)}
      title={g ? `NAT ${g.nat} of router ${g.router} (${g.region})` : `Router ${r.router}`}
    >
      {g ? `${g.router}/${g.nat}` : r.router}
    </Link>
  )
}

const TABS = [
  ['/nat', 'Routers'],
  ['/nat/translations', 'Translation log'],
] as const

/** NatTabs switches between the routers and the translation log. */
export function NatTabs() {
  const { pathname } = useLocation()
  const active = /\/nat\/translations$/.test(pathname) ? '/nat/translations' : '/nat'
  return (
    <nav aria-label="Cloud NAT" className="flex flex-wrap gap-1 border-b">
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
