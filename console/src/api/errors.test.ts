import { describe, expect, it } from 'vitest'

import { ApiError, fieldViolations, networkError, shouldRetry, toApiError } from './errors'

describe('toApiError', () => {
  it('reads the public APIs’ Google error envelope', () => {
    const e = toApiError(409, {
      error: {
        code: 409,
        message: 'Your previous request to create the named bucket succeeded.',
        status: 'ALREADY_EXISTS',
        details: [{ '@type': 'type.googleapis.com/google.rpc.ErrorInfo', reason: 'conflict' }],
      },
    })
    expect(e).toMatchObject({
      httpStatus: 409,
      status: 'ALREADY_EXISTS',
      message: 'Your previous request to create the named bucket succeeded.',
    })
    expect(e.details).toHaveLength(1)
  })

  it('reads the admin API’s {error: message}', () => {
    expect(toApiError(400, { error: 'by must be a non-negative duration' })).toMatchObject({
      httpStatus: 400,
      status: 'INVALID_ARGUMENT',
      message: 'by must be a non-negative duration',
      details: [],
    })
  })

  it('reads a Kubernetes Status passed through the Connect gateway', () => {
    const status = {
      kind: 'Status',
      apiVersion: 'v1',
      status: 'Failure',
      message: 'pods "web" is forbidden: User "dev@example.com" cannot get resource "pods"',
      reason: 'Forbidden',
      code: 403,
    }
    expect(toApiError(403, status)).toMatchObject({
      httpStatus: 403,
      status: 'PERMISSION_DENIED',
      message: status.message,
    })
  })

  it('falls back to the body text or the HTTP status', () => {
    expect(toApiError(502, 'bad gateway\n')).toMatchObject({
      status: 'INTERNAL',
      message: 'bad gateway',
    })
    expect(toApiError(404, undefined)).toMatchObject({ status: 'NOT_FOUND', message: 'HTTP 404' })
  })
})

describe('shouldRetry', () => {
  it('never retries a 4xx', () => {
    expect(shouldRetry(0, new ApiError(404, 'NOT_FOUND', 'no'))).toBe(false)
  })
  it('retries a 5xx or network failure up to twice', () => {
    for (const e of [
      new ApiError(503, 'UNAVAILABLE', 'later'),
      networkError(new TypeError('fetch failed')),
    ]) {
      expect(shouldRetry(0, e)).toBe(true)
      expect(shouldRetry(1, e)).toBe(true)
      expect(shouldRetry(2, e)).toBe(false)
    }
  })
})

it('fieldViolations extracts google.rpc.BadRequest details', () => {
  const e = toApiError(400, {
    error: {
      code: 400,
      message: 'Invalid',
      status: 'INVALID_ARGUMENT',
      details: [
        {
          '@type': 'type.googleapis.com/google.rpc.BadRequest',
          fieldViolations: [{ field: 'name', description: 'must not be empty' }],
        },
      ],
    },
  })
  expect(fieldViolations(e)).toEqual([{ field: 'name', description: 'must not be empty' }])
  expect(fieldViolations(new Error('x'))).toEqual([])
})
