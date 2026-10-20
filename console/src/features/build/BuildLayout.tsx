import { useQuery } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { Outlet, useLocation } from 'react-router'

import { infoQuery } from '@/api/queries'
import { Link } from '@/components/Link'
import { Badge } from '@/components/ui/badge'
import { Card } from '@/components/ui/card'
import { cn } from '@/lib/utils'

import { NotEnabled } from '../services/ServicePage'
import { GLOBAL, STATUS_BADGE, type BuildRef, type BuildStatus } from './api'

/**
 * BuildLayout gates the Cloud Build view (SRS 4.8.3) on the Service being
 * enabled.
 */
export function BuildLayout() {
  const { data: info } = useQuery(infoQuery())
  if (info && !info.services.includes('build')) {
    return <NotEnabled id="build" enabled={info.services} />
  }
  return <Outlet />
}

/** ChooseProject asks for a Project before anything else. */
export function ChooseProject({ what }: { what: ReactNode }) {
  return (
    <div className="flex flex-col gap-6">
      <h1 className="text-2xl font-semibold">Cloud Build</h1>
      <Card role="status">
        <p className="text-sm text-muted-foreground">
          Choose a Project with the Project switcher to {what}.
        </p>
      </Card>
    </div>
  )
}

/** StatusBadge shows a Build's or step's status. */
export function StatusBadge({ status = 'STATUS_UNKNOWN' }: { status?: BuildStatus }) {
  return <Badge variant={STATUS_BADGE[status]}>{status}</Badge>
}

const enc = encodeURIComponent

/**
 * buildPath is the console path of a Build: the path its logUrl points to
 * under /console, with the Project in ?project= as every view has it.
 */
export const buildPath = (r: BuildRef) =>
  `/build/projects/${enc(r.project)}/locations/${enc(r.location)}/builds/${enc(r.id)}?${new URLSearchParams({ project: r.project })}`

/** listPath is the Build list of a location. */
export const listPath = (location: string) =>
  `/build${location && location !== GLOBAL ? `?${new URLSearchParams({ location })}` : ''}`

const TABS = [
  ['/build', 'Builds'],
  ['/build/triggers', 'Build triggers'],
  ['/build/daemon', 'Build daemon'],
] as const

/** BuildHeader is the view's title and its tabs. */
export function BuildHeader({ children }: { children?: ReactNode }) {
  const { pathname } = useLocation()
  return (
    <>
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Cloud Build</h1>
          {children}
        </div>
      </div>
      <nav aria-label="Cloud Build" className="flex flex-wrap gap-1 border-b">
        {TABS.map(([path, label]) => {
          const active = pathname === path
          return (
            <Link
              key={path}
              to={path}
              aria-current={active ? 'page' : undefined}
              className={cn(
                '-mb-px border-b-2 px-3 py-2 text-sm font-medium outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50',
                active
                  ? 'border-primary text-foreground'
                  : 'border-transparent text-muted-foreground hover:text-foreground',
              )}
            >
              {label}
            </Link>
          )
        })}
      </nav>
    </>
  )
}
