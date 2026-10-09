import { useQuery } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { Outlet, useParams } from 'react-router'

import { infoQuery } from '@/api/queries'
import { Card } from '@/components/ui/card'
import { useViewState } from '@/lib/viewState'

import { NotEnabled } from '../services/ServicePage'
import type { SecretRef } from './api'

/**
 * SecretsLayout gates the Secret Manager view (SRS 4.8.3) on the Service
 * being enabled.
 */
export function SecretsLayout() {
  const { data: info } = useQuery(infoQuery())
  if (info && !info.services.includes('secrets')) {
    return <NotEnabled id="secrets" enabled={info.services} />
  }
  return <Outlet />
}

/** ChooseProject asks for a Project before anything else. */
export function ChooseProject({ what }: { what: ReactNode }) {
  return (
    <div className="flex flex-col gap-6">
      <h1 className="text-2xl font-semibold">Secret Manager</h1>
      <Card role="status">
        <p className="text-sm text-muted-foreground">
          Choose a Project with the Project switcher to {what}.
        </p>
      </Card>
    </div>
  )
}

/**
 * useSecretRef is the secret the route names: the Project from
 * ?project=, the location from the path (empty for a global secret).
 */
export function useSecretRef(): SecretRef {
  const [view] = useViewState()
  const { location = '', secret = '' } = useParams()
  return { project: view.project ?? '', location, secret }
}

const enc = encodeURIComponent

/** secretPath is the console path of a secret, or of a page under it. */
export const secretPath = (r: Pick<SecretRef, 'location' | 'secret'>, sub = '') =>
  `/secrets/${r.location ? `locations/${enc(r.location)}/` : ''}secrets/${enc(r.secret)}${sub ? `/${sub}` : ''}`

/** listPath is the secret list of a location ('' for global secrets). */
export const listPath = (location: string) =>
  `/secrets${location ? `?${new URLSearchParams({ location })}` : ''}`

/** locationLabel names a location for people: global secrets have none. */
export const locationLabel = (location: string) => location || 'Global'
