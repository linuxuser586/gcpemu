import { queryOptions } from '@tanstack/react-query'

import { admin, unwrap } from '@/api/admin'
import type { components } from '@/api/admin.gen'
import { POLL_MS } from '@/api/queries'

// The Fault rules view's reads and writes (FR-UI-016, FR-CORE-060), all on
// the admin API. Rules are the Instance's, for every Project. Nothing on
// the event stream reports a rule firing, so the list polls while a rule
// can fire, to keep hit counts current.

export type FaultRule = components['schemas']['FaultRule']

/** CODES are the canonical error names a rule can return, as the API spells them. */
export const CODES = [
  'CANCELLED',
  'UNKNOWN',
  'INVALID_ARGUMENT',
  'DEADLINE_EXCEEDED',
  'NOT_FOUND',
  'ALREADY_EXISTS',
  'PERMISSION_DENIED',
  'RESOURCE_EXHAUSTED',
  'FAILED_PRECONDITION',
  'ABORTED',
  'OUT_OF_RANGE',
  'UNIMPLEMENTED',
  'INTERNAL',
  'UNAVAILABLE',
  'DATA_LOSS',
  'UNAUTHENTICATED',
] as const

/** canFire: the rule is enabled and has not used up its count. */
export const canFire = (r: FaultRule) => !r.disabled && (!r.count || (r.fired ?? 0) < r.count)

export const faultsQuery = () =>
  queryOptions({
    queryKey: ['admin', 'faults'],
    queryFn: async (): Promise<FaultRule[]> =>
      (await unwrap(admin.GET('/_emu/v1/faults'))).faults ?? [],
    refetchInterval: (q) => (q.state.data?.some(canFire) ? POLL_MS : false),
  })

export const addFault = (rule: FaultRule) => unwrap(admin.POST('/_emu/v1/faults', { body: rule }))

export const setFaultDisabled = (id: string, disabled: boolean) =>
  unwrap(admin.PATCH('/_emu/v1/faults/{id}', { params: { path: { id } }, body: { disabled } }))

export const removeFault = (id: string) =>
  unwrap(admin.DELETE('/_emu/v1/faults/{id}', { params: { path: { id } } }))

export const clearFaults = () => unwrap(admin.DELETE('/_emu/v1/faults'))
