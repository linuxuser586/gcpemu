import { expect, it } from 'vitest'

import { formatTaints, parseTaints, taintsError } from './taints'

it('round-trips taints through gcloud’s key=value:Effect text', () => {
  const taints = [
    { key: 'dedicated', value: 'gpu', effect: 'NO_SCHEDULE' as const },
    { key: 'spot', effect: 'PREFER_NO_SCHEDULE' as const },
  ]
  expect(formatTaints(taints)).toBe('dedicated=gpu:NoSchedule\nspot:PreferNoSchedule')
  expect(parseTaints(formatTaints(taints))).toEqual(taints)
})

it('explains malformed taints', () => {
  expect(taintsError('a=b:NoExecute')).toBeUndefined()
  expect(taintsError('a=b')).toMatch(/Invalid taint "a=b"/)
  expect(taintsError('a=b:Never')).toMatch(/NoSchedule, PreferNoSchedule or NoExecute/)
})
