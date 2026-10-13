import { createServer, type Server } from 'node:http'
import type { AddressInfo } from 'node:net'

import { expect, expectConsoleRequests, test } from './fixtures'

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
  await page.goto(`${origin}/page`)
  await expect(page).toHaveTitle('sent')
  expect(requests.some((r) => r.url().endsWith('/hang/v1/never'))).toBe(true)
  await page.goto(`${origin}/next`)

  await expectConsoleRequests(requests, { allowed: /^\/(page|next|hang\/v1\/)/ })
})

test('the request check fails a cancelled request that carries credentials', async ({
  page,
  requests,
}) => {
  test.setTimeout(10_000)
  await page.goto(`${origin}/page`)
  await page.evaluate(() => {
    void fetch('/hang/v1/never', { headers: { Authorization: 'Bearer leaked' } })
  })
  await expect.poll(() => requests.length).toBe(3)
  await page.goto(`${origin}/next`)

  await expect(
    expectConsoleRequests(requests, { allowed: /^\/(page|next|hang\/v1\/)/ }),
  ).rejects.toThrow(/hang\/v1\/never/)
})
