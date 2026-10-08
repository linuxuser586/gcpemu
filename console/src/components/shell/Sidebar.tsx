import { useQuery } from '@tanstack/react-query'
import { LayoutDashboard } from 'lucide-react'

import { infoQuery } from '@/api/queries'
import { NavLink } from '@/components/Link'
import { serviceName, SERVICES } from '@/lib/services'
import { cn } from '@/lib/utils'
import { usePrefs } from '@/stores/prefs'

const item = ({ isActive }: { isActive: boolean }) =>
  cn(
    'flex items-center gap-2 rounded-md px-2 py-1.5 text-sm hover:bg-accent focus-visible:ring-[3px] focus-visible:ring-ring/50 outline-none',
    isActive && 'bg-accent font-medium',
  )

/** Sidebar lists the dashboard and the Services enabled on this Instance. */
export function Sidebar() {
  const collapsed = usePrefs((s) => s.sidebarCollapsed)
  const { data: info } = useQuery(infoQuery())
  const enabled = new Set(info?.services ?? [])
  if (collapsed) return null
  return (
    <nav aria-label="Services" className="w-56 shrink-0 border-r bg-sidebar p-3">
      <ul className="flex flex-col gap-0.5">
        <li>
          <NavLink to="/" end className={item}>
            <LayoutDashboard className="size-4" aria-hidden />
            Dashboard
          </NavLink>
        </li>
      </ul>
      <h2 className="mt-4 mb-1 px-2 text-xs font-medium text-muted-foreground">Services</h2>
      <ul className="flex flex-col gap-0.5">
        {SERVICES.filter((s) => enabled.has(s.id)).map((s) => (
          <li key={s.id}>
            <NavLink to={`/${s.id}`} className={item}>
              {serviceName(s.id)}
            </NavLink>
          </li>
        ))}
      </ul>
    </nav>
  )
}
