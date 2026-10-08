import { http, HttpResponse } from 'msw'

import type { Container, Info, Operation, Readiness, ResourceCounts } from '@/api/admin'
import type { Project } from '@/api/queries'

// A fake Instance for component tests: tests mutate `fake` to shape what
// /_emu/v1 and Resource Manager answer.

export interface Fake {
  info: Info
  ready: Readiness
  endpoints: Record<string, string>
  containers: Container[] | { status: number; error: string }
  counts: ResourceCounts
  env: Record<string, string>
  projects: Project[]
  operations: Operation[]
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
  http.get('*/_emu/v1/resources/counts', () => HttpResponse.json(fake.counts)),
  http.get('*/_emu/v1/env', () => HttpResponse.json(fake.env)),
  http.get('*/_emu/v1/operations', ({ request }) => {
    const project = new URL(request.url).searchParams.get('project')
    return HttpResponse.json({
      operations: fake.operations.filter((op) => !project || op.project === project),
    })
  }),
  http.get('*/cloudresourcemanager/v3/projects\\:search', () =>
    HttpResponse.json({ projects: fake.projects }),
  ),
  // The Cloud Storage view lists buckets on /gcs; tests of it add their own.
  http.get('*/storage/v1/b', () => HttpResponse.json({})),
]
