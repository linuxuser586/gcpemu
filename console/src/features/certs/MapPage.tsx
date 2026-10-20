import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Pencil, Plus, Trash2 } from 'lucide-react'

import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { CopyAs } from '@/components/resource/CopyAs'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { toast } from '@/lib/toast'

import { StatusBadge } from '../gke/format'
import { time } from '../gcs/format'
import { kindQuery, relPath } from '../lb/api'
import { RefLink } from '../lb/LbLayout'
import {
  deleteEntry,
  deleteMap,
  entriesQuery,
  lastSegment,
  locationOf,
  mapQuery,
  type CertificateMap,
  type CertificateMapEntry,
  type MapRef,
} from './api'
import { certPath, ChooseProject, entryPath, mapPath, useMapRef } from './CertsLayout'
import { entryRecipe, mapRecipe } from './copy'

const pairs = (m?: Record<string, string>) => {
  const e = Object.entries(m ?? {})
  return e.length > 0 ? <Mono>{e.map(([k, v]) => `${k}=${v}`).join(', ')}</Mono> : undefined
}

/** CertLink links a certificate's name to its page. */
export function CertLink({ name }: { name: string }) {
  return (
    <Link
      className="font-mono text-primary underline-offset-4 hover:underline"
      to={certPath({ location: locationOf(name), cert: lastSegment(name) })}
    >
      {lastSegment(name)}
    </Link>
  )
}

/** matchLabel is what an entry serves: its hostname, or the primary matcher. */
export const matchLabel = (e: Pick<CertificateMapEntry, 'hostname' | 'matcher'>) =>
  e.matcher === 'PRIMARY' ? 'Primary (any other hostname)' : (e.hostname ?? '')

/**
 * MapPage is one certificate map: edit and delete above its entries
 * (each edited and deleted here), the target proxies that serve it with
 * their forwarding rules, its configuration and the map as the API
 * returns it.
 */
export function MapPage() {
  const ref = useMapRef()
  const query = useQuery({ ...mapQuery(ref), enabled: !!ref.project })
  const m = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const del = useMutation({
    mutationFn: () => deleteMap(ref),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['certs'] })
      toast.success(`Deleted certificate map ${ref.map}.`)
      navigate('/certs/maps')
    },
  })
  if (!ref.project) return <ChooseProject what="see its certificate maps" />

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to="/certs/maps" className="underline-offset-4 hover:underline">
              Certificate maps
            </Link>{' '}
            / <span className="font-mono">global</span>
          </p>
          <h1 className="text-2xl font-semibold">
            <span className="font-mono">{ref.map}</span>
          </h1>
        </div>
        {m && (
          <div className="flex flex-wrap gap-2">
            <CopyAs what="certificate map" recipe={() => mapRecipe(m, ref.project)} />
            <Button variant="outline" asChild>
              <Link to={mapPath(ref, 'edit')}>
                <Pencil aria-hidden />
                Edit certificate map
              </Link>
            </Button>
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Delete certificate map
                </Button>
              }
              title={`Delete certificate map ${ref.map}?`}
              description="The API refuses while a target proxy uses the map or it has entries. This cannot be undone."
              confirmLabel="Delete certificate map"
              onConfirm={() => del.mutateAsync()}
            />
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {m && <MapDetails m={m} r={ref} />}
    </div>
  )
}

function MapDetails({ m, r }: { m: CertificateMap; r: MapRef }) {
  return (
    <>
      <Entries r={r} />
      <Targets m={m} project={r.project} />
      <Card>
        <CardTitle>Configuration</CardTitle>
        <DetailList
          label="Certificate map details"
          rows={[
            ['Name', <Mono key="n">{m.name}</Mono>],
            ['Description', m.description],
            ['Labels', pairs(m.labels)],
            ['Created', time(m.createTime)],
            ['Updated', time(m.updateTime)],
          ]}
        />
      </Card>
      <Card>
        <CardTitle>Certificate map resource</CardTitle>
        <JsonView value={m} label="Certificate map JSON" />
      </Card>
    </>
  )
}

/** Entries are a map's entries: which certificates serve which hostname. */
function Entries({ r }: { r: MapRef }) {
  const query = useQuery(entriesQuery(r))
  const qc = useQueryClient()
  const del = useMutation({
    mutationFn: (entry: string) => deleteEntry({ ...r, entry }),
    onSuccess: (_, entry) => {
      void qc.invalidateQueries({ queryKey: ['certs'] })
      toast.success(`Deleted certificate map entry ${entry}.`)
    },
  })
  return (
    <Card>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <CardTitle>Entries</CardTitle>
        <Button variant="outline" size="sm" asChild>
          <Link to={mapPath(r, 'create-entry')}>
            <Plus aria-hidden />
            Add entry
          </Link>
        </Button>
      </div>
      <QueryStatus query={query} />
      {query.data?.length === 0 && (
        <p className="text-sm text-muted-foreground">No entries in this certificate map yet.</p>
      )}
      {!!query.data?.length && (
        <Table aria-label="Certificate map entries">
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Hostname</TableHead>
              <TableHead>Certificates</TableHead>
              <TableHead>State</TableHead>
              <TableHead>
                <span className="sr-only">Actions</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {query.data.map((e) => {
              const id = lastSegment(e.name)
              return (
                <TableRow key={e.name} data-testid="certificate-map-entry">
                  <TableCell className="font-mono">{id}</TableCell>
                  <TableCell className="font-mono">{matchLabel(e)}</TableCell>
                  <TableCell>
                    <span className="flex flex-wrap gap-x-3">
                      {(e.certificates ?? []).map((c) => (
                        <CertLink key={c} name={c} />
                      ))}
                    </span>
                  </TableCell>
                  <TableCell>
                    <StatusBadge
                      status={e.state ?? 'PENDING'}
                      tone={e.state === 'ACTIVE' ? 'success' : 'warning'}
                    />
                  </TableCell>
                  <TableCell>
                    <div className="flex justify-end gap-1">
                      <CopyAs
                        what="certificate map entry"
                        name={id}
                        size="icon"
                        recipe={() => entryRecipe(e, r.project)}
                      />
                      <Button variant="ghost" size="icon" asChild>
                        <Link
                          to={entryPath({ map: r.map, entry: id }, 'edit')}
                          aria-label={`Edit entry ${id}`}
                        >
                          <Pencil aria-hidden />
                        </Link>
                      </Button>
                      <ConfirmDialog
                        trigger={
                          <Button variant="ghost" size="icon" aria-label={`Delete entry ${id}`}>
                            <Trash2 aria-hidden />
                          </Button>
                        }
                        title={`Delete entry ${id}?`}
                        description="The map stops serving its certificates for this hostname. This cannot be undone."
                        confirmLabel="Delete entry"
                        onConfirm={() => del.mutateAsync(id)}
                      />
                    </div>
                  </TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}

/**
 * Targets are the target proxies that serve the map (its gclbTargets),
 * each with the forwarding rules that send traffic to it.
 */
function Targets({ m, project }: { m: CertificateMap; project: string }) {
  const targets = m.gclbTargets ?? []
  const rules = useQuery({ ...kindQuery(project, 'forwardingRules'), enabled: targets.length > 0 })
  return (
    <Card>
      <CardTitle>Target proxies</CardTitle>
      {targets.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          No target proxy serves this certificate map yet.
        </p>
      ) : (
        <Table aria-label="Target proxies">
          <TableHeader>
            <TableRow>
              <TableHead>Target proxy</TableHead>
              <TableHead>Forwarding rules</TableHead>
              <TableHead>Addresses</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {targets.map((t) => {
              const proxy = t.targetHttpsProxy ?? t.targetSslProxy ?? ''
              const frs = (rules.data ?? []).filter(
                (f) => relPath(f.target as string | undefined) === relPath(proxy),
              )
              return (
                <TableRow key={proxy} data-testid="gclb-target">
                  <TableCell>
                    <RefLink link={proxy} />
                  </TableCell>
                  <TableCell>
                    <span className="flex flex-wrap gap-x-3">
                      {frs.map((f) => (
                        <RefLink key={f.selfLink} link={f.selfLink} />
                      ))}
                    </span>
                  </TableCell>
                  <TableCell className="font-mono text-xs">
                    {(t.ipConfigs ?? [])
                      .flatMap((ip) => (ip.ports ?? []).map((p) => `${ip.ipAddress}:${p}`))
                      .join(', ')}
                  </TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}
