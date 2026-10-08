import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm, useWatch } from 'react-hook-form'
import { z } from 'zod'

import { useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Field, fieldAria } from '@/components/resource/Field'
import { ResourceEditor, type Body } from '@/components/resource/ResourceEditor'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import { formatLabels, labelsError, parseLabels } from '@/lib/labels'
import { toast } from '@/lib/toast'
import { useViewState } from '@/lib/viewState'

import {
  clusterQuery,
  createCluster,
  NAME,
  nameMessage,
  RELEASE_CHANNELS,
  serverConfigQuery,
  setResourceLabels,
  updateCluster,
  waitOperation,
  type Cluster,
  type ClusterRef,
} from './api'
import { clusterPath, useClusterRef } from './GkeLayout'

// Create and edit forms for clusters (FR-GKE-001, FR-UI-011). Create sends
// clusters.create's {cluster}; edit sends clusters.update's {update}
// (ClusterUpdate), with labels set through setResourceLabels first.

type Obj = Record<string, unknown>
const obj = (v: unknown): Obj =>
  typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Obj) : {}

const str = (v: unknown) => (typeof v === 'string' ? v : '')

/** put sets key on o, or removes it when value is undefined. */
function put(o: Obj, key: string, value: unknown) {
  if (value === undefined) delete o[key]
  else o[key] = value
}

const labelsField = z.string().superRefine((v, ctx) => {
  const err = labelsError(v)
  if (err) ctx.addIssue({ code: 'custom', message: err })
})

const DEFAULT_LOCATION = 'us-central1-a'

// ---- create ----

const createSchema = z.object({
  name: z.string().superRefine((v, ctx) => {
    if (!NAME.test(v)) ctx.addIssue({ code: 'custom', message: nameMessage('cluster', v) })
  }),
  location: z.string().trim().min(1, 'A cluster needs a location: a zone or a region.'),
  nodeCount: z
    .number({ error: 'The number of nodes must be a whole number.' })
    .int('The number of nodes must be a whole number.')
    .min(0, 'Node pool "default-pool": initial_node_count must be non-negative.'),
  machineType: z.string().trim(),
  releaseChannel: z.string(),
  version: z.string(),
  workloadIdentity: z.boolean(),
  privateNodes: z.boolean(),
  labels: labelsField,
})
type CreateValues = z.infer<typeof createSchema>

/**
 * createBody writes the create form onto a clusters.create body. The
 * node count and machine type belong to the first node pool when the
 * body lists node pools, and to the default pool's fields otherwise.
 */
export function createBody(project: string, v: CreateValues, base: Body): Body {
  const c = { ...obj(base.cluster) }
  c.name = v.name
  const pools = Array.isArray(c.nodePools) ? (c.nodePools as Obj[]) : undefined
  if (pools?.length) {
    const first = { ...pools[0] }
    first.initialNodeCount = v.nodeCount
    first.config = { ...obj(first.config) }
    put(first.config as Obj, 'machineType', v.machineType || undefined)
    c.nodePools = [first, ...pools.slice(1)]
  } else {
    c.initialNodeCount = v.nodeCount
    c.nodeConfig = { ...obj(c.nodeConfig) }
    put(c.nodeConfig as Obj, 'machineType', v.machineType || undefined)
    if (Object.keys(c.nodeConfig as Obj).length === 0) delete c.nodeConfig
  }
  put(c, 'releaseChannel', v.releaseChannel ? { channel: v.releaseChannel } : undefined)
  put(c, 'initialClusterVersion', v.version || undefined)
  put(
    c,
    'workloadIdentityConfig',
    v.workloadIdentity ? { workloadPool: `${project}.svc.id.goog` } : undefined,
  )
  const pcc = { ...obj(c.privateClusterConfig) }
  put(pcc, 'enablePrivateNodes', v.privateNodes || undefined)
  put(c, 'privateClusterConfig', Object.keys(pcc).length ? pcc : undefined)
  const labels = parseLabels(v.labels)
  put(c, 'resourceLabels', Object.keys(labels).length ? labels : undefined)
  return { ...base, cluster: c }
}

export function createValues(body: Body, prev: CreateValues): CreateValues {
  const c = obj(body.cluster)
  const pools = Array.isArray(c.nodePools) ? (c.nodePools as Obj[]) : []
  const first = pools[0]
  const nodeCount = first ? first.initialNodeCount : c.initialNodeCount
  const cfg = first ? obj(first.config) : obj(c.nodeConfig)
  return {
    name: typeof c.name === 'string' ? c.name : '',
    location: prev.location,
    nodeCount: typeof nodeCount === 'number' ? nodeCount : 0,
    machineType: typeof cfg.machineType === 'string' ? cfg.machineType : '',
    releaseChannel: str(obj(c.releaseChannel).channel),
    version: typeof c.initialClusterVersion === 'string' ? c.initialClusterVersion : '',
    workloadIdentity: !!obj(c.workloadIdentityConfig).workloadPool,
    privateNodes: !!obj(c.privateClusterConfig).enablePrivateNodes,
    labels: formatLabels(obj(c.resourceLabels) as Record<string, string>),
  }
}

function VersionOptions({ project, location }: { project: string; location: string }) {
  const sc = useQuery({ ...serverConfigQuery(project, location), enabled: !!location })
  return (
    <>
      <option value="">
        Default{sc.data?.defaultClusterVersion ? ` (${sc.data.defaultClusterVersion})` : ''}
      </option>
      <option value="latest">latest</option>
      {sc.data?.validMasterVersions?.map((v) => (
        <option key={v} value={v}>
          {v}
        </option>
      ))}
    </>
  )
}

export function CreateCluster() {
  const [view] = useViewState()
  const project = view.project ?? ''
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const defaults: CreateValues = {
    name: '',
    location: view.location || DEFAULT_LOCATION,
    nodeCount: 1,
    machineType: 'e2-medium',
    releaseChannel: '',
    version: '',
    workloadIdentity: true,
    privateNodes: false,
    labels: '',
  }
  const form = useForm<CreateValues>({
    resolver: zodResolver(createSchema),
    defaultValues: defaults,
  })
  const errors = form.formState.errors
  const location = useWatch({ control: form.control, name: 'location' })
  const create = useMutation({
    meta: { toast: false },
    mutationFn: ({ body, values }: { body: Body; values: CreateValues }) =>
      createCluster(project, values.location.trim(), body),
    onSuccess: (_op, { values }) => {
      void qc.invalidateQueries({ queryKey: ['gke'] })
      toast.success(`Creating cluster ${values.name}. Follow it in Operations.`)
      navigate(clusterPath(values.location.trim(), values.name))
    },
  })

  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-2xl font-semibold">Create cluster</h1>
        <p className="text-sm text-muted-foreground">
          A Standard cluster in Project <span className="font-mono">{project}</span>, backed by a
          real k3s cluster.
        </p>
      </div>
      <Card>
        <ResourceEditor
          form={form}
          initialBody={createBody(project, defaults, { cluster: {} })}
          toBody={(v, base) => createBody(project, v, base)}
          fromBody={createValues}
          fields={['name', 'location']}
          onSubmit={(body, values) => create.mutateAsync({ body, values })}
          submitLabel="Create"
          onCancel={() => navigate('/gke')}
          jsonHint={
            <>
              Sent to <span className="font-mono">projects.locations.clusters.create</span> in the
              location the form names.
            </>
          }
        >
          <Field id="cluster-name" label="Name" error={errors.name?.message}>
            <Input
              autoComplete="off"
              spellCheck={false}
              {...fieldAria('cluster-name', errors.name?.message)}
              {...form.register('name')}
            />
          </Field>
          <Field
            id="cluster-location"
            label="Location"
            error={errors.location?.message}
            hint="A zone (us-central1-a) for a zonal cluster, or a region (us-central1) for a regional one."
          >
            <Input
              autoComplete="off"
              spellCheck={false}
              {...fieldAria('cluster-location', errors.location?.message, 'A zone or a region')}
              {...form.register('location')}
            />
          </Field>
          <div className="grid gap-4 sm:grid-cols-2">
            <Field
              id="cluster-nodes"
              label="Nodes in the default pool"
              error={errors.nodeCount?.message}
              hint="Per zone. Each node is a container."
            >
              <Input
                type="number"
                min={0}
                {...fieldAria('cluster-nodes', errors.nodeCount?.message, 'Per zone')}
                {...form.register('nodeCount', { valueAsNumber: true })}
              />
            </Field>
            <Field id="cluster-machine" label="Machine type" hint="Stored; nodes use the host.">
              <Input
                spellCheck={false}
                {...fieldAria('cluster-machine', undefined, 'Stored')}
                {...form.register('machineType')}
              />
            </Field>
          </div>
          <div className="grid gap-4 sm:grid-cols-2">
            <Field id="cluster-channel" label="Release channel">
              <NativeSelect id="cluster-channel" {...form.register('releaseChannel')}>
                <option value="">Default (REGULAR)</option>
                {RELEASE_CHANNELS.map((c) => (
                  <option key={c} value={c}>
                    {c}
                  </option>
                ))}
              </NativeSelect>
            </Field>
            <Field id="cluster-version" label="Version">
              <NativeSelect id="cluster-version" {...form.register('version')}>
                <VersionOptions project={project} location={location.trim()} />
              </NativeSelect>
            </Field>
          </div>
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" className="size-4" {...form.register('workloadIdentity')} />
            Workload Identity (<span className="font-mono">{project}.svc.id.goog</span>)
          </label>
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" className="size-4" {...form.register('privateNodes')} />
            Private nodes (egress only through Cloud NAT)
          </label>
          <Field
            id="cluster-labels"
            label="Labels"
            error={errors.labels?.message}
            hint="key=value, one per line"
          >
            <Textarea
              rows={3}
              spellCheck={false}
              className="font-mono"
              {...fieldAria('cluster-labels', errors.labels?.message, 'key=value')}
              {...form.register('labels')}
            />
          </Field>
        </ResourceEditor>
      </Card>
    </div>
  )
}

// ---- edit ----

const editSchema = z.object({
  releaseChannel: z.string(),
  masterVersion: z.string(),
  workloadIdentity: z.boolean(),
  loggingService: z.string().trim(),
  monitoringService: z.string().trim(),
  labels: labelsField,
})
type EditValues = z.infer<typeof editSchema>

function editDefaults(c: Cluster): EditValues {
  return {
    releaseChannel: c.releaseChannel?.channel ?? 'UNSPECIFIED',
    masterVersion: '',
    workloadIdentity: !!c.workloadIdentityConfig?.workloadPool,
    loggingService: c.loggingService ?? '',
    monitoringService: c.monitoringService ?? '',
    labels: formatLabels(c.resourceLabels),
  }
}

/** updateBody writes what the edit form changed onto a clusters.update body. */
export function updateBody(project: string, c: Cluster, v: EditValues, base: Body): Body {
  const u = { ...obj(base.update) }
  const was = editDefaults(c)
  if (v.releaseChannel !== was.releaseChannel) {
    u.desiredReleaseChannel = { channel: v.releaseChannel }
  } else delete u.desiredReleaseChannel
  put(u, 'desiredMasterVersion', v.masterVersion || undefined)
  if (v.workloadIdentity !== was.workloadIdentity) {
    u.desiredWorkloadIdentityConfig = {
      workloadPool: v.workloadIdentity ? `${project}.svc.id.goog` : '',
    }
  } else delete u.desiredWorkloadIdentityConfig
  put(
    u,
    'desiredLoggingService',
    v.loggingService !== was.loggingService ? v.loggingService : undefined,
  )
  put(
    u,
    'desiredMonitoringService',
    v.monitoringService !== was.monitoringService ? v.monitoringService : undefined,
  )
  return { ...base, update: u }
}

export function editValues(c: Cluster) {
  return (body: Body, prev: EditValues): EditValues => {
    const u = obj(body.update)
    const was = editDefaults(c)
    const wi = obj(u.desiredWorkloadIdentityConfig)
    return {
      releaseChannel: str(obj(u.desiredReleaseChannel).channel) || was.releaseChannel,
      masterVersion: typeof u.desiredMasterVersion === 'string' ? u.desiredMasterVersion : '',
      workloadIdentity: 'workloadPool' in wi ? !!wi.workloadPool : was.workloadIdentity,
      loggingService:
        typeof u.desiredLoggingService === 'string' ? u.desiredLoggingService : was.loggingService,
      monitoringService:
        typeof u.desiredMonitoringService === 'string'
          ? u.desiredMonitoringService
          : was.monitoringService,
      // Labels are not part of ClusterUpdate.
      labels: prev.labels,
    }
  }
}

const sameLabels = (a: Record<string, string> = {}, b: Record<string, string> = {}) =>
  JSON.stringify(Object.entries(a).sort()) === JSON.stringify(Object.entries(b).sort())

/**
 * saveCluster applies an edit: labels through setResourceLabels, then the
 * ClusterUpdate, then a control plane upgrade, each once the previous
 * Operation is done, since a cluster runs one Operation at a time.
 */
export async function saveCluster(r: ClusterRef, c: Cluster, body: Body, labels: string) {
  const want = parseLabels(labels)
  const { desiredMasterVersion, ...rest } = obj(body.update)
  const steps: (() => Promise<{ name: string }>)[] = []
  if (!sameLabels(want, c.resourceLabels)) {
    steps.push(() => setResourceLabels(r, want, c.labelFingerprint))
  }
  if (Object.keys(rest).length > 0) steps.push(() => updateCluster(r, { ...body, update: rest }))
  if (typeof desiredMasterVersion === 'string' && desiredMasterVersion) {
    steps.push(() => updateCluster(r, { update: { desiredMasterVersion } }))
  }
  for (const [i, step] of steps.entries()) {
    const op = await step()
    // The last one (perhaps a long upgrade) is followed in Operations.
    if (i < steps.length - 1) await waitOperation(r.project, op)
  }
  return steps.length
}

export function EditCluster() {
  const ref = useClusterRef()
  const query = useQuery(clusterQuery(ref))
  return (
    <div className="flex flex-col gap-6">
      <h1 className="text-2xl font-semibold">
        Edit cluster <span className="font-mono">{ref.cluster}</span>
      </h1>
      <QueryStatus query={query} />
      {query.data && <EditClusterForm cluster={query.data} clusterRef={ref} />}
    </div>
  )
}

function EditClusterForm({ cluster, clusterRef }: { cluster: Cluster; clusterRef: ClusterRef }) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const defaults = editDefaults(cluster)
  const form = useForm<EditValues>({ resolver: zodResolver(editSchema), defaultValues: defaults })
  const errors = form.formState.errors
  const back = () => navigate(clusterPath(clusterRef.location, clusterRef.cluster))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: ({ body, values }: { body: Body; values: EditValues }) =>
      saveCluster(clusterRef, cluster, body, values.labels),
    onSuccess: (n) => {
      void qc.invalidateQueries({ queryKey: ['gke'] })
      toast.success(n ? `Updating cluster ${cluster.name}.` : 'Nothing to change.')
      back()
    },
  })
  return (
    <Card>
      <ResourceEditor
        form={form}
        initialBody={{ update: {} }}
        toBody={(v, base) => updateBody(clusterRef.project, cluster, v, base)}
        fromBody={editValues(cluster)}
        fields={['releaseChannel', 'masterVersion', 'loggingService', 'monitoringService']}
        onSubmit={(body, values) => save.mutateAsync({ body, values })}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">projects.locations.clusters.update</span>. Labels
            are set with <span className="font-mono">setResourceLabels</span>: edit them in the
            form.
          </>
        }
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id="edit-channel" label="Release channel">
            <NativeSelect id="edit-channel" {...form.register('releaseChannel')}>
              {RELEASE_CHANNELS.map((c) => (
                <option key={c} value={c}>
                  {c}
                </option>
              ))}
            </NativeSelect>
          </Field>
          <Field
            id="edit-version"
            label="Control plane version"
            hint={`Now ${cluster.currentMasterVersion ?? 'unknown'}; upgrades go one minor version at a time.`}
          >
            <NativeSelect
              {...fieldAria('edit-version', undefined, 'Now')}
              {...form.register('masterVersion')}
            >
              <option value="">Keep {cluster.currentMasterVersion}</option>
              <VersionChoices clusterRef={clusterRef} current={cluster.currentMasterVersion} />
            </NativeSelect>
          </Field>
        </div>
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" className="size-4" {...form.register('workloadIdentity')} />
          Workload Identity (<span className="font-mono">{clusterRef.project}.svc.id.goog</span>)
        </label>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id="edit-logging" label="Logging service" error={errors.loggingService?.message}>
            <Input
              spellCheck={false}
              {...fieldAria('edit-logging')}
              {...form.register('loggingService')}
            />
          </Field>
          <Field
            id="edit-monitoring"
            label="Monitoring service"
            error={errors.monitoringService?.message}
          >
            <Input
              spellCheck={false}
              {...fieldAria('edit-monitoring')}
              {...form.register('monitoringService')}
            />
          </Field>
        </div>
        <Field
          id="edit-labels"
          label="Labels"
          error={errors.labels?.message}
          hint="key=value, one per line"
        >
          <Textarea
            rows={3}
            spellCheck={false}
            className="font-mono"
            {...fieldAria('edit-labels', errors.labels?.message, 'key=value')}
            {...form.register('labels')}
          />
        </Field>
      </ResourceEditor>
    </Card>
  )
}

function VersionChoices({ clusterRef, current }: { clusterRef: ClusterRef; current?: string }) {
  const sc = useQuery(serverConfigQuery(clusterRef.project, clusterRef.location))
  return (
    <>
      {sc.data?.validMasterVersions
        ?.filter((v) => v !== current)
        .map((v) => (
          <option key={v} value={v}>
            {v}
          </option>
        ))}
    </>
  )
}
