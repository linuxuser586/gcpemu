import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Pencil, Trash2 } from 'lucide-react'
import { Outlet, useLocation, useOutletContext } from 'react-router'

import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import { toast } from '@/lib/toast'
import { cn } from '@/lib/utils'

import { bucketQuery, deleteBucket, type Bucket } from './api'
import { formatSeconds, time } from './format'
import { bucketPath, useBucket } from './GcsLayout'

const TABS = [
  ['', 'Objects'],
  ['configuration', 'Configuration'],
  ['notifications', 'Notifications'],
] as const

/** tabOf is the tab a path under a bucket belongs to: objects by default. */
function tabOf(pathname: string, base: string): string {
  const rest = pathname.slice(base.length).split('/')[1] ?? ''
  return TABS.some(([p]) => p === rest) ? rest : ''
}

/**
 * BucketPage is one bucket: delete and edit above tabs for its objects,
 * its configuration and its notifications.
 */
export function BucketPage() {
  const name = useBucket()
  const query = useQuery(bucketQuery(name))
  const b = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const { pathname } = useLocation()
  const del = useMutation({
    mutationFn: () => deleteBucket(name),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['gcs'] })
      toast.success(`Deleted bucket ${name}.`)
      navigate('/gcs')
    },
  })
  const base = bucketPath(name)
  const active = tabOf(pathname, base)

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to="/gcs" className="underline-offset-4 hover:underline">
              Buckets
            </Link>
            {b?.location && (
              <>
                {' '}
                / <span className="font-mono">{b.location}</span>
              </>
            )}
          </p>
          <h1 className="text-2xl font-semibold">
            <span className="font-mono">{name}</span>
          </h1>
        </div>
        {b && (
          <div className="flex flex-wrap gap-2">
            <Button variant="outline" asChild>
              <Link to={bucketPath(name, 'edit')}>
                <Pencil aria-hidden />
                Edit bucket
              </Link>
            </Button>
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Delete bucket
                </Button>
              }
              title={`Delete bucket ${name}?`}
              description="Only an empty bucket can be deleted. This cannot be undone."
              confirmLabel="Delete bucket"
              onConfirm={() => del.mutateAsync()}
            />
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {b && (
        <>
          <nav aria-label="Bucket" className="flex flex-wrap gap-1 border-b">
            {TABS.map(([path, label]) => (
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
                {label}
              </Link>
            ))}
          </nav>
          <Outlet context={b} />
        </>
      )}
    </div>
  )
}

/** useBucketResource is the bucket a BucketPage tab belongs to. */
export const useBucketResource = () => useOutletContext<Bucket>()

/** BucketOverview shows the bucket's configuration and the resource as JSON. */
export function BucketOverview() {
  const b = useBucketResource()
  const labels = Object.entries(b.labels ?? {})
  const retention = b.retentionPolicy
  return (
    <div className="flex flex-col gap-6">
      <Card>
        <CardTitle>Configuration</CardTitle>
        <DetailList
          label="Bucket details"
          rows={[
            [
              'Location',
              b.location && (
                <Mono>
                  {b.location}
                  {b.locationType ? ` (${b.locationType})` : ''}
                </Mono>
              ),
            ],
            ['Default storage class', b.storageClass],
            ['Object versioning', b.versioning?.enabled ? 'On' : 'Off'],
            [
              'Uniform bucket-level access',
              b.iamConfiguration?.uniformBucketLevelAccess?.enabled ? 'On' : 'Off',
            ],
            ['Public access prevention', b.iamConfiguration?.publicAccessPrevention],
            [
              'Retention period',
              retention?.retentionPeriod &&
                `${formatSeconds(retention.retentionPeriod)}${retention.isLocked ? ' (locked)' : ''}`,
            ],
            ['Default event-based hold', b.defaultEventBasedHold ? 'On' : undefined],
            ['Soft delete retention', formatSeconds(b.softDeletePolicy?.retentionDurationSeconds)],
            [
              'Website main page',
              b.website?.mainPageSuffix && <Mono>{b.website.mainPageSuffix}</Mono>,
            ],
            [
              'Website not-found page',
              b.website?.notFoundPage && <Mono>{b.website.notFoundPage}</Mono>,
            ],
            ['CORS rules', b.cors?.length ? String(b.cors.length) : undefined],
            [
              'Lifecycle rules',
              b.lifecycle?.rule?.length ? String(b.lifecycle.rule.length) : undefined,
            ],
            [
              'Labels',
              labels.length > 0 && <Mono>{labels.map(([k, v]) => `${k}=${v}`).join(', ')}</Mono>,
            ],
            ['Created', time(b.timeCreated)],
            ['Updated', time(b.updated)],
            ['Metageneration', b.metageneration],
          ]}
        />
      </Card>
      <Card>
        <CardTitle>Bucket resource</CardTitle>
        <JsonView value={b} label="Bucket JSON" />
      </Card>
    </div>
  )
}
