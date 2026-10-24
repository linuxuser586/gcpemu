import { http, HttpResponse } from 'msw'

import type { Container, External, Info, Operation, Readiness, ResourceCounts } from '@/api/admin'
import type { Project } from '@/api/queries'
import type { FaultRule } from '@/features/faults/api'
import type { Snapshot } from '@/features/state/api'

// A fake Instance for component tests: tests mutate `fake` to shape what
// /_emu/v1 and Resource Manager answer.

export interface Fake {
  info: Info
  ready: Readiness
  endpoints: Record<string, string>
  containers: Container[] | { status: number; error: string }
  external: External[]
  counts: ResourceCounts
  env: Record<string, string>
  projects: Project[]
  operations: Operation[]
  faults: FaultRule[]
  snapshots: Snapshot[]
  /** seeds are the seed files applied, in order. */
  seeds: string[]
  resets: number
}

export function defaultFake(): Fake {
  return {
    info: {
      instance: 'default',
      id: 'a1b2c3d4e5f6',
      pid: 4242,
      version: 'v1.2.3',
      dir: '/home/dev/.local/state/gcpemu/default',
      ephemeral: false,
      iamMode: 'audit',
      strictProjects: false,
      console: true,
      services: ['iam', 'gcs', 'pubsub'],
      endpoints: { gateway: '127.0.0.1:4510' },
      runtime: { kind: 'docker', version: '27.3.1', reachable: true },
    },
    ready: {
      ready: false,
      services: {
        iam: { ready: true },
        gcs: { ready: true },
        pubsub: { ready: false, reason: 'loading persisted messages' },
      },
    },
    endpoints: { gateway: '127.0.0.1:4510', gcs: '127.0.0.1:4443', pubsub: '127.0.0.1:8085' },
    containers: [],
    external: [],
    counts: {
      iam: { projects: 2, serviceAccounts: 1 },
      gcs: { buckets: 3, objects: 12 },
      pubsub: { topics: 1, subscriptions: 0 },
    },
    env: { STORAGE_EMULATOR_HOST: '127.0.0.1:4443', PUBSUB_EMULATOR_HOST: "127.0.0.1:8085'x" },
    projects: [
      { projectId: 'alpha-project', name: 'projects/111111111111' },
      { projectId: 'beta-project', name: 'projects/222222222222' },
    ],
    operations: [
      {
        service: 'compute',
        name: 'projects/alpha-project/global/operations/operation-3',
        project: 'alpha-project',
        location: 'global',
        type: 'insert',
        target: 'projects/alpha-project/global/networks/vpc-b',
        status: 'RUNNING',
        done: false,
        startTime: '2026-10-08T12:00:03Z',
        operation: { kind: 'compute#operation', name: 'operation-3', status: 'RUNNING' },
      },
      {
        service: 'compute',
        name: 'projects/alpha-project/global/operations/operation-2',
        project: 'alpha-project',
        location: 'global',
        type: 'insert',
        target: 'projects/alpha-project/global/networks/vpc-a',
        status: 'DONE',
        done: true,
        error: { code: 'RESOURCE_ALREADY_EXISTS', message: "The resource 'vpc-a' already exists" },
        startTime: '2026-10-08T12:00:02Z',
        endTime: '2026-10-08T12:00:03.500Z',
        operation: { kind: 'compute#operation', name: 'operation-2', status: 'DONE' },
      },
      {
        service: 'dns',
        name: 'projects/beta-project/managedZones/z/changes/1',
        project: 'beta-project',
        location: 'global',
        type: 'change',
        target: 'projects/beta-project/managedZones/z',
        status: 'done',
        done: true,
        startTime: '2026-10-08T12:00:00Z',
        endTime: '2026-10-08T12:00:00.250Z',
        operation: { kind: 'dns#change', id: '1', status: 'done' },
      },
    ],
    faults: [
      {
        id: 'flaky-publish',
        service: 'pubsub',
        method: '/google.pubsub.v1.Publisher/Publish',
        code: 'UNAVAILABLE',
        probability: 0.25,
        fired: 7,
      },
      {
        id: 'slow-uploads',
        service: 'gcs',
        method: '^POST /upload/',
        latency: '2s',
        count: 3,
        fired: 3,
      },
      { id: 'drop-all', drop: true, disabled: true, fired: 0 },
    ],
    snapshots: [
      {
        name: 'clean',
        createTime: '2026-10-08T11:00:00Z',
        services: ['gcs', 'iam', 'pubsub'],
        sizeBytes: 1536,
      },
    ],
    seeds: [],
    resets: 0,
  }
}

export const fake: Fake = defaultFake()

export function resetFake() {
  Object.assign(fake, defaultFake())
}

export const handlers = [
  http.get('*/_emu/v1/info', () => HttpResponse.json(fake.info)),
  http.get('*/_emu/v1/ready', () =>
    HttpResponse.json(fake.ready, { status: fake.ready.ready ? 200 : 503 }),
  ),
  http.get('*/_emu/v1/endpoints', () => HttpResponse.json(fake.endpoints)),
  http.get('*/_emu/v1/containers', () =>
    Array.isArray(fake.containers)
      ? HttpResponse.json({ containers: fake.containers })
      : HttpResponse.json({ error: fake.containers.error }, { status: fake.containers.status }),
  ),
  http.get('*/_emu/v1/external', () => HttpResponse.json({ external: fake.external })),
  http.get('*/_emu/v1/resources/counts', () => HttpResponse.json(fake.counts)),
  http.get('*/_emu/v1/env', () => HttpResponse.json(fake.env)),
  http.get('*/_emu/v1/operations', ({ request }) => {
    const project = new URL(request.url).searchParams.get('project')
    return HttpResponse.json({
      operations: fake.operations.filter((op) => !project || op.project === project),
    })
  }),
  http.get('*/_emu/v1/faults', () => HttpResponse.json({ faults: fake.faults })),
  http.post('*/_emu/v1/faults', async ({ request }) => {
    const rule = (await request.json()) as FaultRule
    if (!rule.code && !rule.latency && !rule.drop) {
      return HttpResponse.json({ error: 'rule needs code, latency or drop' }, { status: 400 })
    }
    const added = { ...rule, id: rule.id || `fault-${fake.faults.length + 1}`, fired: 0 }
    fake.faults = [...fake.faults, added]
    return HttpResponse.json(added)
  }),
  http.patch('*/_emu/v1/faults/:id', async ({ params, request }) => {
    const { disabled } = (await request.json()) as { disabled: boolean }
    const rule = fake.faults.find((r) => r.id === params.id)
    if (!rule) return HttpResponse.json({ error: 'no such fault' }, { status: 404 })
    rule.disabled = disabled
    return HttpResponse.json(rule)
  }),
  http.delete('*/_emu/v1/faults/:id', ({ params }) => {
    fake.faults = fake.faults.filter((r) => r.id !== params.id)
    return HttpResponse.json({ status: 'removed' })
  }),
  http.delete('*/_emu/v1/faults', () => {
    fake.faults = []
    return HttpResponse.json({ status: 'cleared' })
  }),
  http.get('*/_emu/v1/snapshots', () => HttpResponse.json({ snapshots: fake.snapshots })),
  http.post('*/_emu/v1/snapshots', async ({ request }) => {
    const { name } = (await request.json()) as { name: string }
    if (name === 'busy') {
      return HttpResponse.json(
        { error: 'Instance busy: Operation operation-1 of sql is running; wait for it to finish' },
        { status: 409 },
      )
    }
    const saved: Snapshot = {
      name,
      createTime: '2026-10-08T12:30:00Z',
      services: fake.info.services ?? [],
      sizeBytes: 2048,
    }
    fake.snapshots = [...fake.snapshots.filter((s) => s.name !== name), saved].sort((a, b) =>
      a.name.localeCompare(b.name),
    )
    return HttpResponse.json(saved)
  }),
  http.post('*/_emu/v1/snapshots/:name/restore', ({ params }) =>
    fake.snapshots.some((s) => s.name === params.name)
      ? HttpResponse.json({ status: 'restored' })
      : HttpResponse.json(
          { error: `State snapshot not found: ${String(params.name)}` },
          { status: 404 },
        ),
  ),
  http.delete('*/_emu/v1/snapshots/:name', ({ params }) => {
    fake.snapshots = fake.snapshots.filter((s) => s.name !== params.name)
    return HttpResponse.json({ status: 'deleted' })
  }),
  http.post('*/_emu/v1/reset', () => {
    fake.resets++
    return HttpResponse.json({ status: 'reset' })
  }),
  http.post('*/_emu/v1/seed', async ({ request }) => {
    const text = await request.text()
    if (text.includes('nope:')) {
      return HttpResponse.json(
        { error: 'seed section "nope": service not running' },
        { status: 400 },
      )
    }
    fake.seeds = [...fake.seeds, text]
    return HttpResponse.json({ status: 'seeded' })
  }),
  http.get(
    '*/_emu/v1/seed',
    () =>
      new HttpResponse('projects:\n  - alpha-project\n', {
        headers: { 'Content-Type': 'application/yaml' },
      }),
  ),
  http.get('*/cloudresourcemanager/v3/projects\\:search', () =>
    HttpResponse.json({ projects: fake.projects }),
  ),
  // The OAuth2 token endpoint names the token after the Principal.
  http.post('*/oauth2/token', async ({ request }) => {
    const form = new URLSearchParams(await request.text())
    return HttpResponse.json({
      access_token: `tok-${form.get('refresh_token')}`,
      expires_in: 3600,
      token_type: 'Bearer',
    })
  }),
  http.get('*/iam/v1/projects/:project/serviceAccounts', () => HttpResponse.json({})),
  // The Cloud Storage view lists buckets on /gcs; tests of it add their own.
  http.get('*/storage/v1/b', () => HttpResponse.json({})),
]
