import { useMutation, useQuery } from '@tanstack/react-query'
import { Share2, Trash2 } from 'lucide-react'
import { useId, useState } from 'react'

import { errorMessage } from '@/api/errors'
import { externalQuery } from '@/api/queries'

import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { DetailList, Mono } from '@/components/resource/DetailList'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { PROJECT_ID } from '@/lib/services'
import { toast } from '@/lib/toast'

import {
  delegate,
  linkStatusQuery,
  linksQuery,
  share,
  sharedWith,
  unavailable,
  undelegate,
  unpair,
  unshare,
  type Delegation,
  type LinkState,
  type LinkStatus,
  type Share,
} from './api'
import { DiscoveredInstances, PairingRequests } from './Pairing'
import { useRefreshLinks } from './refresh'

const time = (iso?: string) => {
  if (!iso) return undefined
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}

/** Reachability is what the latest dial over a link found. */
function Reachability({ status }: { status: LinkStatus | undefined }) {
  if (!status) return <Badge variant="outline">checking…</Badge>
  if (status.reachable) return <Badge variant="success">reachable</Badge>
  if (status.inboundOnly) {
    return (
      <Badge variant="outline" title={status.error}>
        inbound only
      </Badge>
    )
  }
  return (
    <Badge variant="destructive" title={status.error}>
      unreachable
    </Badge>
  )
}

/** SharedByIt lists the Projects a link shares with this Instance, to delegate. */
function SharedByIt({
  name,
  status,
  delegated,
}: {
  name: string
  status: LinkStatus | undefined
  delegated: Delegation[]
}) {
  const refresh = useRefreshLinks()
  const add = useMutation({
    mutationFn: (project: string) => delegate({ project, link: name }),
    onSuccess: (d) => {
      refresh()
      toast.success(`Delegated ${d.project} to ${d.link}.`)
    },
  })
  if (status?.reachable && !status.shared) {
    return (
      <p className="text-sm text-muted-foreground">{name} did not say which Projects it shares.</p>
    )
  }
  if (!status?.reachable) {
    return <p className="text-sm text-muted-foreground">Known once {name} is reachable.</p>
  }
  const projects = status.shared ?? []
  if (projects.length === 0) {
    return <p className="text-sm text-muted-foreground">None.</p>
  }
  return (
    <ul aria-label={`Shared by ${name}`} className="flex flex-col gap-1">
      {projects.map((p) => {
        const to = delegated.find((d) => d.project === p)?.link
        return (
          <li key={p} data-project={p} className="flex items-center gap-2 text-sm">
            <span className="font-mono">{p}</span>
            {to === name ? (
              <Badge variant="success">delegated</Badge>
            ) : to ? (
              <Badge variant="outline">delegated to {to}</Badge>
            ) : (
              <Button
                variant="outline"
                size="sm"
                aria-label={`Delegate ${p}`}
                disabled={add.isPending}
                onClick={() => add.mutate(p)}
              >
                Delegate
              </Button>
            )}
          </li>
        )
      })}
    </ul>
  )
}

function LinkCard({
  link,
  status,
  shared,
  delegated,
}: {
  link: LinkState
  status: LinkStatus | undefined
  shared: string[]
  delegated: Delegation[]
}) {
  const name = link.name ?? ''
  const refresh = useRefreshLinks()
  const remove = useMutation({
    mutationFn: () => unpair(name),
    onSuccess: () => {
      refresh({ rediscover: true })
      toast.success(`Unpaired ${name}.`)
    },
  })
  const lost = delegated.filter((d) => d.link === name).map((d) => d.project)
  const broken = link.state === 'broken' || status?.state === 'broken'
  return (
    <Card aria-label={`Link ${name}`}>
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="font-mono text-base font-semibold">{name}</h3>
        <Badge variant={broken ? 'destructive' : 'success'}>{broken ? 'broken' : 'paired'}</Badge>
        <Reachability status={status} />
        <div className="ml-auto">
          <ConfirmDialog
            trigger={
              <Button variant="outline" size="sm" aria-label={`Unpair ${name}`}>
                Unpair
              </Button>
            }
            title={`Unpair ${name}?`}
            description={
              <>
                This Instance stops accepting {name} at once, and stops sharing Projects with it
                {lost.length > 0 && <> and delegating {lost.join(', ')} to it</>}. Pairing again
                needs approval on both sides.
              </>
            }
            confirmLabel="Unpair"
            onConfirm={() => remove.mutateAsync()}
          />
        </div>
      </div>
      {(link.problem || (status && !status.reachable && status.error)) && (
        <p role="status" className="text-sm text-destructive">
          {link.problem || status?.error}
        </p>
      )}
      {(link.warning || status?.warning) && (
        <p className="text-sm text-warning">{link.warning || status?.warning}</p>
      )}
      <DetailList
        label={`Link ${name} details`}
        rows={[
          ['Instance', link.instance],
          ['Instance ID', link.instanceId && <Mono>{link.instanceId}</Mono>],
          [
            'Address',
            link.address ? <Mono>{link.address}</Mono> : 'none: it connects to this Instance',
          ],
          ['Protocol version', String(status?.version ?? link.version ?? '')],
          ['CA fingerprint', link.fingerprint && <Mono>{link.fingerprint}</Mono>],
          ['Paired', time(link.pairedAt)],
        ]}
      />
      <div className="grid gap-4 sm:grid-cols-2">
        <div className="flex flex-col gap-1">
          <h4 className="text-sm font-medium">Shared by {name}</h4>
          <SharedByIt name={name} status={status} delegated={delegated} />
        </div>
        <div className="flex flex-col gap-1">
          <h4 className="text-sm font-medium">Shared with {name}</h4>
          {shared.length === 0 ? (
            <p className="text-sm text-muted-foreground">None.</p>
          ) : (
            <ul aria-label={`Shared with ${name}`} className="flex flex-col gap-1">
              {shared.map((p) => (
                <li key={p} className="font-mono text-sm">
                  {p}
                </li>
              ))}
            </ul>
          )}
        </div>
      </div>
    </Card>
  )
}

function LinkList() {
  const query = useQuery(linksQuery())
  const status = useQuery(linkStatusQuery()).data
  const links = query.data?.links ?? []
  return (
    <section aria-labelledby="links-paired" className="flex flex-col gap-3">
      <h2 id="links-paired" className="text-lg font-semibold">
        Linked Instances
      </h2>
      <QueryStatus query={query} />
      {query.data && links.length === 0 && (
        <p className="text-sm text-muted-foreground" role="status">
          No links yet. Pair with a Discovered Instance below, or run{' '}
          <span className="font-mono">gcpemu link pair</span>.
        </p>
      )}
      {links.map((l) => (
        <LinkCard
          key={l.name}
          link={l}
          status={status?.find((s) => s.name === l.name)}
          shared={sharedWith(query.data?.shared ?? [], l.name ?? '')}
          delegated={query.data?.delegated ?? []}
        />
      ))}
    </section>
  )
}

/** DelegationRow is a Delegated Project, available only once its link answered. */
function DelegationRow({ d, status }: { d: Delegation; status: LinkStatus[] | undefined }) {
  const dialled = status?.some((s) => s.name === d.link)
  const down = unavailable(status, d.link)
  const refresh = useRefreshLinks()
  const remove = useMutation({
    mutationFn: () => undelegate(d.project),
    onSuccess: () => {
      refresh()
      toast.success(`Stopped delegating ${d.project}.`)
    },
  })
  return (
    <TableRow>
      <TableCell className="font-mono">
        <Link to={`/?project=${encodeURIComponent(d.project)}`}>{d.project}</Link>
      </TableCell>
      <TableCell className="font-mono">{d.link}</TableCell>
      <TableCell>
        {!dialled ? (
          <Badge variant="outline">checking…</Badge>
        ) : down ? (
          <Badge variant="destructive" title={down}>
            unavailable
          </Badge>
        ) : (
          <Badge variant="success">available</Badge>
        )}
      </TableCell>
      <TableCell className="text-right">
        <ConfirmDialog
          trigger={
            <Button variant="ghost" size="icon" aria-label={`Undelegate ${d.project}`}>
              <Trash2 />
            </Button>
          }
          title={`Stop delegating ${d.project}?`}
          description={`This Instance stops forwarding requests for ${d.project} to ${d.link}. Its resources stay on ${d.link}; nothing is created here.`}
          confirmLabel="Undelegate"
          onConfirm={() => remove.mutateAsync()}
        />
      </TableCell>
    </TableRow>
  )
}

function ShareRow({ sh }: { sh: Share }) {
  const refresh = useRefreshLinks()
  const remove = useMutation({
    mutationFn: () => unshare(sh),
    onSuccess: () => {
      refresh()
      toast.success(`Stopped sharing ${sh.project}.`)
    },
  })
  return (
    <TableRow>
      <TableCell className="font-mono">{sh.project}</TableCell>
      <TableCell>{sh.with ? <Mono>{sh.with}</Mono> : 'every link'}</TableCell>
      <TableCell className="text-right">
        <ConfirmDialog
          trigger={
            <Button variant="ghost" size="icon" aria-label={`Unshare ${sh.project}`}>
              <Trash2 />
            </Button>
          }
          title={`Stop sharing ${sh.project}?`}
          description={`Linked Instances that delegate ${sh.project} are refused from now on. Its resources stay here.`}
          confirmLabel="Unshare"
          onConfirm={() => remove.mutateAsync()}
        />
      </TableCell>
    </TableRow>
  )
}

/** SharedProjects lists and changes the Projects this Instance offers its links. */
function SharedProjects() {
  const id = useId()
  const data = useQuery(linksQuery()).data
  const shared = data?.shared ?? []
  const [project, setProject] = useState('')
  const [withLink, setWithLink] = useState('')
  const [error, setError] = useState('')
  const refresh = useRefreshLinks()
  const add = useMutation({
    meta: { toast: false },
    mutationFn: share,
    onSuccess: (sh) => {
      refresh()
      setProject('')
      setError('')
      toast.success(`Shared ${sh.project} with ${sh.with || 'every link'}.`)
    },
    onError: (e) => setError(errorMessage(e)),
  })
  return (
    <Card>
      <div>
        <h2 className="text-lg font-semibold">Shared Projects</h2>
        <p className="text-sm text-muted-foreground">
          Projects this Instance offers to the Instances it is paired with. Sharing alone forwards
          nothing: the other Instance delegates a Project to use it.
        </p>
      </div>
      <form
        aria-label="Share a Project"
        noValidate
        className="flex flex-wrap items-end gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          const p = project.trim()
          if (!PROJECT_ID.test(p)) {
            setError(
              'A Project ID is 6 to 30 lowercase letters, digits or hyphens, starting with a letter and not ending with a hyphen.',
            )
            return
          }
          add.mutate(withLink ? { project: p, with: withLink } : { project: p })
        }}
      >
        <div className="flex flex-col gap-1">
          <label htmlFor={`${id}-project`} className="text-sm font-medium">
            Project ID
          </label>
          <Input
            id={`${id}-project`}
            autoComplete="off"
            spellCheck={false}
            placeholder="my-project"
            aria-invalid={!!error || undefined}
            value={project}
            onChange={(e) => setProject(e.target.value)}
          />
        </div>
        <div className="flex flex-col gap-1">
          <label htmlFor={`${id}-with`} className="text-sm font-medium">
            With
          </label>
          <NativeSelect
            id={`${id}-with`}
            value={withLink}
            onChange={(e) => setWithLink(e.target.value)}
          >
            <option value="">Every link</option>
            {(data?.links ?? []).map((l) => (
              <option key={l.name} value={l.name}>
                {l.name}
              </option>
            ))}
          </NativeSelect>
        </div>
        <Button type="submit" disabled={add.isPending}>
          <Share2 aria-hidden />
          Share
        </Button>
        {error && (
          <p role="alert" className="basis-full text-sm text-destructive">
            {error}
          </p>
        )}
      </form>
      {shared.length > 0 && (
        <Table aria-label="Shared Projects">
          <TableHeader>
            <TableRow>
              <TableHead>Project</TableHead>
              <TableHead>With</TableHead>
              <TableHead>
                <span className="sr-only">Actions</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {shared.map((sh) => (
              <ShareRow key={`${sh.project}/${sh.with ?? ''}`} sh={sh} />
            ))}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}

/** DelegatedProjects lists the Projects this Instance forwards to its links. */
function DelegatedProjects() {
  const delegated = useQuery(linksQuery()).data?.delegated ?? []
  const status = useQuery(linkStatusQuery()).data
  return (
    <Card>
      <div>
        <h2 className="text-lg font-semibold">Delegated Projects</h2>
        <p className="text-sm text-muted-foreground">
          Projects this Instance forwards to the Linked Instance that shares them. Choose one in the
          Project picker to browse its resources through this Instance.
        </p>
      </div>
      {delegated.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          None. Delegate a Project a Linked Instance shares from its link above.
        </p>
      ) : (
        <Table aria-label="Delegated Projects">
          <TableHeader>
            <TableRow>
              <TableHead>Project</TableHead>
              <TableHead>Link</TableHead>
              <TableHead>State</TableHead>
              <TableHead>
                <span className="sr-only">Actions</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {delegated.map((d) => (
              <DelegationRow key={d.project} d={d} status={status} />
            ))}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}

/** DelegatedApis lists the APIs this Instance forwards to External emulators. */
function DelegatedApis() {
  const query = useQuery(externalQuery())
  const apis = query.data ?? []
  return (
    <Card>
      <div>
        <h2 className="text-lg font-semibold">Delegated APIs</h2>
        <p className="text-sm text-muted-foreground">
          APIs no Service implements, which this Instance forwards to External emulators set with{' '}
          <span className="font-mono">--external</span>. It checks no IAM for them.
        </p>
      </div>
      <QueryStatus query={query} rows={1} />
      {query.data && apis.length === 0 && <p className="text-sm text-muted-foreground">None.</p>}
      {apis.length > 0 && (
        <Table aria-label="Delegated APIs">
          <TableHeader>
            <TableRow>
              <TableHead>API</TableHead>
              <TableHead>External emulator</TableHead>
              <TableHead>State</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {apis.map((a) => (
              <TableRow key={a.api}>
                <TableCell className="font-mono">{a.api}</TableCell>
                <TableCell className="font-mono">{a.address}</TableCell>
                <TableCell>
                  {a.reachable ? (
                    <Badge variant="success">reachable</Badge>
                  ) : (
                    <Badge variant="destructive" title={a.error}>
                      unreachable
                    </Badge>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}

/**
 * Links shows this Instance's Linked Instances and their state, the
 * Projects shared each way and delegated, the Delegated APIs, and the
 * Discovered Instances and Pairing requests to pair with (ADR 0027).
 */
export function Links() {
  const fingerprint = useQuery(linksQuery()).data?.fingerprint
  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-2xl font-semibold">Links</h1>
        <p className="text-sm text-muted-foreground">
          Instances this one is paired with, and the Projects and APIs it delegates.
        </p>
        {fingerprint && (
          <p className="mt-1 text-sm">
            This Instance&apos;s CA fingerprint: <Mono>{fingerprint}</Mono>
          </p>
        )}
      </div>
      <PairingRequests />
      <LinkList />
      <DelegatedProjects />
      <SharedProjects />
      <DelegatedApis />
      <DiscoveredInstances />
    </div>
  )
}
