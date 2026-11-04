import type { Page, Request } from '@playwright/test'

import { expect, goto, projectId, test } from './fixtures'

// ADR 0063: a page whose own script or stylesheet failed to load reloads
// itself, at most twice, and says so if every attempt failed. Aborting
// with internetdisconnected stands in for Chromium's ERR_NETWORK_CHANGED.

const bundle = /\/console\/assets\/index-[^/]+\.js$/
const stylesheet = /\/console\/assets\/index-[^/]+\.css$/

/** abort fails the first times requests whose URL matches re. */
async function abort(page: Page, re: RegExp, times = Infinity) {
  let n = 0
  await page.route(re, (route) =>
    n++ < times ? route.abort('internetdisconnected') : route.continue(),
  )
}

/** documents are the page loads among requests. */
function documents(requests: Request[]) {
  return requests.filter((r) => r.resourceType() === 'document')
}

/** cspViolations collects what the page logs about its CSP. */
function cspViolations(page: Page) {
  const violations: string[] = []
  page.on('console', (m) => {
    if (/content.security.policy/i.test(m.text())) violations.push(m.text())
  })
  return violations
}

test('a bundle that fails its first load is reloaded, keeping the loader redirect', async ({
  page,
  requests,
  browserName,
}) => {
  const id = projectId('e2e-boot', browserName)
  const violations = cspViolations(page)
  await abort(page, bundle, 1)
  await goto(page, `/console/build/projects/${id}/locations/global/builds/b-1`)
  await expect(page).toHaveURL(new RegExp(`/builds/b-1\\?project=${id}$`))
  await expect(page.getByRole('navigation', { name: 'Services' })).toBeVisible()
  expect(documents(requests)).toHaveLength(2)
  expect(violations).toEqual([])
})

test('a bundle that never loads ends in a message after two reloads', async ({
  page,
  requests,
}) => {
  await abort(page, bundle)
  await goto(page, '/console/pubsub?project=p1')
  await expect(page.locator('#root')).toHaveText(
    /The Web console couldn’t load its files from the emulator\. Reload to try again\./,
    { timeout: 10_000 },
  )
  // Longer than the next backoff would be: nothing reloads any more.
  await page.waitForTimeout(4_500)
  expect(documents(requests)).toHaveLength(3)
  await expect(page).toHaveURL(/\/console\/pubsub\?project=p1$/)
})

test('without sessionStorage, a bundle that never loads is reloaded once', async ({
  page,
  requests,
}) => {
  await page.addInitScript(() => {
    Object.defineProperty(window, 'sessionStorage', {
      get() {
        throw new DOMException('denied', 'SecurityError')
      },
    })
  })
  await abort(page, bundle)
  await goto(page, '/console/pubsub?project=p1')
  await expect(page.locator('#root')).toHaveText(/couldn’t load its files/, { timeout: 10_000 })
  await page.waitForTimeout(2_500)
  expect(documents(requests)).toHaveLength(2)
})

test('a stylesheet that fails its first load is reloaded', async ({ page, requests }) => {
  await abort(page, stylesheet, 1)
  await goto(page, '/console/pubsub?project=p1')
  await expect.poll(() => documents(requests).length).toBe(2)
  await expect(page.getByRole('heading', { name: 'Pub/Sub' })).toBeVisible()
  expect(
    await page.evaluate(() =>
      [...document.styleSheets].some((s) => /\/assets\/index-/.test(s.href ?? '')),
    ),
  ).toBe(true)
})

test('a failed image or fetch does not reload the page', async ({ page, requests }) => {
  const violations = cspViolations(page)
  await abort(page, /\/console\/(favicon\.svg|missing\.png)$/)
  await abort(page, /\/_emu\/v1\/info$/, 1)
  await goto(page, '/console/pubsub?project=p1')
  await expect(page.getByRole('heading', { name: 'Pub/Sub' })).toBeVisible()
  const failed = await page.evaluate(
    () =>
      new Promise((resolve) => {
        const img = document.createElement('img')
        img.onerror = () => resolve(true)
        img.onload = () => resolve(false)
        img.src = '/console/missing.png'
        document.body.append(img)
      }),
  )
  expect(failed).toBe(true)
  // Longer than the first backoff.
  await page.waitForTimeout(2_000)
  expect(documents(requests)).toHaveLength(1)
  expect(violations).toEqual([])
})
