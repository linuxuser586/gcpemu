import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ArchiveRestore, Plus, Trash2 } from 'lucide-react'
import { useState } from 'react'

import { errorMessage } from '@/api/errors'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { Field, fieldAria } from '@/components/resource/Field'
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

import { StatusBadge } from '../gke/format'
import {
  backupRunsQuery,
  deleteBackupRun,
  insertBackupRun,
  isRunning,
  restoreBackup,
  type InstanceRef,
} from './api'
import { NotRunning, useInstance } from './InstancePage'
import { useInstanceRef } from './SqlLayout'

const TONES: Record<string, 'success' | 'warning' | 'destructive'> = {
  SUCCESSFUL: 'success',
  ENQUEUED: 'warning',
  RUNNING: 'warning',
  DELETION_PENDING: 'warning',
  FAILED: 'destructive',
}

const time = (iso?: string) => (iso ? new Date(iso).toLocaleString() : '—')

/** CreateBackupDialog takes an on-demand backup (backupRuns.insert). */
function CreateBackupDialog({ instanceRef }: { instanceRef: InstanceRef }) {
  const [open, setOpen] = useState(false)
  const [description, setDescription] = useState('')
  const qc = useQueryClient()
  const create = useMutation({
    meta: { toast: false },
    mutationFn: () => insertBackupRun(instanceRef, description.trim()),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['sql'] })
      toast.success(`Backing up instance ${instanceRef.instance}.`)
      setOpen(false)
    },
  })
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        setOpen(o)
        if (o) {
          setDescription('')
          create.reset()
        }
      }}
    >
      <DialogTrigger asChild>
        <Button size="sm">
          <Plus aria-hidden />
          Create backup
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogTitle>Create backup</DialogTitle>
        <DialogDescription>
          An on-demand backup of every database and user, kept until it is deleted.
        </DialogDescription>
        <form
          className="flex flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault()
            create.mutate()
          }}
        >
          <Field id="backup-description" label="Description">
            <Input
              autoComplete="off"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              {...fieldAria('backup-description')}
            />
          </Field>
          {create.isError && (
            <p role="alert" className="text-sm text-destructive">
              {errorMessage(create.error)}
            </p>
          )}
          <DialogFooter>
            <DialogClose asChild>
              <Button type="button" variant="outline">
                Cancel
              </Button>
            </DialogClose>
            <Button type="submit" disabled={create.isPending}>
              Create
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/**
 * Backups lists the instance's backup runs (FR-SQL-008) and restores one
 * onto the instance, replacing its data, databases and users.
 */
export function Backups() {
  const i = useInstance()
  const ref = useInstanceRef()
  const query = useQuery(backupRunsQuery(ref))
  const qc = useQueryClient()
  const restore = useMutation({
    mutationFn: (id: string) => restoreBackup(ref, id, ref.instance),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['sql'] })
      toast.success(`Restoring instance ${ref.instance}. Follow it in Operations.`)
    },
  })
  const del = useMutation({
    mutationFn: (id: string) => deleteBackupRun(ref, id),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['sql'] })
      toast.success('Deleting the backup.')
    },
  })
  const running = isRunning(i)
  const runs = query.data ?? []
  return (
    <div className="flex flex-col gap-4">
      {!running && <NotRunning i={i} what="Backups can be taken and restored" />}
      <Card>
        {running && (
          <div className="flex justify-end">
            <CreateBackupDialog instanceRef={ref} />
          </div>
        )}
        <QueryStatus query={query} />
        {query.data && runs.length === 0 && (
          <p className="text-sm text-muted-foreground">No backups yet.</p>
        )}
        {runs.length > 0 && (
          <Table aria-label="Backups">
            <TableHeader>
              <TableRow>
                <TableHead>ID</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Description</TableHead>
                <TableHead>Started</TableHead>
                <TableHead>Finished</TableHead>
                <TableHead>
                  <span className="sr-only">Actions</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {runs.map((b) => (
                <TableRow key={b.id} data-testid="backup">
                  <TableCell className="font-mono">{b.id}</TableCell>
                  <TableCell>
                    <StatusBadge
                      status={b.status ?? 'SQL_BACKUP_RUN_STATUS_UNSPECIFIED'}
                      tone={TONES[b.status ?? ''] ?? 'default'}
                    />
                  </TableCell>
                  <TableCell>{b.type ?? '—'}</TableCell>
                  <TableCell>{b.description || '—'}</TableCell>
                  <TableCell>{time(b.startTime ?? b.enqueuedTime)}</TableCell>
                  <TableCell>{time(b.endTime)}</TableCell>
                  <TableCell>
                    <div className="flex justify-end gap-2">
                      <ConfirmDialog
                        trigger={
                          <Button
                            variant="outline"
                            size="sm"
                            disabled={!running || b.status !== 'SUCCESSFUL'}
                            aria-label={`Restore backup ${b.id}`}
                          >
                            <ArchiveRestore aria-hidden />
                            Restore
                          </Button>
                        }
                        title={`Restore backup ${b.id}?`}
                        description={`Every database and user of ${ref.instance} is replaced by the backup's. Changes since ${time(b.endTime)} are lost.`}
                        confirmLabel="Restore"
                        onConfirm={() => restore.mutateAsync(b.id)}
                      />
                      <ConfirmDialog
                        trigger={
                          <Button variant="outline" size="sm" aria-label={`Delete backup ${b.id}`}>
                            <Trash2 aria-hidden />
                            Delete
                          </Button>
                        }
                        title={`Delete backup ${b.id}?`}
                        description="It can no longer be restored. This cannot be undone."
                        confirmLabel="Delete backup"
                        onConfirm={() => del.mutateAsync(b.id)}
                      />
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  )
}
