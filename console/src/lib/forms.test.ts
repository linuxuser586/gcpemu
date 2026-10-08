import { expect, it, vi } from 'vitest'

import { toApiError } from '@/api/errors'

import { applyFieldErrors } from './forms'

it('puts field violations on the fields they name', () => {
  const setError = vi.fn()
  const err = toApiError(400, {
    error: {
      code: 400,
      status: 'INVALID_ARGUMENT',
      message: 'Invalid request',
      details: [
        {
          '@type': 'type.googleapis.com/google.rpc.BadRequest',
          fieldViolations: [
            { field: 'name', description: 'Bucket names must be 3 to 63 characters.' },
            { field: 'unknownField', description: 'ignored' },
          ],
        },
      ],
    },
  })
  expect(
    applyFieldErrors<{ name: string; location: string }>(setError, err, ['name', 'location']),
  ).toBe(true)
  expect(setError).toHaveBeenCalledExactlyOnceWith('name', {
    type: 'server',
    message: 'Bucket names must be 3 to 63 characters.',
  })
  expect(applyFieldErrors(setError, new Error('x'), ['name'])).toBe(false)
})
