import { createServer, type Server } from 'node:http'
import type { AddressInfo } from 'node:net'

import type { Page } from '@playwright/test'

import { expect, expectConsoleRequests, goto, test } from './fixtures'

// The request check every view test ends with (#234): a request the
// browser cancels when a navigation unloads its page is still checked,
// and checking it does not hang.

let server: Server
let origin: string

test.beforeAll(async () => {
  server = createServer((req, res) => {
    if (req.url === '/page') {
      res.setHeader('Content-Type', 'text/html')
      res.end(`<script>fetch('/hang/v1/never'); document.title = 'sent'</script>`)
      return
    }
    if (req.url === '/next') {
      res.end('next')
      return
    }
    // /hang/v1/never: no answer until the page that asked is gone.
  })
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve))
  origin = `http://127.0.0.1:${(server.address() as AddressInfo).port}`
})

test.afterAll(async () => {
  server.closeAllConnections()
  await new Promise((resolve) => server.close(resolve))
})

test('the request check reads a cancelled request’s headers without waiting for it', async ({
  page,
  requests,
}) => {
  test.setTimeout(10_000)
  await goto(page, `${origin}/page`)
  await expect(page).toHaveTitle('sent')
  expect(requests.some((r) => r.url().endsWith('/hang/v1/never'))).toBe(true)
  await goto(page, `${origin}/next`)

  await expectConsoleRequests(requests, { allowed: /^\/(page|next|hang\/v1\/)/ })
})

test('the request check fails a cancelled request that carries credentials', async ({
  page,
  requests,
}) => {
  test.setTimeout(10_000)
  await goto(page, `${origin}/page`)
  await page.evaluate(() => {
    void fetch('/hang/v1/never', { headers: { Authorization: 'Bearer leaked' } })
  })
  await expect.poll(() => requests.length).toBe(3)
  await goto(page, `${origin}/next`)

  await expect(
    expectConsoleRequests(requests, { allowed: /^\/(page|next|hang\/v1\/)/ }),
  ).rejects.toThrow(/hang\/v1\/never/)
})

// The navigation every spec makes (#939): Chromium cancels a request when
// the host's network changes, as the container runtime's bridges and veths
// do, so a navigation is tried again once on that error and on no other.
function stubPage(...results: (Error | 'ok')[]) {
  const calls: unknown[][] = []
  const page = {
    goto: (...args: unknown[]) => {
      calls.push(args)
      const r = results.shift()
      return r === 'ok' ? Promise.resolve(null) : Promise.reject(r ?? new Error('no more'))
    },
  } as unknown as Page
  return { page, calls }
}

const networkChanged = () =>
  new Error('page.goto: net::ERR_NETWORK_CHANGED at http://127.0.0.1/console/')

test('a navigation the host’s network change cancelled is tried again once', async () => {
  const { page, calls } = stubPage(networkChanged(), 'ok')
  await goto(page, '/console/', { waitUntil: 'load' })
  expect(calls).toEqual([
    ['/console/', { waitUntil: 'load' }],
    ['/console/', { waitUntil: 'load' }],
  ])
})

test('a navigation cancelled twice fails', async () => {
  const { page, calls } = stubPage(networkChanged(), networkChanged(), 'ok')
  await expect(goto(page, '/console/')).rejects.toThrow(/ERR_NETWORK_CHANGED/)
  expect(calls).toHaveLength(2)
})

test('a navigation that fails any other way is not tried again', async ({ page }) => {
  let tries = 0
  await page.route(`${origin}/refused`, (route) => {
    tries++
    return route.abort('connectionrefused')
  })
  await expect(goto(page, `${origin}/refused`)).rejects.toThrow()
  expect(tries).toBe(1)
})
