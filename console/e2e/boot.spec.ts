import type { Page, Request } from '@playwright/test'

import { expect, projectId, test } from './fixtures'

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

test('a bundle that fails its first load is reloaded, keeping the loader redirect', async ({
  page,
  requests,
  browserName,
}) => {
  const id = projectId('e2e-boot', browserName)
  await abort(page, bundle, 1)
  await page.goto(`/console/build/projects/${id}/locations/global/builds/b-1`)
  await expect(page).toHaveURL(new RegExp(`/builds/b-1\\?project=${id}$`))
  await expect(page.getByRole('navigation', { name: 'Services' })).toBeVisible()
  expect(documents(requests)).toHaveLength(2)
})

test('a bundle that never loads ends in a message after two reloads', async ({
  page,
  requests,
}) => {
  await abort(page, bundle)
  await page.goto('/console/pubsub?project=p1')
  await expect(page.locator('#root')).toHaveText(
    /The Web console couldn’t load its files from the emulator\. Reload to try again\./,
    { timeout: 10_000 },
  )
  // Longer than the next backoff would be: nothing reloads any more.
  await page.waitForTimeout(4_500)
  expect(documents(requests)).toHaveLength(3)
  await expect(page).toHaveURL(/\/console\/pubsub\?project=p1$/)
})

test('a stylesheet that fails its first load is reloaded', async ({ page, requests }) => {
  await abort(page, stylesheet, 1)
  await page.goto('/console/pubsub?project=p1')
  await expect(page.getByRole('heading', { name: 'Pub/Sub' })).toBeVisible()
  await expect.poll(() => documents(requests).length).toBe(2)
  await expect(page.getByRole('heading', { name: 'Pub/Sub' })).toBeVisible()
  expect(
    await page.evaluate(() =>
      [...document.styleSheets].some((s) => /\/assets\/index-/.test(s.href ?? '')),
    ),
  ).toBe(true)
})

test('a failed image or fetch does not reload the page', async ({ page, requests }) => {
  await abort(page, /\/console\/favicon\.svg$/)
  await abort(page, /\/_emu\/v1\/info$/, 1)
  await page.goto('/console/pubsub?project=p1')
  await expect(page.getByRole('heading', { name: 'Pub/Sub' })).toBeVisible()
  // Longer than the first backoff.
  await page.waitForTimeout(2_000)
  expect(documents(requests)).toHaveLength(1)
})
