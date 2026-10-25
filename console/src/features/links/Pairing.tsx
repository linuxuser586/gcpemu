import { useMutation, useQuery } from '@tanstack/react-query'
import { Check, RefreshCw, X } from 'lucide-react'
import { useId, useState } from 'react'

import { errorMessage } from '@/api/errors'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogTitle,
  DialogTrigger,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { toast } from '@/lib/toast'

import {
  approve,
  discoveredQuery,
  identify,
  pair,
  pairingRequestsQuery,
  reject,
  type Discovered,
  type PairingRequest,
} from './api'
import { useRefreshLinks } from './refresh'

// Pairing (ADR 0027) is approved on both sides: this side sends a
// Pairing request to a Discovered Instance, or one at an address, and
// approves or rejects the requests other Instances send it, each after
// comparing the fingerprint presented with the one that Instance shows.

/** PairTarget is an Instance to pair with, as discovery or identify found it. */
interface PairTarget {
  label: string
  address: string
  fingerprint: string
}

/**
 * PairDialog sends a Pairing request to target and waits, up to 5
 * minutes, for the other Instance to approve it.
 */
function PairDialog({
  target,
  open,
  onOpenChange,
}: {
  target: PairTarget
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const id = useId()
  const [name, setName] = useState('')
  const [error, setError] = useState('')
  const refresh = useRefreshLinks()
  const send = useMutation({
    meta: { toast: false },
    mutationFn: () =>
      pair({
        address: target.address,
        fingerprint: target.fingerprint,
        ...(name.trim() ? { name: name.trim() } : {}),
      }),
    onSuccess: (l) => {
      refresh({ rediscover: true })
      onOpenChange(false)
      toast.success(`Paired with ${l.name}.`)
    },
    onError: (e) => setError(errorMessage(e)),
  })
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        onOpenChange(o)
        setError('')
      }}
    >
      <DialogContent>
        <DialogTitle>Pair with {target.label}?</DialogTitle>
        <DialogDescription>
          Pair only if this fingerprint is the one {target.label} shows for its own Instance CA. It
          then has to approve the request on its side.
        </DialogDescription>
        <p className="font-mono text-sm break-all">{target.fingerprint}</p>
        <div className="flex flex-col gap-1">
          <label htmlFor={`${id}-name`} className="text-sm font-medium">
            Link name
          </label>
          <Input
            id={`${id}-name`}
            autoComplete="off"
            spellCheck={false}
            placeholder="Its Instance name"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </div>
        {send.isPending && (
          <p role="status" className="text-sm text-muted-foreground">
            Waiting for {target.label} to approve. Approve it there under Links, Pairing requests,
            or with <span className="font-mono">gcpemu link approve</span>.
          </p>
        )}
        {error && (
          <p role="alert" className="text-sm text-destructive">
            {error}
          </p>
        )}
        <DialogFooter>
          <DialogClose asChild>
            <Button variant="outline">Cancel</Button>
          </DialogClose>
          <Button disabled={send.isPending} onClick={() => send.mutate()}>
            Send Pairing request
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function DiscoveredRow({ d }: { d: Discovered }) {
  const [open, setOpen] = useState(false)
  return (
    <TableRow>
      <TableCell className="font-mono">{d.name}</TableCell>
      <TableCell className="font-mono">{d.address}</TableCell>
      <TableCell className="font-mono text-xs break-all">{d.fingerprint}</TableCell>
      <TableCell>{d.source === 'static' ? 'static address' : 'mDNS'}</TableCell>
      <TableCell className="text-right">
        {d.link ? (
          <Badge variant="success">paired as {d.link}</Badge>
        ) : d.problem ? (
          <span className="text-sm text-destructive">{d.problem}</span>
        ) : (
          <>
            <Button
              variant="outline"
              size="sm"
              aria-label={`Pair ${d.name}`}
              onClick={() => setOpen(true)}
            >
              Pair
            </Button>
            <PairDialog
              target={{ label: d.name, address: d.address, fingerprint: d.fingerprint ?? '' }}
              open={open}
              onOpenChange={setOpen}
            />
          </>
        )}
      </TableCell>
    </TableRow>
  )
}

/** PairByAddress pairs with an Instance discovery does not reach. */
function PairByAddress() {
  const id = useId()
  const [address, setAddress] = useState('')
  const [target, setTarget] = useState<PairTarget>()
  const [error, setError] = useState('')
  const identifyIt = useMutation({
    meta: { toast: false },
    mutationFn: identify,
    onSuccess: (i, addr) => {
      setError('')
      setTarget({ label: i.name ?? addr, address: addr, fingerprint: i.fingerprint ?? '' })
    },
    onError: (e) => setError(errorMessage(e)),
  })
  return (
    <>
      <form
        aria-label="Pair by address"
        className="flex flex-wrap items-end gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          if (address.trim()) identifyIt.mutate(address.trim())
        }}
      >
        <div className="flex flex-col gap-1">
          <label htmlFor={`${id}-address`} className="text-sm font-medium">
            Link address
          </label>
          <Input
            id={`${id}-address`}
            autoComplete="off"
            spellCheck={false}
            placeholder="office.local:4511"
            aria-invalid={!!error || undefined}
            value={address}
            onChange={(e) => setAddress(e.target.value)}
          />
        </div>
        <Button type="submit" variant="outline" disabled={!address.trim() || identifyIt.isPending}>
          Identify
        </Button>
        {error && (
          <p role="alert" className="basis-full text-sm text-destructive">
            {error}
          </p>
        )}
      </form>
      {target && (
        <PairDialog
          target={target}
          open
          onOpenChange={(o) => {
            if (!o) setTarget(undefined)
          }}
        />
      )}
    </>
  )
}

/**
 * DiscoveredInstances lists the Instances found on the LAN or at the
 * --link-discover addresses, to pair with. Discovery never pairs.
 */
export function DiscoveredInstances() {
  const query = useQuery(discoveredQuery())
  const found = query.data ?? []
  return (
    <Card>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold">Discovered Instances</h2>
          <p className="text-sm text-muted-foreground">
            Instances started with <span className="font-mono">--link-bind</span> on the LAN, and
            those at the <span className="font-mono">--link-discover</span> addresses.
          </p>
        </div>
        <Button variant="outline" disabled={query.isFetching} onClick={() => void query.refetch()}>
          <RefreshCw aria-hidden />
          Discover
        </Button>
      </div>
      <QueryStatus query={query} rows={1} />
      {query.data && found.length === 0 && (
        <p className="text-sm text-muted-foreground" role="status">
          No Instances found. Pair by address where multicast does not reach.
        </p>
      )}
      {found.length > 0 && (
        <Table aria-label="Discovered Instances">
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Address</TableHead>
              <TableHead>CA fingerprint</TableHead>
              <TableHead>Found by</TableHead>
              <TableHead>
                <span className="sr-only">Actions</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {found.map((d) => (
              <DiscoveredRow key={`${d.name}/${d.address}`} d={d} />
            ))}
          </TableBody>
        </Table>
      )}
      <PairByAddress />
    </Card>
  )
}

/** ApproveDialog approves a Pairing request, optionally naming the link here. */
function ApproveDialog({ r }: { r: PairingRequest }) {
  const id = useId()
  const [open, setOpen] = useState(false)
  const [name, setName] = useState('')
  const [error, setError] = useState('')
  const refresh = useRefreshLinks()
  const who = r.name ?? r.instanceId ?? ''
  const accept = useMutation({
    meta: { toast: false },
    mutationFn: () => approve(r.id ?? '', name.trim()),
    onSuccess: (l) => {
      refresh({ rediscover: true })
      setOpen(false)
      toast.success(l.name === who ? `Paired with ${who}.` : `Paired with ${who} as ${l.name}.`)
    },
    onError: (e) => setError(errorMessage(e)),
  })
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        setOpen(o)
        setError('')
      }}
    >
      <DialogTrigger asChild>
        <Button variant="outline" size="sm" aria-label={`Approve ${who}`}>
          <Check aria-hidden />
          Approve
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogTitle>Approve the Pairing request from {who}?</DialogTitle>
        <DialogDescription>
          {who} will be able to assert its callers&apos; Principals to this Instance for the
          Projects shared with it. Approve only if this fingerprint is the one {who} shows for its
          own Instance CA:
        </DialogDescription>
        <p className="font-mono text-sm break-all">{r.fingerprint}</p>
        {r.replaces && (
          <p className="text-sm text-warning">
            Approving replaces the link {r.replaces}, which is pinned to another Instance CA.
          </p>
        )}
        <div className="flex flex-col gap-1">
          <label htmlFor={`${id}-name`} className="text-sm font-medium">
            Link name
          </label>
          <Input
            id={`${id}-name`}
            autoComplete="off"
            spellCheck={false}
            placeholder={who}
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </div>
        {error && (
          <p role="alert" className="text-sm text-destructive">
            {error}
          </p>
        )}
        <DialogFooter>
          <DialogClose asChild>
            <Button variant="outline">Cancel</Button>
          </DialogClose>
          <Button disabled={accept.isPending} onClick={() => accept.mutate()}>
            Approve
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function RequestRow({ r }: { r: PairingRequest }) {
  const refresh = useRefreshLinks()
  const who = r.name ?? r.instanceId ?? ''
  const refuse = useMutation({
    mutationFn: () => reject(r.id ?? ''),
    onSuccess: () => {
      refresh()
      toast.success(`Rejected the Pairing request from ${who}.`)
    },
  })
  return (
    <TableRow>
      <TableCell>
        <span className="font-mono">{who}</span>
        {r.replaces && (
          <Badge variant="warning" className="ml-2" title="It is pinned to another Instance CA.">
            replaces {r.replaces}
          </Badge>
        )}
      </TableCell>
      <TableCell className="font-mono text-xs break-all">{r.fingerprint}</TableCell>
      <TableCell className="font-mono">{r.address}</TableCell>
      <TableCell className="text-right whitespace-nowrap">
        <ApproveDialog r={r} />
        <ConfirmDialog
          trigger={
            <Button variant="ghost" size="sm" aria-label={`Reject ${who}`}>
              <X aria-hidden />
              Reject
            </Button>
          }
          title={`Reject the Pairing request from ${who}?`}
          description={`${who} is told its request was rejected. It can ask again.`}
          confirmLabel="Reject"
          onConfirm={() => refuse.mutateAsync()}
        />
      </TableCell>
    </TableRow>
  )
}

/** PairingRequests lists the requests waiting for this Instance's approval. */
export function PairingRequests() {
  const query = useQuery(pairingRequestsQuery())
  const requests = query.data ?? []
  return (
    <Card>
      <div>
        <h2 className="text-lg font-semibold">Pairing requests</h2>
        <p className="text-sm text-muted-foreground">
          Instances asking to pair with this one. A request lapses if it is neither approved nor
          rejected.
        </p>
      </div>
      <QueryStatus query={query} rows={1} />
      {query.data && requests.length === 0 && (
        <p className="text-sm text-muted-foreground" role="status">
          No Pairing requests waiting.
        </p>
      )}
      {requests.length > 0 && (
        <Table aria-label="Pairing requests">
          <TableHeader>
            <TableRow>
              <TableHead>Instance</TableHead>
              <TableHead>CA fingerprint</TableHead>
              <TableHead>From</TableHead>
              <TableHead>
                <span className="sr-only">Actions</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {requests.map((r) => (
              <RequestRow key={r.id} r={r} />
            ))}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}
