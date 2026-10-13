import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useId, type ReactNode } from 'react'
import { Outlet, useLocation, useParams } from 'react-router'

import type { Info } from '@/api/admin'
import { infoQuery } from '@/api/queries'
import { Link } from '@/components/Link'
import { Card } from '@/components/ui/card'
import { NativeSelect } from '@/components/ui/native-select'
import { toast } from '@/lib/toast'
import { cn } from '@/lib/utils'
import { useViewState } from '@/lib/viewState'

import { NotEnabled } from '../services/ServicePage'
import { setEnforcement, type EnforcementLevel } from './api'

/**
 * IamLayout is the IAM view (SRS 4.8.3), gated on the Service being
 * enabled: its sections and the IAM enforcement level above every page.
 */
export function IamLayout() {
  const { data: info } = useQuery(infoQuery())
  if (info && !info.services.includes('iam')) {
    return <NotEnabled id="iam" enabled={info.services} />
  }
  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-3 border-b">
        <IamNav />
        {info && <Enforcement level={info.iamMode} />}
      </div>
      <Outlet />
    </div>
  )
}

const SECTIONS = [
  ['/iam', 'Policies'],
  ['/iam/serviceAccounts', 'Service accounts'],
  ['/iam/roles', 'Roles'],
  ['/iam/simulator', 'Policy simulator'],
  ['/iam/audit', 'Audit'],
] as const

/** section is the IAM section a console path belongs to. */
function section(pathname: string): string {
  const rest = pathname.slice(pathname.indexOf('/iam') + 4)
  if (/^\/(serviceAccounts|create-service-account)(\/|$)/.test(rest)) return '/iam/serviceAccounts'
  if (/^\/(roles|customRoles|create-role)(\/|$)/.test(rest)) return '/iam/roles'
  if (/^\/simulator(\/|$)/.test(rest)) return '/iam/simulator'
  if (/^\/audit(\/|$)/.test(rest)) return '/iam/audit'
  return '/iam'
}

function IamNav() {
  const active = section(useLocation().pathname)
  return (
    <nav aria-label="IAM" className="flex flex-wrap gap-1">
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

/** LEVELS describes each IAM enforcement level (FR-IAM-004). */
export const LEVELS: Record<EnforcementLevel, string> = {
  off: 'Off: every call is allowed.',
  audit: 'Audit: calls IAM would deny are allowed and logged.',
  enforce: 'Enforce: calls IAM would deny fail with PERMISSION_DENIED.',
}

/**
 * Enforcement shows the Instance's IAM enforcement level and switches it
 * through the admin API, until the Instance stops.
 */
function Enforcement({ level }: { level: Info['iamMode'] }) {
  const id = useId()
  const qc = useQueryClient()
  const change = useMutation({
    mutationFn: setEnforcement,
    onSuccess: ({ level }) => {
      qc.setQueryData<Info>(infoQuery().queryKey, (old) => old && { ...old, iamMode: level })
      toast.success(`IAM enforcement level is now ${level}. ${LEVELS[level]}`)
    },
  })
  return (
    <div className="mb-2 flex items-center gap-2 text-sm">
      <label htmlFor={id} className="font-medium">
        IAM enforcement level
      </label>
      <NativeSelect
        id={id}
        value={change.isPending ? change.variables : level}
        disabled={change.isPending}
        aria-describedby={`${id}-hint`}
        onChange={(e) => change.mutate(e.target.value as EnforcementLevel)}
      >
        <option value="off">off</option>
        <option value="audit">audit</option>
        <option value="enforce">enforce</option>
      </NativeSelect>
      <span id={`${id}-hint`} className="sr-only">
        {LEVELS[level]} A change lasts until the Instance stops.
      </span>
    </div>
  )
}

/** ChooseProject asks for a Project before anything else. */
export function ChooseProject({ title, what }: { title: string; what: ReactNode }) {
  return (
    <div className="flex flex-col gap-6">
      <h1 className="text-2xl font-semibold">{title}</h1>
      <Card role="status">
        <p className="text-sm text-muted-foreground">
          Choose a Project with the Project switcher to {what}.
        </p>
      </Card>
    </div>
  )
}

/** useProject is the chosen Project, '' if none. */
export function useProject(): string {
  const [view] = useViewState()
  return view.project ?? ''
}

/** useAccount is the service account the route names. */
export function useAccount(): { project: string; email: string } {
  const { email = '' } = useParams()
  return { project: useProject(), email }
}

const enc = encodeURIComponent

/** accountPath is the console path of a service account, or of a page under it. */
export const accountPath = (email: string, sub = '') =>
  `/iam/serviceAccounts/${enc(email)}${sub ? `/${sub}` : ''}`

/** rolePath is the console path of a predefined (roles/X) or custom role. */
export const rolePath = (name: string, sub = '') => {
  const id = name.slice(name.lastIndexOf('/') + 1)
  const base = name.startsWith('roles/') ? `/iam/roles/${enc(id)}` : `/iam/customRoles/${enc(id)}`
  return `${base}${sub ? `/${sub}` : ''}`
}

/** policyPath is the Policies page of a resource ('' kind: the Project). */
export const policyPath = (kind = '', name = '') => {
  const q = new URLSearchParams()
  if (kind && kind !== 'project') q.set('kind', kind)
  if (name) q.set('name', name)
  const s = q.toString()
  return s ? `/iam?${s}` : '/iam'
}

/** simulatorPath is the policy simulator, filled in. */
export const simulatorPath = (principal = '', kind = '', name = '') => {
  const q = new URLSearchParams()
  if (principal) q.set('principal', principal)
  if (kind && kind !== 'project') q.set('kind', kind)
  if (name) q.set('name', name)
  const s = q.toString()
  return s ? `/iam/simulator?${s}` : '/iam/simulator'
}
