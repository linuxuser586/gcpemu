import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Download, Plus, RotateCcw, Trash2, Upload } from 'lucide-react'
import { useId, useState } from 'react'

import { errorMessage } from '@/api/errors'
import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { Textarea } from '@/components/ui/textarea'
import { bytes } from '@/features/cdn/api'
import { serviceName } from '@/lib/services'
import { toast } from '@/lib/toast'

import {
  applySeed,
  deleteSnapshot,
  exportSeed,
  resetInstance,
  restoreSnapshot,
  snapshotsQuery,
  type Snapshot,
} from './api'

const time = (iso: string) => {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}

function SnapshotRow({ s }: { s: Snapshot }) {
  const qc = useQueryClient()
  const restore = useMutation({
    mutationFn: () => restoreSnapshot(s.name),
    onSuccess: () => {
      // The event stream's reset event refetches the other views.
      void qc.invalidateQueries()
      toast.success(`Restored State snapshot ${s.name}.`)
    },
  })
  const remove = useMutation({
    mutationFn: () => deleteSnapshot(s.name),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: snapshotsQuery().queryKey })
      toast.success(`Deleted State snapshot ${s.name}.`)
    },
  })
  return (
    <TableRow data-testid="snapshot" data-name={s.name}>
      <TableCell className="font-mono">{s.name}</TableCell>
      <TableCell>{time(s.createTime)}</TableCell>
      <TableCell className="text-right tabular-nums">{bytes(s.sizeBytes)}</TableCell>
      <TableCell className="text-sm">{s.services.map(serviceName).join(', ')}</TableCell>
      <TableCell className="text-right whitespace-nowrap">
        <ConfirmDialog
          trigger={
            <Button variant="outline" size="sm" aria-label={`Restore ${s.name}`}>
              <RotateCcw aria-hidden />
              Restore
            </Button>
          }
          title={`Restore State snapshot ${s.name}?`}
          description="All of the Instance's state is replaced with the snapshot's, undoing everything done since it was saved. Configuration, Fault rules and the Emulator clock are kept."
          confirmLabel="Restore"
          onConfirm={() => restore.mutateAsync()}
        />
        <ConfirmDialog
          trigger={
            <Button variant="ghost" size="icon" aria-label={`Delete ${s.name}`}>
              <Trash2 />
            </Button>
          }
          title={`Delete State snapshot ${s.name}?`}
          description="The snapshot's files are removed. The Instance's state is not affected."
          confirmLabel="Delete"
          onConfirm={() => remove.mutateAsync()}
        />
      </TableCell>
    </TableRow>
  )
}

function Snapshots() {
  const query = useQuery(snapshotsQuery())
  const all = query.data ?? []
  return (
    <Card>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold">State snapshots</h2>
          <p className="text-sm text-muted-foreground">
            Named, saved copies of all of the Instance&apos;s state, including object data and Cloud
            SQL databases, to restore later.
          </p>
        </div>
        <Button asChild>
          <Link to="/state/snapshots/save">
            <Plus aria-hidden />
            Save snapshot
          </Link>
        </Button>
      </div>
      <QueryStatus query={query} />
      {query.data && all.length === 0 && (
        <p className="text-sm text-muted-foreground" role="status">
          No State snapshots saved.
        </p>
      )}
      {all.length > 0 && (
        <Table aria-label="State snapshots">
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Saved</TableHead>
              <TableHead className="text-right">Size</TableHead>
              <TableHead>Services</TableHead>
              <TableHead>
                <span className="sr-only">Actions</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {all.map((s) => (
              <SnapshotRow key={s.name} s={s} />
            ))}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}

function Reset() {
  const qc = useQueryClient()
  const reset = useMutation({
    mutationFn: resetInstance,
    onSuccess: () => {
      void qc.invalidateQueries()
      toast.success('Reset the Instance.')
    },
  })
  return (
    <Card>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold">Reset</h2>
          <p className="text-sm text-muted-foreground">
            Removes all resources and data, keeping the configuration. It does not re-apply the seed
            file; restore a State snapshot to get back to a known state.
          </p>
        </div>
        <ConfirmDialog
          trigger={<Button variant="destructive">Reset Instance</Button>}
          title="Reset the Instance?"
          description="Every resource and all data in every Project are removed, including Cloud SQL databases and GKE clusters. State snapshots are kept."
          confirmLabel="Reset"
          onConfirm={() => reset.mutateAsync()}
        />
      </div>
    </Card>
  )
}

/** download saves text as a file through a temporary link. */
function download(text: string, name: string) {
  const url = URL.createObjectURL(new Blob([text], { type: 'application/yaml' }))
  const a = document.createElement('a')
  a.href = url
  a.download = name
  document.body.append(a)
  a.click()
  a.remove()
  URL.revokeObjectURL(url)
}

function Seed() {
  const id = useId()
  const [yaml, setYaml] = useState('')
  const [error, setError] = useState('')
  const apply = useMutation({
    meta: { toast: false },
    mutationFn: applySeed,
    onSuccess: () => {
      setError('')
      toast.success('Applied the seed file.')
    },
    onError: (e) => setError(errorMessage(e)),
  })
  const exp = useMutation({
    mutationFn: exportSeed,
    onSuccess: (text) => download(text, 'seed.yaml'),
  })
  return (
    <Card>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold">Seed file</h2>
          <p className="text-sm text-muted-foreground">
            A declarative file of Projects, resources and Fault rules. Applying one is idempotent:
            what already exists is kept.
          </p>
        </div>
        <Button variant="outline" disabled={exp.isPending} onClick={() => exp.mutate()}>
          <Download aria-hidden />
          Export seed file
        </Button>
      </div>
      <form
        aria-label="Import seed file"
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          apply.mutate(yaml)
        }}
      >
        <div className="flex flex-col gap-1">
          <label htmlFor={`${id}-file`} className="text-sm font-medium">
            Seed file
          </label>
          <input
            id={`${id}-file`}
            type="file"
            accept=".yaml,.yml,application/yaml,text/yaml"
            className="text-sm"
            onChange={(e) => {
              const f = e.target.files?.[0]
              if (f) void f.text().then(setYaml)
            }}
          />
        </div>
        <div className="flex flex-col gap-1">
          <label htmlFor={`${id}-yaml`} className="text-sm font-medium">
            YAML
          </label>
          <Textarea
            id={`${id}-yaml`}
            rows={10}
            spellCheck={false}
            className="font-mono text-xs"
            placeholder={
              'projects: [my-project]\ngcs:\n  buckets:\n    - name: my-bucket\n      project: my-project'
            }
            aria-describedby={`${id}-hint`}
            aria-invalid={!!error || undefined}
            value={yaml}
            onChange={(e) => setYaml(e.target.value)}
          />
          <p id={`${id}-hint`} className="text-xs text-muted-foreground">
            Sent to <span className="font-mono">POST /_emu/v1/seed</span>. Relative paths resolve
            against the instance dir.
          </p>
        </div>
        {error && (
          <p role="alert" className="text-sm text-destructive">
            {error}
          </p>
        )}
        <div>
          <Button type="submit" disabled={!yaml.trim() || apply.isPending}>
            <Upload aria-hidden />
            Apply seed file
          </Button>
        </div>
      </form>
    </Card>
  )
}

/**
 * State saves and restores State snapshots, resets the Instance, and
 * imports and exports seed files (FR-UI-017).
 */
export function State() {
  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-2xl font-semibold">State</h1>
        <p className="text-sm text-muted-foreground">
          Save, restore and reset all of this Instance&apos;s state, for every Project.
        </p>
      </div>
      <Snapshots />
      <Reset />
      <Seed />
    </div>
  )
}
