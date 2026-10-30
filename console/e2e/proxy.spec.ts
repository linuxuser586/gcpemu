import http from 'node:http'
import type { AddressInfo } from 'node:net'

import { expect, projectId, test } from './fixtures'

// The console behind an authenticating reverse proxy on its own origin,
// as oauth2-proxy puts it (#888): the proxy sends a request without its
// session cookie to sign in on another origin, so every console request,
// public API calls and token mint and revoke among them, must carry the
// same-origin cookie.

const SESSION = '_oauth2_proxy'

let proxy: http.Server | undefined
let proxyURL = ''
// redirected lists what the proxy sent to sign in; forwarded what it let
// through.
const redirected: string[] = []
const forwarded: string[] = []

test.beforeAll(async () => {
  const upstream = new URL(String(process.env.GCPEMU_E2E_URL))
  proxy = http.createServer((req, res) => {
    const path = req.url ?? '/'
    if (!req.headers.cookie?.includes(`${SESSION}=signed-in`)) {
      redirected.push(`${req.method} ${path}`)
      res.writeHead(302, { Location: 'http://sign-in.invalid/oauth2/start' }).end()
      return
    }
    forwarded.push(`${req.method} ${path}`)
    const out = http.request(
      {
        host: upstream.hostname,
        port: upstream.port,
        method: req.method,
        path,
        headers: req.headers,
      },
      (up) => {
        res.writeHead(up.statusCode ?? 502, up.headers)
        up.pipe(res)
      },
    )
    out.on('error', () => res.writeHead(502).end())
    req.pipe(out)
  })
  await new Promise<void>((resolve) => proxy?.listen(0, '127.0.0.1', resolve))
  proxyURL = `http://127.0.0.1:${(proxy.address() as AddressInfo).port}`
})

test.afterAll(() => {
  proxy?.close()
})

test('Authenticating proxy: every console request carries its session cookie', async ({
  page,
  context,
  request,
  browserName,
}, testInfo) => {
  const id = projectId(`e2e-proxy${testInfo.retry}`, browserName)
  const email = `viewer@${id}.iam.gserviceaccount.com`
  const bucket = `${id}-proxied`
  expect((await request.post(`/storage/v1/b?project=${id}`, { data: { name: bucket } })).ok()).toBe(
    true,
  )
  expect(
    (
      await request.post(`/iam/v1/projects/${id}/serviceAccounts`, {
        data: { accountId: 'viewer' },
      })
    ).ok(),
  ).toBe(true)
  await context.addCookies([{ name: SESSION, value: 'signed-in', url: proxyURL }])

  // Choose the Project in the Project dialog, then list its buckets.
  await page.goto(`${proxyURL}/console/`)
  await page.getByRole('button', { name: 'Project' }).click()
  await page.getByRole('list', { name: 'Projects' }).getByRole('button', { name: id }).click()
  await expect(page).toHaveURL(new RegExp(`\\?project=${id}$`))
  await page
    .getByRole('navigation', { name: 'Services' })
    .getByRole('link', { name: 'Cloud Storage' })
    .click()
  await expect(page.getByRole('link', { name: bucket })).toBeVisible()

  // Act as the service account: its token is minted through the proxy.
  await page.getByRole('button', { name: 'Acting as' }).click()
  await page
    .getByRole('list', { name: 'Service accounts' })
    .getByRole('button', { name: email })
    .click()
  await expect(page.getByRole('button', { name: 'Acting as' })).toHaveText(new RegExp(email))
  await expect(page.getByRole('link', { name: bucket })).toBeVisible()

  // The policy simulator mints a token and revokes it after.
  await page.goto(`${proxyURL}/console/iam/simulator?project=${id}`)
  await page.getByLabel('Principal').fill(`serviceAccount:${email}`)
  await page.getByLabel('Resource type').selectOption('bucket')
  await page.getByLabel('Bucket').fill(bucket)
  await page.getByLabel('Permissions').fill('storage.buckets.get')
  await page.getByRole('button', { name: 'Test permissions' }).click()
  await expect(page.getByText(/holds \d of 1 permissions/)).toBeVisible()
  await expect.poll(() => forwarded.some((r) => r === 'POST /oauth2/revoke')).toBe(true)

  await expect(page.getByText(/Cannot reach the emulator/)).toHaveCount(0)
  expect(forwarded).toContain('POST /oauth2/token')
  expect(redirected).toEqual([])
})
