import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Pencil, RefreshCw, Trash2 } from 'lucide-react'
import { Outlet, useLocation, useOutletContext } from 'react-router'

import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import { toast } from '@/lib/toast'
import { cn } from '@/lib/utils'

import { formatSeconds, time } from '../gcs/format'
import { deleteSecret, rotateSecret, seconds, secretQuery, versionId, type Secret } from './api'
import { replicationLabel } from './Secrets'
import { ChooseProject, listPath, locationLabel, secretPath, useSecretRef } from './SecretsLayout'

const TABS = [
  ['', 'Versions'],
  ['overview', 'Overview'],
] as const

/** isManaged reports whether a secret's versions come from managed rotation. */
export const isManaged = (s: Secret) => s.secretType === 'CLOUD_SQL_DB_CREDENTIALS'

/**
 * SecretPage is one secret: edit, delete and (under managed rotation)
 * rotate above tabs for its versions and its configuration.
 */
export function SecretPage() {
  const ref = useSecretRef()
  const query = useQuery({ ...secretQuery(ref), enabled: !!ref.project })
  const s = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const { pathname } = useLocation()
  const del = useMutation({
    mutationFn: () => deleteSecret(ref),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['secrets'] })
      toast.success(`Deleted secret ${ref.secret}.`)
      navigate(listPath(ref.location))
    },
  })
  const rotate = useMutation({
    mutationFn: () => rotateSecret(ref),
    onSuccess: (v) => {
      void qc.invalidateQueries({ queryKey: ['secrets'] })
      toast.success(`Rotated: version ${versionId(v.name)} holds the new password.`)
    },
  })
  if (!ref.project) return <ChooseProject what="see its secrets" />
  const base = secretPath(ref)
  const rest = pathname.slice(pathname.indexOf(base) + base.length).split('/')[1] ?? ''
  const active = TABS.some(([p]) => p === rest) ? rest : ''

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to={listPath(ref.location)} className="underline-offset-4 hover:underline">
              Secrets
            </Link>{' '}
            / <span className="font-mono">{locationLabel(ref.location)}</span>
          </p>
          <h1 className="text-2xl font-semibold">
            <span className="font-mono">{ref.secret}</span>
          </h1>
        </div>
        {s && (
          <div className="flex flex-wrap gap-2">
            {isManaged(s) && s.rotation?.managedRotationStatus && (
              <Button variant="outline" disabled={rotate.isPending} onClick={() => rotate.mutate()}>
                <RefreshCw aria-hidden />
                Rotate now
              </Button>
            )}
            <Button variant="outline" asChild>
              <Link to={secretPath(ref, 'edit')}>
                <Pencil aria-hidden />
                Edit secret
              </Link>
            </Button>
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Delete secret
                </Button>
              }
              title={`Delete secret ${ref.secret}?`}
              description="Every version of the secret and its payloads are deleted. This cannot be undone."
              confirmLabel="Delete secret"
              onConfirm={() => del.mutateAsync()}
            />
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {s && (
        <>
          <nav aria-label="Secret" className="flex flex-wrap gap-1 border-b">
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
          <Outlet context={s} />
        </>
      )}
    </div>
  )
}

/** useSecret is the secret a SecretPage tab belongs to. */
export const useSecret = () => useOutletContext<Secret>()

const pairs = (m?: Record<string, string>) => {
  const e = Object.entries(m ?? {})
  return e.length > 0 ? <Mono>{e.map(([k, v]) => `${k}=${v}`).join(', ')}</Mono> : undefined
}

/** SecretOverview shows the secret's configuration and the resource as JSON. */
export function SecretOverview() {
  const s = useSecret()
  const ref = useSecretRef()
  const rot = s.rotation
  const managed = rot?.managedRotationStatus
  return (
    <div className="flex flex-col gap-6">
      <Card>
        <CardTitle>Configuration</CardTitle>
        <DetailList
          label="Secret details"
          rows={[
            ['Name', <Mono key="n">{s.name}</Mono>],
            ['Location', <Mono key="l">{locationLabel(ref.location)}</Mono>],
            ['Replication', replicationLabel(s, ref.location)],
            [
              'Type',
              s.secretType && s.secretType !== 'SECRET_TYPE_UNSPECIFIED' ? s.secretType : undefined,
            ],
            ['Labels', pairs(s.labels)],
            ['Annotations', pairs(s.annotations)],
            [
              'Topics',
              s.topics?.length ? <Mono>{s.topics.map((t) => t.name).join(', ')}</Mono> : undefined,
            ],
            ['Expires', time(s.expireTime)],
            ['Next rotation', time(rot?.nextRotationTime)],
            ['Rotation period', formatSeconds(seconds(rot?.rotationPeriod))],
            [
              'Managed rotation',
              managed &&
                `${managed.state ?? ''}${managed.error?.message ? ` — ${managed.error.message}` : ''}`,
            ],
            ['Delayed version destruction', formatSeconds(seconds(s.versionDestroyTtl))],
            ['Version aliases', pairs(s.versionAliases)],
            ['Created', time(s.createTime)],
          ]}
        />
      </Card>
      <Card>
        <CardTitle>Secret resource</CardTitle>
        <JsonView value={s} label="Secret JSON" />
      </Card>
    </div>
  )
}
