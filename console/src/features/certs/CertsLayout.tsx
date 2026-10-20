import { useQuery } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { Outlet, useLocation, useParams } from 'react-router'

import { infoQuery } from '@/api/queries'
import { Link } from '@/components/Link'
import { Card } from '@/components/ui/card'
import { cn } from '@/lib/utils'
import { useViewState } from '@/lib/viewState'

import { NotEnabled } from '../services/ServicePage'
import {
  lastSegment,
  locationOf,
  mapLabel,
  mapRefOf,
  type CertRef,
  type DnsAuthRef,
  type EntryRef,
  type IssuanceRef,
  type MapRef,
  type TrustRef,
} from './api'
import { collOf, TLS_COLLS, TLS_KINDS, tlsLabel, tlsRefOf, type TlsColl, type TlsRef } from './tls'

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

/**
 * ChooseProject asks for a Project before anything else. actions are
 * the ones that need none, such as those on the whole Instance.
 */
export function ChooseProject({ what, actions }: { what: ReactNode; actions?: ReactNode }) {
  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <h1 className="text-2xl font-semibold">Certificate Manager</h1>
        {actions}
      </div>
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
  ['/certs/dnsAuthorizations', 'DNS authorizations'],
  ['/certs/trustConfigs', 'Trust configs'],
  ['/certs/issuanceConfigs', 'Issuance configs'],
  ['/certs/tls', 'TLS policies'],
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

/** useTrustRef is the trust config the route names, in the chosen Project. */
export function useTrustRef(): TrustRef {
  const [view] = useViewState()
  const { location = '', trust = '' } = useParams()
  return { project: view.project ?? '', location, trust }
}

/** trustPath is the console path of a trust config, or of a page under it. */
export const trustPath = (r: Pick<TrustRef, 'location' | 'trust'>, sub = '') =>
  `/certs/locations/${enc(r.location)}/trustConfigs/${enc(r.trust)}${sub ? `/${sub}` : ''}`

/** useDnsAuthRef is the DNS authorization the route names, in the chosen Project. */
export function useDnsAuthRef(): DnsAuthRef {
  const [view] = useViewState()
  const { location = '', auth = '' } = useParams()
  return { project: view.project ?? '', location, auth }
}

/** dnsAuthPath is the console path of a DNS authorization, or of a page under it. */
export const dnsAuthPath = (r: Pick<DnsAuthRef, 'location' | 'auth'>, sub = '') =>
  `/certs/locations/${enc(r.location)}/dnsAuthorizations/${enc(r.auth)}${sub ? `/${sub}` : ''}`

/** useIssuanceRef is the certificate issuance config the route names. */
export function useIssuanceRef(): IssuanceRef {
  const [view] = useViewState()
  const { location = '', config = '' } = useParams()
  return { project: view.project ?? '', location, config }
}

/** issuancePath is the console path of a certificate issuance config. */
export const issuancePath = (r: Pick<IssuanceRef, 'location' | 'config'>) =>
  `/certs/locations/${enc(r.location)}/certificateIssuanceConfigs/${enc(r.config)}`

/**
 * TrustConfigLink links a trust config's name to its page, in the Project
 * that holds it.
 */
export function TrustConfigLink({ name }: { name: string }) {
  const [view] = useViewState()
  const m = /projects\/([^/]+)\/locations\/[^/]+\/trustConfigs\/[^/]+$/.exec(name)
  if (!m) return <span className="font-mono break-all">{name}</span>
  const project = m[1]!
  const to = trustPath({ location: locationOf(name), trust: lastSegment(name) })
  return (
    <Link
      className="font-mono text-primary underline-offset-4 hover:underline"
      to={project === view.project ? to : `${to}?project=${enc(project)}`}
      title={`trust config ${lastSegment(name)}`}
    >
      {project === view.project ? lastSegment(name) : `${project}/${lastSegment(name)}`}
    </Link>
  )
}

/** useTlsRef is the resource of kind coll that the route names. */
export function useTlsRef(coll: TlsColl): TlsRef {
  const [view] = useViewState()
  const { location = '', id = '' } = useParams()
  return { project: view.project ?? '', location, coll, id }
}

/** tlsListPath is the console path of one kind's list. */
export const tlsListPath = (coll: TlsColl) => `/certs/tls/${coll}`

/** tlsPath is the console path of a TLS policies resource, or of a page under it. */
export const tlsPath = (r: Pick<TlsRef, 'location' | 'coll' | 'id'>, sub = '') =>
  `/certs/locations/${enc(r.location)}/${r.coll}/${enc(r.id)}${sub ? `/${sub}` : ''}`

/** TlsNav switches between the TLS policy kinds. */
export function TlsNav() {
  const { coll } = useParams()
  const active = collOf(coll)
  return (
    <nav aria-label="TLS policy types" className="flex flex-wrap gap-1">
      {TLS_COLLS.map((c) => (
        <Link
          key={c}
          to={tlsListPath(c)}
          aria-current={active === c ? 'page' : undefined}
          className={cn(
            'rounded-md px-3 py-1 text-sm outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50',
            active === c
              ? 'bg-muted font-medium text-foreground'
              : 'text-muted-foreground hover:text-foreground',
          )}
        >
          {TLS_KINDS[c].title}
        </Link>
      ))}
    </nav>
  )
}

/**
 * TlsLink links the name of a server TLS policy, backend authentication
 * config or client TLS policy, in any of its forms, to its page in
 * the Project that holds it.
 */
export function TlsLink({ name }: { name: string }) {
  const [view] = useViewState()
  const r = tlsRefOf(name)
  if (!r) return <span className="font-mono break-all">{name}</span>
  const to = tlsPath(r)
  return (
    <Link
      className="font-mono text-primary underline-offset-4 hover:underline"
      to={r.project === view.project ? to : `${to}?project=${enc(r.project)}`}
      title={`${TLS_KINDS[r.coll].singular} ${r.id}`}
    >
      {tlsLabel(name, view.project ?? '')}
    </Link>
  )
}
