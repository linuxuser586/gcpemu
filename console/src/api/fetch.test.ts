import { http, HttpResponse } from 'msw'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { server } from '@/test/setup'

import { gcpFetch } from './fetch'

// A write that got no response is re-sent, if repeating it is safe
// (ADR 0062): HttpResponse.error() is what a request Chromium cancels on
// a host network change looks like to fetch.

const URL = '/test/v1/things/a'

/** flaky answers with a network error for the first `fail` attempts. */
function flaky(method: 'get' | 'put' | 'patch' | 'delete' | 'post', fail: number) {
  const bodies: string[] = []
  server.use(
    http[method](URL, async ({ request }) => {
      bodies.push(await request.text())
      if (bodies.length <= fail) return HttpResponse.error()
      return HttpResponse.json({ attempt: bodies.length })
    }),
  )
  return bodies
}

describe('gcpFetch no-response retry', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it.each(['PUT', 'PATCH', 'DELETE'] as const)(
    're-sends a %s that got no response, with the same body',
    async (method) => {
      const bodies = flaky(method.toLowerCase() as 'put', 2)
      const res = gcpFetch(URL, { method, body: '{"a":1}' })
      await vi.runAllTimersAsync()
      await expect(res).resolves.toEqual({ attempt: 3 })
      expect(bodies).toEqual(['{"a":1}', '{"a":1}', '{"a":1}'])
    },
  )

  it('gives up after 3 attempts, 1 s and then 2 s apart', async () => {
    const bodies = flaky('put', 3)
    const res = gcpFetch(URL, { method: 'PUT', body: '{}' })
    const failed = expect(res).rejects.toThrow(
      /^Cannot reach the emulator \(.*\)\. Is the Instance/,
    )
    await vi.advanceTimersByTimeAsync(999)
    expect(bodies).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(bodies).toHaveLength(2)
    await vi.advanceTimersByTimeAsync(1999)
    expect(bodies).toHaveLength(2)
    await vi.advanceTimersByTimeAsync(1)
    await failed
    expect(bodies).toHaveLength(3)
    await vi.runAllTimersAsync()
    expect(bodies).toHaveLength(3)
  })

  it('sends a POST that did not opt in once', async () => {
    const bodies = flaky('post', 1)
    await expect(gcpFetch(URL, { method: 'POST', body: '{}' })).rejects.toMatchObject({
      httpStatus: 0,
    })
    expect(bodies).toHaveLength(1)
  })

  it('re-sends a POST that opted in', async () => {
    const bodies = flaky('post', 2)
    const res = gcpFetch(URL, { method: 'POST', body: '{"b":2}', repeatable: true })
    await vi.runAllTimersAsync()
    await expect(res).resolves.toEqual({ attempt: 3 })
    expect(bodies).toEqual(['{"b":2}', '{"b":2}', '{"b":2}'])
  })

  it('sends a GET once, even if it says it is repeatable', async () => {
    const bodies = flaky('get', 1)
    await expect(gcpFetch(URL, { repeatable: true })).rejects.toMatchObject({ httpStatus: 0 })
    expect(bodies).toHaveLength(1)
  })

  it.each(['GET', 'PUT', 'PATCH', 'DELETE', 'POST'] as const)(
    'never re-sends a %s that got a 5xx',
    async (method) => {
      let attempts = 0
      server.use(
        http.all(URL, () => {
          attempts++
          return HttpResponse.json({ error: { code: 503, message: 'down' } }, { status: 503 })
        }),
      )
      const res = gcpFetch(URL, { method, repeatable: true })
      const failed = expect(res).rejects.toMatchObject({ httpStatus: 503 })
      await vi.runAllTimersAsync()
      await failed
      expect(attempts).toBe(1)
    },
  )

  it('re-sends after a refused token, and the retry has the fresh token', async () => {
    let minted = 0
    const auth: string[] = []
    server.use(
      http.post('*/oauth2/token', () =>
        HttpResponse.json({ access_token: `tok-${++minted}`, expires_in: 3600 }),
      ),
      http.put(URL, ({ request }) => {
        auth.push(request.headers.get('Authorization') ?? '')
        if (auth.length === 1) return HttpResponse.json({}, { status: 401 })
        if (auth.length === 2) return HttpResponse.error()
        return HttpResponse.json({ ok: true })
      }),
    )
    const res = gcpFetch(URL, { method: 'PUT', body: '{}', principal: 'user:a@example.com' })
    await vi.runAllTimersAsync()
    await expect(res).resolves.toEqual({ ok: true })
    expect(auth).toEqual(['Bearer tok-1', 'Bearer tok-2', 'Bearer tok-2'])
  })
})
