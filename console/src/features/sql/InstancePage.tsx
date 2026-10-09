import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Copy, Pencil, Play, RotateCw, Square, Trash2 } from 'lucide-react'
import { useState } from 'react'
import { Outlet, useOutletContext } from 'react-router'

import { origin } from '@/api/fetch'
import { envQuery } from '@/api/queries'
import { Link, NavLink, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import { toast } from '@/lib/toast'
import { cn } from '@/lib/utils'

import {
  deleteInstance,
  instanceQuery,
  ipOf,
  isRunning,
  isStopped,
  restartInstance,
  setActivation,
  type DatabaseInstance,
} from './api'
import { InstanceStateBadge, instancePath, useInstanceRef } from './SqlLayout'

const TABS = [
  ['', 'Overview'],
  ['databases', 'Databases'],
  ['users', 'Users'],
  ['flags', 'Flags'],
  ['query', 'Query'],
  ['backups', 'Backups'],
] as const

const BUSY = new Set(['PENDING_CREATE', 'PENDING_DELETE', 'MAINTENANCE'])

/**
 * InstancePage is one Cloud SQL instance: its state and actions (start,
 * stop, restart, edit, delete) above tabs for its databases, users,
 * flags, query runner and backups.
 */
export function InstancePage() {
  const ref = useInstanceRef()
  const query = useQuery(instanceQuery(ref))
  const i = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const done = (message: string) => () => {
    void qc.invalidateQueries({ queryKey: ['sql'] })
    toast.success(message)
  }
  const start = useMutation({
    mutationFn: () => setActivation(ref, 'ALWAYS'),
    onSuccess: done(`Starting instance ${ref.instance}.`),
  })
  const stop = useMutation({
    mutationFn: () => setActivation(ref, 'NEVER'),
    onSuccess: done(`Stopping instance ${ref.instance}.`),
  })
  const restart = useMutation({
    mutationFn: () => restartInstance(ref),
    onSuccess: done(`Restarting instance ${ref.instance}.`),
  })
  const del = useMutation({
    mutationFn: () => deleteInstance(ref),
    onSuccess: () => {
      done(`Deleting instance ${ref.instance}.`)()
      navigate('/sql')
    },
  })
  const base = instancePath(ref.instance)
  const busy = !i || BUSY.has(i.state ?? '')

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to="/sql" className="underline-offset-4 hover:underline">
              Instances
            </Link>
            {i?.region && (
              <>
                {' '}
                / <span className="font-mono">{i.region}</span>
              </>
            )}
          </p>
          <h1 className="flex items-center gap-3 text-2xl font-semibold">
            <span className="font-mono">{ref.instance}</span>
            {i && <InstanceStateBadge instance={i} />}
          </h1>
        </div>
        {i && (
          <div className="flex flex-wrap gap-2">
            {isStopped(i) ? (
              <Button variant="outline" disabled={busy} onClick={() => start.mutate()}>
                <Play aria-hidden />
                Start
              </Button>
            ) : (
              <ConfirmDialog
                trigger={
                  <Button variant="outline" disabled={busy}>
                    <Square aria-hidden />
                    Stop
                  </Button>
                }
                title={`Stop instance ${ref.instance}?`}
                description="Its PostgreSQL server shuts down (activationPolicy NEVER); its data and settings are kept."
                confirmLabel="Stop instance"
                onConfirm={() => stop.mutateAsync()}
              />
            )}
            <ConfirmDialog
              trigger={
                <Button variant="outline" disabled={busy || !isRunning(i)}>
                  <RotateCw aria-hidden />
                  Restart
                </Button>
              }
              title={`Restart instance ${ref.instance}?`}
              description="Open connections are closed while PostgreSQL restarts."
              confirmLabel="Restart instance"
              onConfirm={() => restart.mutateAsync()}
            />
            <Button variant="outline" asChild>
              <Link to={`${base}/edit`}>
                <Pencil aria-hidden />
                Edit
              </Link>
            </Button>
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Delete
                </Button>
              }
              title={`Delete instance ${ref.instance}?`}
              description="Its PostgreSQL server, data and backups are removed. This cannot be undone."
              confirmLabel="Delete instance"
              onConfirm={() => del.mutateAsync()}
            />
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {i && (
        <>
          <nav aria-label="Instance" className="flex flex-wrap gap-1 border-b">
            {TABS.map(([path, label]) => (
              <NavLink
                key={path}
                to={path ? `${base}/${path}` : base}
                end={!path}
                className={({ isActive }) =>
                  cn(
                    '-mb-px border-b-2 px-3 py-2 text-sm font-medium outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50',
                    isActive
                      ? 'border-primary text-foreground'
                      : 'border-transparent text-muted-foreground hover:text-foreground',
                  )
                }
              >
                {label}
              </NavLink>
            ))}
          </nav>
          <Outlet context={i} />
        </>
      )}
    </div>
  )
}

/** useInstance is the instance an InstancePage tab belongs to. */
export const useInstance = () => useOutletContext<DatabaseInstance>()

/** NotRunning explains why a tab that needs PostgreSQL shows nothing. */
export function NotRunning({ i, what }: { i: DatabaseInstance; what: string }) {
  return (
    <Card role="status">
      <p className="text-sm text-muted-foreground">
        The instance is {isStopped(i) ? 'stopped' : (i.state ?? 'not running')}. {what} once it is
        RUNNABLE.
      </p>
    </Card>
  )
}

/** CopyText is a value with a button that copies it. */
export function CopyText({ label, value }: { label: string; value: string }) {
  const [copied, setCopied] = useState(false)
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch {
      toast.error('The clipboard is not available here; select the text to copy it.')
    }
  }
  return (
    <div className="flex items-start gap-2">
      <code className="min-w-0 flex-1 rounded-md bg-muted px-2 py-1 font-mono text-xs break-all">
        {value}
      </code>
      <Button variant="outline" size="sm" aria-label={`Copy ${label}`} onClick={() => void copy()}>
        <Copy aria-hidden />
        {copied ? 'Copied' : 'Copy'}
      </Button>
    </div>
  )
}

/** hostEnvVar is the `gcpemu env` variable holding an instance's host port. */
export const hostEnvVar = (instance: string) =>
  `GCPEMU_SQL_${instance.toUpperCase().replaceAll('-', '_')}`

/**
 * connectionStrings are the ways to reach an instance: its published host
 * port, its public and private IPs, and the Cloud SQL Auth Proxy against
 * the emulated API.
 */
export function connectionStrings(i: DatabaseInstance, hostAddr?: string, gateway = '') {
  const dsn = (host: string, port = '5432') =>
    `psql "host=${host} port=${port} user=postgres dbname=postgres"`
  const out: [string, string][] = []
  if (hostAddr) {
    const at = hostAddr.lastIndexOf(':')
    out.push(['psql through the host port', dsn(hostAddr.slice(0, at), hostAddr.slice(at + 1))])
  }
  const pub = ipOf(i, 'PRIMARY')
  if (pub) out.push(['psql to the public IP', dsn(pub)])
  const priv = ipOf(i, 'PRIVATE')
  if (priv) out.push(['psql to the private IP (from the VPC)', dsn(priv)])
  if (i.connectionName) {
    out.push([
      'Cloud SQL Auth Proxy',
      `cloud-sql-proxy --sqladmin-api-endpoint ${gateway}/ ${i.connectionName}`,
    ])
  }
  return out
}

const time = (iso?: string) => (iso ? new Date(iso).toLocaleString() : undefined)

/** InstanceOverview shows the instance's settings, connections and JSON. */
export function InstanceOverview() {
  const i = useInstance()
  const env = useQuery(envQuery())
  const st = i.settings ?? {}
  const labels = Object.entries(st.userLabels ?? {})
  const strings = connectionStrings(i, env.data?.[hostEnvVar(i.name)], origin())
  return (
    <div className="flex flex-col gap-6">
      <Card aria-labelledby="connect-title">
        <CardTitle id="connect-title">Connect to this instance</CardTitle>
        <div className="flex flex-col gap-3 text-sm">
          {i.connectionName && (
            <div className="flex flex-col gap-1">
              <span className="text-muted-foreground">Connection name</span>
              <CopyText label="connection name" value={i.connectionName} />
            </div>
          )}
          {strings.map(([label, value]) => (
            <div key={label} className="flex flex-col gap-1">
              <span className="text-muted-foreground">{label}</span>
              <CopyText label={label} value={value} />
            </div>
          ))}
        </div>
      </Card>
      <Card>
        <CardTitle>Details</CardTitle>
        <DetailList
          label="Instance details"
          rows={[
            ['Database version', i.databaseVersion && <Mono>{i.databaseVersion}</Mono>],
            [
              'Installed version',
              i.databaseInstalledVersion && <Mono>{i.databaseInstalledVersion}</Mono>,
            ],
            ['Region', i.region && <Mono>{i.region}</Mono>],
            ['Zone', i.gceZone && <Mono>{i.gceZone}</Mono>],
            ['Edition', st.edition],
            ['Tier', st.tier && <Mono>{st.tier}</Mono>],
            ['Availability', st.availabilityType],
            [
              'Disk',
              st.dataDiskSizeGb ? `${st.dataDiskSizeGb} GB ${st.dataDiskType ?? ''}` : undefined,
            ],
            ['Activation policy', st.activationPolicy],
            [
              'IP addresses',
              i.ipAddresses?.length ? (
                <Mono>{i.ipAddresses.map((a) => `${a.ipAddress} (${a.type})`).join(', ')}</Mono>
              ) : undefined,
            ],
            [
              'Authorized networks',
              st.ipConfiguration?.authorizedNetworks?.length ? (
                <Mono>
                  {st.ipConfiguration.authorizedNetworks
                    .map((a) => (a.name ? `${a.name}=${a.value}` : a.value))
                    .join(', ')}
                </Mono>
              ) : undefined,
            ],
            [
              'Private network',
              st.ipConfiguration?.privateNetwork && (
                <Mono>{st.ipConfiguration.privateNetwork}</Mono>
              ),
            ],
            ['SSL mode', st.ipConfiguration?.sslMode],
            ['Data API access', st.dataApiAccess === 'ALLOW_DATA_API' ? 'Allowed' : 'Not allowed'],
            ['Deletion protection', st.deletionProtectionEnabled ? 'On' : 'Off'],
            [
              'Database flags',
              st.databaseFlags?.length ? (
                <Mono>{st.databaseFlags.map((f) => `${f.name}=${f.value}`).join(', ')}</Mono>
              ) : undefined,
            ],
            [
              'Labels',
              labels.length > 0 && <Mono>{labels.map(([k, v]) => `${k}=${v}`).join(', ')}</Mono>,
            ],
            [
              'Service account',
              i.serviceAccountEmailAddress && <Mono>{i.serviceAccountEmailAddress}</Mono>,
            ],
            ['Created', time(i.createTime)],
          ]}
        />
      </Card>
      <Card>
        <CardTitle>Instance resource</CardTitle>
        <JsonView value={i} label="Instance JSON" />
      </Card>
    </div>
  )
}
