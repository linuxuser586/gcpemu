import { useQuery } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { Outlet, useParams } from 'react-router'

import { infoQuery } from '@/api/queries'
import { Card } from '@/components/ui/card'

import { NotEnabled } from '../services/ServicePage'

/**
 * GcsLayout gates the Cloud Storage view (SRS 4.8.3) on the Service being
 * enabled. Buckets are global by name, so only the bucket list and
 * create need a Project.
 */
export function GcsLayout() {
  const { data: info } = useQuery(infoQuery())
  if (info && !info.services.includes('gcs')) {
    return <NotEnabled id="gcs" enabled={info.services} />
  }
  return <Outlet />
}

/** ChooseProject asks for a Project before listing or creating buckets. */
export function ChooseProject({ what }: { what: ReactNode }) {
  return (
    <div className="flex flex-col gap-6">
      <h1 className="text-2xl font-semibold">Cloud Storage</h1>
      <Card role="status">
        <p className="text-sm text-muted-foreground">
          Choose a Project with the Project switcher to {what}.
        </p>
      </Card>
    </div>
  )
}

/** useBucket is the bucket the route names. */
export const useBucket = () => useParams().bucket ?? ''

/** useObjectName is the object the route's splat names. */
export const useObjectName = () => useParams()['*'] ?? ''

const enc = encodeURIComponent

/** bucketPath is the console path of a bucket, or of a page under it. */
export const bucketPath = (bucket: string, sub = '') =>
  `/gcs/b/${enc(bucket)}${sub ? `/${sub}` : ''}`

/** browsePath is a bucket's object browser at prefix. */
export const browsePath = (bucket: string, prefix = '') =>
  bucketPath(bucket) + (prefix ? `?${new URLSearchParams({ prefix })}` : '')

/** objectPath is the console path of an object; its slashes stay as they are. */
export const objectPath = (bucket: string, name: string, sub: 'o' | 'edit' = 'o') =>
  bucketPath(bucket, `${sub}/${name.split('/').map(enc).join('/')}`)
