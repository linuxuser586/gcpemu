import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Copy, Eye, EyeOff, Plus } from 'lucide-react'
import { useId, useState } from 'react'

import { errorMessage } from '@/api/errors'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogTitle,
  DialogTrigger,
} from '@/components/ui/dialog'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { Textarea } from '@/components/ui/textarea'
import { toast } from '@/lib/toast'

import { formatSeconds, time } from '../gcs/format'
import {
  accessVersion,
  addVersion,
  changeVersion,
  decodePayload,
  seconds,
  versionId,
  versionsQuery,
  type SecretRef,
  type SecretVersion,
  type VersionState,
} from './api'
import { isManaged, useSecret } from './SecretPage'
import { useSecretRef } from './SecretsLayout'

const STATE_BADGE: Record<VersionState, 'success' | 'warning' | 'destructive'> = {
  ENABLED: 'success',
  DISABLED: 'warning',
  DESTROYED: 'destructive',
}

/**
 * Versions lists a secret's versions, newest first, with their aliases;
 * adds versions and views, enables, disables and destroys them.
 */
export function Versions() {
  const s = useSecret()
  const ref = useSecretRef()
  const query = useQuery(versionsQuery(ref))
  const qc = useQueryClient()
  const aliases = new Map<string, string[]>()
  for (const [alias, v] of Object.entries(s.versionAliases ?? {})) {
    aliases.set(String(v), [...(aliases.get(String(v)) ?? []), alias])
  }
  const change = useMutation({
    mutationFn: ({ v, action }: { v: string; action: 'enable' | 'disable' | 'destroy' }) =>
      changeVersion(ref, v, action),
    onSuccess: (out, { v, action }) => {
      void qc.invalidateQueries({ queryKey: ['secrets'] })
      const done = { enable: 'Enabled', disable: 'Disabled', destroy: 'Destroyed' }[action]
      toast.success(
        out.scheduledDestroyTime
          ? `Version ${v} is disabled and will be destroyed ${time(out.scheduledDestroyTime)}.`
          : `${done} version ${v}.`,
      )
    },
  })
  const ttl = formatSeconds(seconds(s.versionDestroyTtl))
  const versions = query.data ?? []

  return (
    <Card>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <CardTitle>Versions</CardTitle>
        {isManaged(s) ? (
          <p className="text-sm text-muted-foreground">
            Managed rotation adds the versions of this secret.
          </p>
        ) : (
          <AddVersion r={ref} />
        )}
      </div>
      <QueryStatus query={query} />
      {query.data && versions.length === 0 && (
        <p className="text-sm text-muted-foreground">No versions yet: add one to store a value.</p>
      )}
      {versions.length > 0 && (
        <Table aria-label="Versions">
          <TableHeader>
            <TableRow>
              <TableHead>Version</TableHead>
              <TableHead>State</TableHead>
              <TableHead>Aliases</TableHead>
              <TableHead>Created</TableHead>
              <TableHead>Destroyed</TableHead>
              <TableHead>
                <span className="sr-only">Actions</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {versions.map((v, i) => {
              const id = versionId(v.name)
              const state = v.state ?? 'ENABLED'
              const names = [...(i === 0 ? ['latest'] : []), ...(aliases.get(id) ?? [])]
              return (
                <TableRow key={v.name} data-testid="version">
                  <TableCell className="font-mono">{id}</TableCell>
                  <TableCell>
                    <Badge variant={STATE_BADGE[state]}>{state}</Badge>
                  </TableCell>
                  <TableCell className="font-mono text-xs">{names.join(', ')}</TableCell>
                  <TableCell>{time(v.createTime)}</TableCell>
                  <TableCell>
                    {v.scheduledDestroyTime
                      ? `Scheduled ${time(v.scheduledDestroyTime)}`
                      : time(v.destroyTime)}
                  </TableCell>
                  <TableCell>
                    <div className="flex flex-wrap justify-end gap-2">
                      {state === 'ENABLED' && <ViewValue r={ref} version={id} />}
                      {state === 'ENABLED' && (
                        <Button
                          size="sm"
                          variant="outline"
                          aria-label={`Disable version ${id}`}
                          onClick={() => change.mutate({ v: id, action: 'disable' })}
                        >
                          Disable
                        </Button>
                      )}
                      {state === 'DISABLED' && (
                        <Button
                          size="sm"
                          variant="outline"
                          aria-label={`Enable version ${id}`}
                          onClick={() => change.mutate({ v: id, action: 'enable' })}
                        >
                          Enable
                        </Button>
                      )}
                      {state !== 'DESTROYED' && !v.scheduledDestroyTime && (
                        <ConfirmDialog
                          trigger={
                            <Button
                              size="sm"
                              variant="outline"
                              aria-label={`Destroy version ${id}`}
                            >
                              Destroy
                            </Button>
                          }
                          title={`Destroy version ${id}?`}
                          description={
                            ttl
                              ? `The version is disabled now and its payload destroyed after ${ttl}; enabling it before then cancels the destruction.`
                              : 'Its payload is destroyed for good. This cannot be undone.'
                          }
                          confirmLabel="Destroy version"
                          onConfirm={() => change.mutateAsync({ v: id, action: 'destroy' })}
                        />
                      )}
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

/** AddVersion adds a version holding the text typed (UTF-8). */
function AddVersion({ r }: { r: SecretRef }) {
  const [open, setOpen] = useState(false)
  const [text, setText] = useState('')
  const [error, setError] = useState<string>()
  const id = useId()
  const qc = useQueryClient()
  const add = useMutation({
    meta: { toast: false },
    mutationFn: () => addVersion(r, text),
    onSuccess: (v: SecretVersion) => {
      void qc.invalidateQueries({ queryKey: ['secrets'] })
      toast.success(`Added version ${versionId(v.name)}.`)
      setOpen(false)
      setText('')
    },
    onError: (e) => setError(errorMessage(e)),
  })
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        setOpen(o)
        setError(undefined)
      }}
    >
      <DialogTrigger asChild>
        <Button>
          <Plus aria-hidden />
          Add version
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogTitle>Add version</DialogTitle>
        <DialogDescription>
          The value becomes the secret&apos;s newest version and what{' '}
          <span className="font-mono">latest</span> refers to.
        </DialogDescription>
        <form
          className="flex flex-col gap-3"
          onSubmit={(e) => {
            e.preventDefault()
            add.mutate()
          }}
        >
          <label htmlFor={id} className="text-sm font-medium">
            Secret value
          </label>
          <Textarea
            id={id}
            rows={5}
            spellCheck={false}
            autoComplete="off"
            className="font-mono"
            value={text}
            onChange={(e) => setText(e.target.value)}
          />
          {error && (
            <p role="alert" className="text-sm text-destructive">
              {error}
            </p>
          )}
          <DialogFooter>
            <DialogClose asChild>
              <Button type="button" variant="outline">
                Cancel
              </Button>
            </DialogClose>
            <Button type="submit" disabled={add.isPending}>
              Add version
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/**
 * ViewValue accesses a version's payload when opened and shows it masked
 * until revealed; payloads that are not UTF-8 are shown as base64.
 */
function ViewValue({ r, version }: { r: SecretRef; version: string }) {
  const [open, setOpen] = useState(false)
  const [shown, setShown] = useState(false)
  const [copied, setCopied] = useState(false)
  const query = useQuery({
    queryKey: ['secrets', 'access', r, version],
    queryFn: () => accessVersion(r, version),
    enabled: open,
    gcTime: 0,
    staleTime: 0,
  })
  const data = query.data?.payload?.data ?? ''
  const text = data ? decodePayload(data) : ''
  const value = text ?? data
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value)
      setCopied(true)
    } catch {
      toast.error('The clipboard is not available here; reveal the value to copy it.')
    }
  }
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        setOpen(o)
        setShown(false)
        setCopied(false)
      }}
    >
      <DialogTrigger asChild>
        <Button size="sm" variant="outline" aria-label={`View value of version ${version}`}>
          <Eye aria-hidden />
          View value
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogTitle>Version {version}</DialogTitle>
        <DialogDescription>
          Read with <span className="font-mono">versions.access</span>
          {text === undefined && data
            ? '; the payload is not UTF-8 text, so it is shown as base64'
            : ''}
          .
        </DialogDescription>
        <QueryStatus query={query} />
        {query.data && (
          <pre
            aria-label="Secret value"
            className="max-h-64 overflow-auto rounded-md bg-muted p-3 font-mono text-sm break-all whitespace-pre-wrap"
          >
            {shown ? value : '•'.repeat(Math.min(Math.max(value.length, 8), 32))}
          </pre>
        )}
        <p role="status" className="sr-only">
          {copied && 'Copied to the clipboard'}
        </p>
        <DialogFooter>
          <Button variant="outline" disabled={!query.data} onClick={() => setShown((v) => !v)}>
            {shown ? <EyeOff aria-hidden /> : <Eye aria-hidden />}
            {shown ? 'Hide' : 'Reveal'}
          </Button>
          <Button variant="outline" disabled={!query.data} onClick={() => void copy()}>
            <Copy aria-hidden />
            {copied ? 'Copied' : 'Copy'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
