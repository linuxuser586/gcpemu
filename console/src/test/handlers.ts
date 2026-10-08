import { http, HttpResponse } from 'msw'

import type { Container, Info, Readiness, ResourceCounts } from '@/api/admin'
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
  http.get('*/cloudresourcemanager/v3/projects\\:search', () =>
    HttpResponse.json({ projects: fake.projects }),
  ),
]
