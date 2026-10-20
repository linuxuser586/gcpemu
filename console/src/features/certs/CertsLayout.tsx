import { useQuery } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { Outlet, useLocation, useParams } from 'react-router'

import { infoQuery } from '@/api/queries'
import { Link } from '@/components/Link'
import { Card } from '@/components/ui/card'
import { cn } from '@/lib/utils'
import { useViewState } from '@/lib/viewState'

import { NotEnabled } from '../services/ServicePage'
import { mapLabel, mapRefOf, type CertRef, type EntryRef, type MapRef } from './api'

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
const SECTIONS = [
  ['/certs', 'Certificates'],
  ['/certs/maps', 'Certificate maps'],
] as const

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

/** useMapRef is the certificate map the route names, in the chosen Project. */
export function useMapRef(): MapRef {
  const [view] = useViewState()
  const { map = '' } = useParams()
  return { project: view.project ?? '', map }
}

/** useEntryRef is the certificate map entry the route names. */
export function useEntryRef(): EntryRef {
  const { entry = '' } = useParams()
  return { ...useMapRef(), entry }
}

/** mapPath is the console path of a certificate map, or of a page under it. */
export const mapPath = (r: Pick<MapRef, 'map'>, sub = '') =>
  `/certs/locations/global/certificateMaps/${enc(r.map)}${sub ? `/${sub}` : ''}`

/** entryPath is the console path of a page of a certificate map entry. */
export const entryPath = (r: Pick<EntryRef, 'map' | 'entry'>, sub: string) =>
  mapPath(r, `certificateMapEntries/${enc(r.entry)}/${sub}`)

/**
 * MapLink links a certificate map's name, in any of its forms, to its
 * page in the Project that holds it.
 */
export function MapLink({ name }: { name: string }) {
  const [view] = useViewState()
  const r = mapRefOf(name)
  if (!r) return <span className="font-mono break-all">{name}</span>
  return (
    <Link
      className="font-mono text-primary underline-offset-4 hover:underline"
      to={r.project === view.project ? mapPath(r) : `${mapPath(r)}?project=${enc(r.project)}`}
      title={`certificate map ${r.map}`}
    >
      {mapLabel(name, view.project ?? '')}
    </Link>
  )
}
