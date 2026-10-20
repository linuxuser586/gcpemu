import { useQuery } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { Outlet, useLocation, useParams } from 'react-router'

import { infoQuery } from '@/api/queries'
import { Link } from '@/components/Link'
import { Card } from '@/components/ui/card'
import { cn } from '@/lib/utils'
import { useViewState } from '@/lib/viewState'

import { NotEnabled } from '../services/ServicePage'
import type { CertRef } from './api'

/**
 * CertsLayout gates the Certificate Manager view (SRS 4.8.3) on the
 * Service being enabled.
 */
export function CertsLayout() {
  const { data: info } = useQuery(infoQuery())
  if (info && !info.services.includes('certs')) {
    return <NotEnabled id="certs" enabled={info.services} />
  }
  return <Outlet />
}

/** ChooseProject asks for a Project before anything else. */
export function ChooseProject({ what }: { what: ReactNode }) {
  return (
    <div className="flex flex-col gap-6">
      <h1 className="text-2xl font-semibold">Certificate Manager</h1>
      <Card role="status">
        <p className="text-sm text-muted-foreground">
          Choose a Project with the Project switcher to {what}.
        </p>
      </Card>
    </div>
  )
}

// Each resource type the view covers has a section; later ones add theirs.
const SECTIONS = [['/certs', 'Certificates']] as const

/** CertsNav switches between the view's resource types. */
export function CertsNav() {
  const { pathname } = useLocation()
  const active =
    SECTIONS.filter(([p]) => pathname === p || pathname.startsWith(`${p}/`)).at(-1)?.[0] ?? '/certs'
  return (
    <nav aria-label="Certificate Manager" className="flex flex-wrap gap-1 border-b">
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

/** useCertRef is the certificate the route names, in the chosen Project. */
export function useCertRef(): CertRef {
  const [view] = useViewState()
  const { location = '', cert = '' } = useParams()
  return { project: view.project ?? '', location, cert }
}

const enc = encodeURIComponent

/** certPath is the console path of a certificate, or of a page under it. */
export const certPath = (r: Pick<CertRef, 'location' | 'cert'>, sub = '') =>
  `/certs/locations/${enc(r.location)}/certificates/${enc(r.cert)}${sub ? `/${sub}` : ''}`
