import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm } from 'react-hook-form'
import { useParams } from 'react-router'
import { z } from 'zod'

import { useCarriedNavigate } from '@/components/Link'
import { Field, fieldAria } from '@/components/resource/Field'
import { ResourceEditor, type Body } from '@/components/resource/ResourceEditor'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import { formatLabels, labelsError, parseLabels } from '@/lib/labels'
import { toast } from '@/lib/toast'

import {
  createNodePool,
  NAME,
  nameMessage,
  serverConfigQuery,
  updateNodePool,
  waitOperation,
  type ClusterRef,
  type NodePool,
} from './api'
import { useCluster } from './ClusterPage'
import { clusterPath, useClusterRef } from './GkeLayout'
import { formatTaints, parseTaints, taintsError } from './taints'

// Create and edit forms for node pools (FR-GKE-003, FR-UI-011): create
// sends nodePools.create's {nodePool}; edit sends nodePools.update's
// request (labels, taints, machine type), then a node version upgrade.

type Obj = Record<string, unknown>
const obj = (v: unknown): Obj =>
  typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Obj) : {}

const labelsField = z.string().superRefine((v, ctx) => {
  const err = labelsError(v)
  if (err) ctx.addIssue({ code: 'custom', message: err })
})
const taintsField = z.string().superRefine((v, ctx) => {
  const err = taintsError(v)
  if (err) ctx.addIssue({ code: 'custom', message: err })
})

function LabelsAndTaints({
  form,
  prefix,
}: {
  form: {
    register: (n: 'labels' | 'taints') => object
    errors: { labels?: string; taints?: string }
  }
  prefix: string
}) {
  return (
    <>
      <Field
        id={`${prefix}-labels`}
        label="Kubernetes labels"
        error={form.errors.labels}
        hint="key=value, one per line; set on every node"
      >
        <Textarea
          rows={3}
          spellCheck={false}
          className="font-mono"
          {...fieldAria(`${prefix}-labels`, form.errors.labels, 'key=value')}
          {...form.register('labels')}
        />
      </Field>
      <Field
        id={`${prefix}-taints`}
        label="Taints"
        error={form.errors.taints}
        hint="key=value:Effect, one per line (NoSchedule, PreferNoSchedule or NoExecute)"
      >
        <Textarea
          rows={2}
          spellCheck={false}
          className="font-mono"
          {...fieldAria(`${prefix}-taints`, form.errors.taints, 'key=value:Effect')}
          {...form.register('taints')}
        />
      </Field>
    </>
  )
}

// ---- create ----

const createSchema = z.object({
  name: z.string().superRefine((v, ctx) => {
    if (!NAME.test(v)) ctx.addIssue({ code: 'custom', message: nameMessage('nodePool', v) })
  }),
  nodeCount: z
    .number({ error: 'The number of nodes must be a whole number.' })
    .int('The number of nodes must be a whole number.')
    .min(0, 'initial_node_count must be non-negative.'),
  machineType: z.string().trim(),
  labels: labelsField,
  taints: taintsField,
})
type CreateValues = z.infer<typeof createSchema>

export function nodePoolBody(v: CreateValues, base: Body): Body {
  const np = { ...obj(base.nodePool) }
  np.name = v.name
  np.initialNodeCount = v.nodeCount
  const cfg = { ...obj(np.config) }
  if (v.machineType) cfg.machineType = v.machineType
  else delete cfg.machineType
  const labels = parseLabels(v.labels)
  if (Object.keys(labels).length) cfg.labels = labels
  else delete cfg.labels
  const taints = parseTaints(v.taints)
  if (taints.length) cfg.taints = taints
  else delete cfg.taints
  np.config = cfg
  return { ...base, nodePool: np }
}

export function nodePoolValues(body: Body): CreateValues {
  const np = obj(body.nodePool)
  const cfg = obj(np.config)
  return {
    name: typeof np.name === 'string' ? np.name : '',
    nodeCount: typeof np.initialNodeCount === 'number' ? np.initialNodeCount : 0,
    machineType: typeof cfg.machineType === 'string' ? cfg.machineType : '',
    labels: formatLabels(obj(cfg.labels) as Record<string, string>),
    taints: formatTaints(Array.isArray(cfg.taints) ? (cfg.taints as never) : []),
  }
}

export function CreateNodePool() {
  const ref = useClusterRef()
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const base = clusterPath(ref.location, ref.cluster)
  const defaults: CreateValues = {
    name: '',
    nodeCount: 1,
    machineType: 'e2-medium',
    labels: '',
    taints: '',
  }
  const form = useForm<CreateValues>({
    resolver: zodResolver(createSchema),
    defaultValues: defaults,
  })
  const errors = form.formState.errors
  const create = useMutation({
    meta: { toast: false },
    mutationFn: (body: Body) => createNodePool(ref, body),
    onSuccess: (_op, body) => {
      const name = obj(body.nodePool).name as string
      void qc.invalidateQueries({ queryKey: ['gke'] })
      toast.success(`Creating node pool ${name}.`)
      navigate(`${base}/nodePools/${name}`)
    },
  })
  return (
    <Card>
      <h2 className="text-lg font-semibold">Add node pool</h2>
      <ResourceEditor
        form={form}
        initialBody={nodePoolBody(defaults, { nodePool: {} })}
        toBody={nodePoolBody}
        fromBody={(body) => nodePoolValues(body)}
        fields={['name', 'nodeCount']}
        onSubmit={(body) => create.mutateAsync(body)}
        submitLabel="Create"
        onCancel={() => navigate(`${base}/nodePools`)}
        jsonHint={
          <>
            Sent to <span className="font-mono">projects.locations.clusters.nodePools.create</span>.
          </>
        }
      >
        <Field id="pool-name" label="Name" error={errors.name?.message}>
          <Input
            autoComplete="off"
            spellCheck={false}
            {...fieldAria('pool-name', errors.name?.message)}
            {...form.register('name')}
          />
        </Field>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field
            id="pool-nodes"
            label="Nodes per zone"
            error={errors.nodeCount?.message}
            hint="Each node is a container."
          >
            <Input
              type="number"
              min={0}
              {...fieldAria('pool-nodes', errors.nodeCount?.message, 'Each')}
              {...form.register('nodeCount', { valueAsNumber: true })}
            />
          </Field>
          <Field id="pool-machine" label="Machine type" hint="Stored; nodes use the host.">
            <Input
              spellCheck={false}
              {...fieldAria('pool-machine', undefined, 'Stored')}
              {...form.register('machineType')}
            />
          </Field>
        </div>
        <LabelsAndTaints
          prefix="pool"
          form={{
            register: (n) => form.register(n),
            errors: { labels: errors.labels?.message, taints: errors.taints?.message },
          }}
        />
      </ResourceEditor>
    </Card>
  )
}

// ---- edit ----

const editSchema = z.object({
  machineType: z.string().trim(),
  nodeVersion: z.string(),
  labels: labelsField,
  taints: taintsField,
})
type EditValues = z.infer<typeof editSchema>

function editDefaults(np: NodePool): EditValues {
  return {
    machineType: np.config?.machineType ?? '',
    nodeVersion: '',
    labels: formatLabels(np.config?.labels),
    taints: formatTaints(np.config?.taints),
  }
}

/** updatePoolBody writes what the edit form changed onto a nodePools.update body. */
export function updatePoolBody(np: NodePool, v: EditValues, base: Body): Body {
  const out = { ...base }
  const was = editDefaults(np)
  const set = (k: string, changed: boolean, value: unknown) => {
    if (changed) out[k] = value
    else delete out[k]
  }
  set('machineType', v.machineType !== was.machineType, v.machineType)
  set('nodeVersion', !!v.nodeVersion, v.nodeVersion)
  set('labels', v.labels !== was.labels, { labels: parseLabels(v.labels) })
  set('taints', v.taints !== was.taints, { taints: parseTaints(v.taints) })
  return out
}

export function updatePoolValues(np: NodePool) {
  return (body: Body): EditValues => {
    const was = editDefaults(np)
    return {
      machineType: typeof body.machineType === 'string' ? body.machineType : was.machineType,
      nodeVersion: typeof body.nodeVersion === 'string' ? body.nodeVersion : '',
      labels:
        'labels' in body
          ? formatLabels(obj(obj(body.labels).labels) as Record<string, string>)
          : was.labels,
      taints:
        'taints' in body ? formatTaints((obj(body.taints).taints as never) ?? []) : was.taints,
    }
  }
}

/** savePool sends the update, then a node version upgrade once it is done. */
export async function savePool(r: ClusterRef, pool: string, body: Body) {
  const { nodeVersion, ...rest } = body
  const steps: (() => Promise<{ name: string }>)[] = []
  if (Object.keys(rest).length > 0) steps.push(() => updateNodePool(r, pool, rest))
  if (typeof nodeVersion === 'string' && nodeVersion) {
    steps.push(() => updateNodePool(r, pool, { nodeVersion }))
  }
  for (const [i, step] of steps.entries()) {
    const op = await step()
    if (i < steps.length - 1) await waitOperation(r.project, op)
  }
  return steps.length
}

export function EditNodePool() {
  const c = useCluster()
  const ref = useClusterRef()
  const { pool = '' } = useParams()
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
  return <EditNodePoolForm np={np} clusterRef={ref} />
}

function EditNodePoolForm({ np, clusterRef }: { np: NodePool; clusterRef: ClusterRef }) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const sc = useQuery(serverConfigQuery(clusterRef.project, clusterRef.location))
  const form = useForm<EditValues>({
    resolver: zodResolver(editSchema),
    defaultValues: editDefaults(np),
  })
  const errors = form.formState.errors
  const back = () =>
    navigate(`${clusterPath(clusterRef.location, clusterRef.cluster)}/nodePools/${np.name}`)
  const save = useMutation({
    meta: { toast: false },
    mutationFn: (body: Body) => savePool(clusterRef, np.name, body),
    onSuccess: (n) => {
      void qc.invalidateQueries({ queryKey: ['gke'] })
      toast.success(n ? `Updating node pool ${np.name}.` : 'Nothing to change.')
      back()
    },
  })
  return (
    <Card>
      <h2 className="text-lg font-semibold">
        Edit node pool <span className="font-mono">{np.name}</span>
      </h2>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={(v, base) => updatePoolBody(np, v, base)}
        fromBody={updatePoolValues(np)}
        fields={['machineType', 'nodeVersion']}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">projects.locations.clusters.nodePools.update</span>;
            a <span className="font-mono">nodeVersion</span> is sent on its own afterwards.
          </>
        }
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id="edit-pool-machine" label="Machine type" hint="Stored; nodes use the host.">
            <Input
              spellCheck={false}
              {...fieldAria('edit-pool-machine', undefined, 'Stored')}
              {...form.register('machineType')}
            />
          </Field>
          <Field
            id="edit-pool-version"
            label="Node version"
            hint={`Now ${np.version ?? 'unknown'}; nodes may not be newer than the control plane.`}
          >
            <NativeSelect
              {...fieldAria('edit-pool-version', undefined, 'Now')}
              {...form.register('nodeVersion')}
            >
              <option value="">Keep {np.version}</option>
              {sc.data?.validNodeVersions
                ?.filter((v) => v !== np.version)
                .map((v) => (
                  <option key={v} value={v}>
                    {v}
                  </option>
                ))}
            </NativeSelect>
          </Field>
        </div>
        <LabelsAndTaints
          prefix="edit-pool"
          form={{
            register: (n) => form.register(n),
            errors: { labels: errors.labels?.message, taints: errors.taints?.message },
          }}
        />
      </ResourceEditor>
    </Card>
  )
}
