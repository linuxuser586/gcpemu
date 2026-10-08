import { useQuery } from '@tanstack/react-query'
import { PanelLeft } from 'lucide-react'
import { Outlet } from 'react-router'

import { infoQuery } from '@/api/queries'
import { Link } from '@/components/Link'
import { Button } from '@/components/ui/button'
import { useApplyTheme } from '@/lib/theme'
import { usePrefs } from '@/stores/prefs'

import { ProjectSwitcher } from './ProjectSwitcher'
import { Sidebar } from './Sidebar'
import { ThemeToggle } from './ThemeToggle'
import { Toaster } from './Toaster'

export function AppShell() {
  useApplyTheme()
  const { data: info } = useQuery(infoQuery())
  const collapsed = usePrefs((s) => s.sidebarCollapsed)
  const toggleSidebar = usePrefs((s) => s.toggleSidebar)
  return (
    <div className="flex min-h-screen flex-col">
      <a
        href="#main"
        className="sr-only focus:not-sr-only focus:absolute focus:top-2 focus:left-2 focus:z-50 focus:rounded-md focus:bg-background focus:p-2"
      >
        Skip to content
      </a>
      <header className="flex h-14 items-center gap-3 border-b px-3">
        <Button
          variant="ghost"
          size="icon"
          aria-label={collapsed ? 'Show sidebar' : 'Hide sidebar'}
          aria-expanded={!collapsed}
          onClick={toggleSidebar}
        >
          <PanelLeft />
        </Button>
        <Link to="/" className="font-semibold">
          gcpemu
        </Link>
        {info && (
          <span className="text-sm text-muted-foreground" title={`Instance ID ${info.id}`}>
            Instance <span className="font-mono">{info.instance}</span>
          </span>
        )}
        <div className="ml-4">
          <ProjectSwitcher />
        </div>
        <div className="ml-auto">
          <ThemeToggle />
        </div>
      </header>
      <div className="flex flex-1">
        <Sidebar />
        <main id="main" className="min-w-0 flex-1 p-6">
          <Outlet />
        </main>
      </div>
      <Toaster />
    </div>
  )
}
