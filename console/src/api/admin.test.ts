import { http, HttpResponse } from 'msw'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { server } from '@/test/setup'

import { admin, repeatable, unwrap } from './admin'

// The admin client re-sends a write that got no response as the public
// API path does (ADR 0062).

function flaky(method: 'put' | 'post', path: string, fail: number) {
  const bodies: string[] = []
  server.use(
    http[method](`*${path}`, async ({ request }) => {
      bodies.push(await request.text())
      if (bodies.length <= fail) return HttpResponse.error()
      return HttpResponse.json({ level: 'enforce' })
    }),
  )
  return bodies
}

describe('admin no-response retry', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('re-sends a PUT that got no response, with the same body', async () => {
    const bodies = flaky('put', '/_emu/v1/iam/enforcement', 2)
    const res = unwrap(admin.PUT('/_emu/v1/iam/enforcement', { body: { level: 'enforce' } }))
    await vi.runAllTimersAsync()
    await expect(res).resolves.toEqual({ level: 'enforce' })
    expect(bodies).toEqual(Array(3).fill('{"level":"enforce"}'))
  })

  it('sends a POST that did not opt in once, with one networkError message', async () => {
    const bodies = flaky('post', '/_emu/v1/links', 1)
    const res = unwrap(admin.POST('/_emu/v1/links', { body: { address: 'h:1', fingerprint: 'f' } }))
    await expect(res).rejects.toMatchObject({ httpStatus: 0 })
    await expect(res).rejects.toThrow(/^Cannot reach the emulator \([^()]*\)\. Is the Instance/)
    expect(bodies).toHaveLength(1)
  })

  it('re-sends a POST that opted in', async () => {
    const bodies = flaky('post', '/_emu/v1/cdn/purge', 1)
    const res = unwrap(admin.POST('/_emu/v1/cdn/purge', repeatable))
    await vi.runAllTimersAsync()
    await expect(res).resolves.toBeDefined()
    expect(bodies).toHaveLength(2)
  })
})
