import { expect, it } from 'vitest'

import { formatLabels, labelsError, parseLabels } from './labels'

it('round-trips labels through text', () => {
  const labels = { team: 'web', env: '' }
  expect(parseLabels(formatLabels(labels))).toEqual(labels)
  expect(parseLabels('a=1, b=2\nc=3')).toEqual({ a: '1', b: '2', c: '3' })
})

it('explains malformed labels', () => {
  expect(labelsError('team=web\nenv=prod')).toBeUndefined()
  expect(labelsError('Team=web')).toMatch(/Invalid label key "Team"/)
  expect(labelsError('team=Web')).toMatch(/Invalid value for label "team"/)
})
