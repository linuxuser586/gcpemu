import { queryOptions } from '@tanstack/react-query'

import { admin, unwrap } from '@/api/admin'
import type { components } from '@/api/admin.gen'

// The State view's reads and writes (FR-UI-017), all on the admin API:
// State snapshots, Reset, and Seed file import and export. Restoring a
// snapshot, a Reset and a seed all change resources everywhere; the event
// stream's `reset` event, or the resource events a seed sends, refetch
// the other views.

export type Snapshot = components['schemas']['Snapshot']

export const snapshotsQuery = () =>
  queryOptions({
    queryKey: ['admin', 'snapshots'],
    queryFn: async (): Promise<Snapshot[]> =>
      (await unwrap(admin.GET('/_emu/v1/snapshots'))).snapshots ?? [],
  })

export const saveSnapshot = (body: { name: string }) =>
  unwrap(admin.POST('/_emu/v1/snapshots', { body }))

export const restoreSnapshot = (name: string) =>
  unwrap(admin.POST('/_emu/v1/snapshots/{name}/restore', { params: { path: { name } } }))

export const deleteSnapshot = (name: string) =>
  unwrap(admin.DELETE('/_emu/v1/snapshots/{name}', { params: { path: { name } } }))

export const resetInstance = () => unwrap(admin.POST('/_emu/v1/reset'))

/** applySeed uploads a seed file's YAML, which the Instance applies idempotently. */
export const applySeed = (yaml: string) =>
  unwrap(
    admin.POST('/_emu/v1/seed', {
      body: yaml,
      bodySerializer: (b) => b ?? '',
      headers: { 'Content-Type': 'application/yaml' },
    }),
  )

/** exportSeed returns the Instance's resources as seed file YAML. */
export const exportSeed = (): Promise<string> =>
  unwrap<string>(admin.GET('/_emu/v1/seed', { parseAs: 'text' }))

/** SNAPSHOT_NAME is what the admin API takes as a State snapshot name. */
export const SNAPSHOT_NAME = /^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$/
