import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Pencil, Plus, Scaling, Trash2 } from 'lucide-react'
import { useState } from 'react'
import { useForm } from 'react-hook-form'
import { useParams } from 'react-router'
import { z } from 'zod'

import { errorMessage } from '@/api/errors'
import { Link, useCarriedNavigate } from '@/components/Link'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Field, fieldAria } from '@/components/resource/Field'
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

import { deleteNodePool, setNodePoolSize, type ClusterRef, type NodePool } from './api'
import { useCluster } from './ClusterPage'
import { StatusBadge } from './format'
import { clusterPath, useClusterRef } from './GkeLayout'
import { NodesTable } from './Kube'
import { formatTaints } from './taints'

const POOL_TONES: Record<string, 'success' | 'warning' | 'destructive'> = {
  RUNNING: 'success',
  PROVISIONING: 'warning',
  RECONCILING: 'warning',
  STOPPING: 'warning',
  RUNNING_WITH_ERROR: 'destructive',
  ERROR: 'destructive',
}

function PoolStatus({ np }: { np: NodePool }) {
  const s = np.status ?? 'STATUS_UNSPECIFIED'
  return <StatusBadge status={s} tone={POOL_TONES[s] ?? 'default'} />
}

const zonesOf = (np: NodePool, fallback?: string[]) => np.locations ?? fallback ?? []

const resizeSchema = z.object({
  nodeCount: z
    .number({ error: 'node_count must be a whole number.' })
    .int('node_count must be a whole number.')
    .min(0, 'node_count must be non-negative.'),
})

/**
 * ResizeDialog sets a node pool's size per zone (setSize), which adds or
 * removes node containers (FR-GKE-003).
 */
export function ResizeDialog({ clusterRef, pool }: { clusterRef: ClusterRef; pool: NodePool }) {
  const [open, setOpen] = useState(false)
  const qc = useQueryClient()
  const form = useForm<z.infer<typeof resizeSchema>>({
    resolver: zodResolver(resizeSchema),
    defaultValues: { nodeCount: pool.initialNodeCount ?? 0 },
  })
  const [serverError, setServerError] = useState<string>()
  const resize = useMutation({
    meta: { toast: false },
    mutationFn: (n: number) => setNodePoolSize(clusterRef, pool.name, n),
    onSuccess: (_op, n) => {
      void qc.invalidateQueries({ queryKey: ['gke'] })
      toast.success(`Resizing node pool ${pool.name} to ${n} node${n === 1 ? '' : 's'} per zone.`)
      setOpen(false)
    },
    onError: (e) => setServerError(errorMessage(e)),
  })
  const error = form.formState.errors.nodeCount?.message
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        setOpen(o)
        if (o) {
          form.reset({ nodeCount: pool.initialNodeCount ?? 0 })
          setServerError(undefined)
        }
      }}
    >
      <DialogTrigger asChild>
        <Button variant="outline" size="sm" aria-label={`Resize ${pool.name}`}>
          <Scaling aria-hidden />
          Resize
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogTitle>Resize node pool {pool.name}</DialogTitle>
        <DialogDescription>
          Nodes per zone. Each node is a container; the Instance runs at most a fixed number of
          nodes in all.
        </DialogDescription>
        <form
          noValidate
          className="flex flex-col gap-4"
          onSubmit={(e) => void form.handleSubmit(({ nodeCount }) => resize.mutate(nodeCount))(e)}
        >
          <Field id="resize-count" label="Number of nodes" error={error}>
            <Input
              type="number"
              min={0}
              {...fieldAria('resize-count', error)}
              {...form.register('nodeCount', { valueAsNumber: true })}
            />
          </Field>
          {serverError && (
            <p role="alert" className="text-sm text-destructive">
              {serverError}
            </p>
          )}
          <DialogFooter>
            <DialogClose asChild>
              <Button type="button" variant="outline">
                Cancel
              </Button>
            </DialogClose>
            <Button type="submit" disabled={resize.isPending}>
              Resize
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/** NodePools lists the cluster's node pools. */
export function NodePools() {
  const c = useCluster()
  const ref = useClusterRef()
  const base = clusterPath(ref.location, ref.cluster)
  const pools = c.nodePools ?? []
  return (
    <Card>
      <div className="flex justify-end">
        <Button asChild size="sm">
          <Link to={`${base}/nodePools/create`}>
            <Plus aria-hidden />
            Add node pool
          </Link>
        </Button>
      </div>
      {pools.length === 0 && <p className="text-sm text-muted-foreground">No node pools.</p>}
      {pools.length > 0 && (
        <Table aria-label="Node pools">
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Status</TableHead>
              <TableHead className="text-right">Nodes per zone</TableHead>
              <TableHead>Zones</TableHead>
              <TableHead>Machine type</TableHead>
              <TableHead>Version</TableHead>
              <TableHead>
                <span className="sr-only">Actions</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {pools.map((np) => (
              <TableRow key={np.name} data-testid="node-pool">
                <TableCell>
                  <Link
                    className="font-mono text-primary underline-offset-4 hover:underline"
                    to={`${base}/nodePools/${np.name}`}
                  >
                    {np.name}
                  </Link>
                </TableCell>
                <TableCell>
                  <PoolStatus np={np} />
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  {np.initialNodeCount ?? 0}
                </TableCell>
                <TableCell className="font-mono">{zonesOf(np, c.locations).join(', ')}</TableCell>
                <TableCell className="font-mono">{np.config?.machineType ?? '—'}</TableCell>
                <TableCell className="font-mono">{np.version ?? '—'}</TableCell>
                <TableCell>
                  <ResizeDialog clusterRef={ref} pool={np} />
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}

/** NodePoolPage is one node pool: its fields, actions and nodes. */
export function NodePoolPage() {
  const c = useCluster()
  const ref = useClusterRef()
  const { pool = '' } = useParams()
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const base = clusterPath(ref.location, ref.cluster)
  const del = useMutation({
    mutationFn: () => deleteNodePool(ref, pool),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['gke'] })
      toast.success(`Deleting node pool ${pool}.`)
      navigate(`${base}/nodePools`)
    },
  })
  const np = c.nodePools?.find((p) => p.name === pool)
  if (!np) {
    return (
      <Card role="status">
        <p className="text-sm text-muted-foreground">
          Cluster {ref.cluster} has no node pool <span className="font-mono">{pool}</span>.
        </p>
      </Card>
    )
  }
  const labels = Object.entries(np.config?.labels ?? {})
  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 className="flex items-center gap-3 text-lg font-semibold">
          Node pool <span className="font-mono">{np.name}</span>
          <PoolStatus np={np} />
        </h2>
        <div className="flex flex-wrap gap-2">
          <ResizeDialog clusterRef={ref} pool={np} />
          <Button variant="outline" size="sm" asChild>
            <Link to={`${base}/nodePools/${np.name}/edit`}>
              <Pencil aria-hidden />
              Edit
            </Link>
          </Button>
          <ConfirmDialog
            trigger={
              <Button variant="outline" size="sm">
                <Trash2 aria-hidden />
                Delete
              </Button>
            }
            title={`Delete node pool ${np.name}?`}
            description="Its nodes are drained and their containers removed."
            confirmLabel="Delete node pool"
            onConfirm={() => del.mutateAsync()}
          />
        </div>
      </div>
      {np.statusMessage && (
        <p role="alert" className="text-sm text-destructive">
          {np.statusMessage}
        </p>
      )}
      <Card>
        <CardTitle>Details</CardTitle>
        <DetailList
          label="Node pool details"
          rows={[
            ['Nodes per zone', String(np.initialNodeCount ?? 0)],
            ['Zones', <Mono>{zonesOf(np, c.locations).join(', ')}</Mono>],
            ['Version', np.version && <Mono>{np.version}</Mono>],
            ['Machine type', np.config?.machineType && <Mono>{np.config.machineType}</Mono>],
            ['Disk size', np.config?.diskSizeGb ? `${np.config.diskSizeGb} GB` : undefined],
            ['Image type', np.config?.imageType && <Mono>{np.config.imageType}</Mono>],
            [
              'Service account',
              np.config?.serviceAccount && <Mono>{np.config.serviceAccount}</Mono>,
            ],
            [
              'Kubernetes labels',
              labels.length > 0 && <Mono>{labels.map(([k, v]) => `${k}=${v}`).join(', ')}</Mono>,
            ],
            [
              'Taints',
              np.config?.taints?.length ? (
                <Mono>{formatTaints(np.config.taints).replaceAll('\n', ', ')}</Mono>
              ) : undefined,
            ],
            [
              'Autoscaling',
              np.autoscaling?.enabled
                ? `${np.autoscaling.minNodeCount ?? 0} to ${np.autoscaling.maxNodeCount ?? 0} nodes (stored)`
                : undefined,
            ],
          ]}
        />
      </Card>
      <Card>
        <CardTitle>Nodes</CardTitle>
        <NodesTable pool={np.name} />
      </Card>
      <Card>
        <CardTitle>Node pool resource</CardTitle>
        <JsonView value={np} label="Node pool JSON" />
      </Card>
    </div>
  )
}
