import { useQuery } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { Outlet, useLocation } from 'react-router'

import { infoQuery } from '@/api/queries'
import { Link } from '@/components/Link'
import { Card } from '@/components/ui/card'
import { cn } from '@/lib/utils'
import { useViewState } from '@/lib/viewState'

import { NotEnabled } from '../services/ServicePage'

/**
 * PubSubLayout gates the Pub/Sub view (SRS 4.8.3) on the Service being
 * enabled.
 */
export function PubSubLayout() {
  const { data: info } = useQuery(infoQuery())
  if (info && !info.services.includes('pubsub')) {
    return <NotEnabled id="pubsub" enabled={info.services} />
  }
  return <Outlet />
}

/** useProject is the Project of ?project=, or '' before one is chosen. */
export function useProject() {
  const [view] = useViewState()
  return view.project ?? ''
}

/** ChooseProject asks for a Project before anything else. */
export function ChooseProject({ what }: { what: ReactNode }) {
  return (
    <div className="flex flex-col gap-6">
      <h1 className="text-2xl font-semibold">Pub/Sub</h1>
      <Card role="status">
        <p className="text-sm text-muted-foreground">
          Choose a Project with the Project switcher to {what}.
        </p>
      </Card>
    </div>
  )
}

const enc = encodeURIComponent

/** topicPath is the console path of a topic, or of a page under it. */
export const topicPath = (topic: string, sub = '') =>
  `/pubsub/topics/${enc(topic)}${sub ? `/${sub}` : ''}`

/** subscriptionPath is the console path of a subscription, or of a page under it. */
export const subscriptionPath = (id: string, sub = '') =>
  `/pubsub/subscriptions/${enc(id)}${sub ? `/${sub}` : ''}`

/** Tabs is a row of links to the pages of one resource or list. */
export function Tabs({
  label,
  base,
  tabs,
}: {
  label: string
  base: string
  tabs: readonly (readonly [path: string, label: string])[]
}) {
  const { pathname } = useLocation()
  const rest = pathname.slice(pathname.indexOf(base) + base.length).split('/')[1] ?? ''
  const active = tabs.some(([p]) => p === rest) ? rest : ''
  return (
    <nav aria-label={label} className="flex flex-wrap gap-1 border-b">
      {tabs.map(([path, text]) => (
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
          {text}
        </Link>
      ))}
    </nav>
  )
}

/** ListHeader is the heading and tabs above the topic and subscription lists. */
export function ListHeader({ action }: { action: ReactNode }) {
  const project = useProject()
  return (
    <>
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Pub/Sub</h1>
          <p className="text-sm text-muted-foreground">
            Topics and subscriptions of Project <span className="font-mono">{project}</span>
          </p>
        </div>
        {action}
      </div>
      <Tabs
        label="Pub/Sub"
        base="/pubsub"
        tabs={[
          ['', 'Topics'],
          ['subscriptions', 'Subscriptions'],
        ]}
      />
    </>
  )
}
