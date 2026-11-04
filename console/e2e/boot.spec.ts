import type { Page, Request } from '@playwright/test'

import { expect, goto, projectId, test } from './fixtures'

// ADR 0063: a page whose own script failed to load reloads itself, at most
// twice, and a page whose stylesheet failed fetches it again in place; each
// says so if every attempt failed. Aborting with internetdisconnected stands
// in for Chromium's ERR_NETWORK_CHANGED.

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

/** denySessionStorage makes every use of sessionStorage on page throw. */
async function denySessionStorage(page: Page) {
  await page.addInitScript(() => {
    Object.defineProperty(window, 'sessionStorage', {
      get() {
        throw new DOMException('denied', 'SecurityError')
      },
    })
  })
}

/**
 * styled says whether the console's stylesheet applies to page: its reset
 * takes away the margin browsers give body. document.styleSheets cannot say,
 * because it lists a <link> that failed.
 */
function styled(page: Page) {
  return page.evaluate(() => getComputedStyle(document.body).marginTop === '0px')
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
  await denySessionStorage(page)
  await abort(page, bundle)
  await goto(page, '/console/pubsub?project=p1')
  await expect(page.locator('#root')).toHaveText(/couldn’t load its files/, { timeout: 10_000 })
  await page.waitForTimeout(2_500)
  expect(documents(requests)).toHaveLength(2)
})

test('a stylesheet that fails its first load is fetched again without a reload', async ({
  page,
  requests,
}) => {
  const violations = cspViolations(page)
  await abort(page, stylesheet, 1)
  await goto(page, '/console/pubsub?project=p1')
  await expect(page.getByRole('heading', { name: 'Pub/Sub' })).toBeVisible()
  await expect.poll(() => styled(page)).toBe(true)
  expect(documents(requests)).toHaveLength(1)
  expect(violations).toEqual([])
})

test('a form keeps its input when the stylesheet fails its first load', async ({
  page,
  requests,
}) => {
  await abort(page, stylesheet, 1)
  await goto(page, '/console/compute/create-address?project=p1')
  await page.getByLabel('Name').fill('web-ip')
  // Longer than the second after which the page used to reload (#969).
  await page.waitForTimeout(1_500)
  await expect(page.getByLabel('Name')).toHaveValue('web-ip')
  expect(documents(requests)).toHaveLength(1)
  await expect.poll(() => styled(page)).toBe(true)
})

test('a stylesheet that never loads ends in a notice, and the console keeps working', async ({
  page,
  requests,
  browserName,
}) => {
  const id = projectId('e2e-boot', browserName)
  const notice = 'The Web console couldn’t load its styles from the emulator. Reload to try again.'
  await abort(page, stylesheet)
  await goto(page, `/console/pubsub?project=${id}`)
  await expect(page.getByRole('heading', { name: 'Pub/Sub' })).toBeVisible()
  await expect(page.getByText(notice)).toBeVisible({ timeout: 10_000 })
  await expect(page.locator('#root').getByText(notice)).toHaveCount(0)
  // Longer than the next backoff would be: nothing is fetched any more.
  await page.waitForTimeout(4_500)
  expect(requests.filter((r) => stylesheet.test(r.url()))).toHaveLength(3)
  expect(documents(requests)).toHaveLength(1)
  await page
    .getByRole('navigation', { name: 'Services' })
    .getByRole('link', { name: 'Cloud Storage' })
    .click()
  await expect(page).toHaveURL(/\/console\/gcs/)
  await expect(page.getByRole('heading', { name: 'Cloud Storage' })).toBeVisible()
  expect(documents(requests)).toHaveLength(1)
})

test('a bundle that fails while a stylesheet attempt waits still reloads the page', async ({
  page,
  requests,
}) => {
  await abort(page, stylesheet, 1)
  // The bundle fails once the stylesheet has, so that its attempt is waiting.
  const stylesheetFailed = page.waitForEvent('requestfailed', (r) => stylesheet.test(r.url()))
  let n = 0
  await page.route(bundle, async (route) => {
    if (n++ > 0) return route.continue()
    await stylesheetFailed
    await route.abort('internetdisconnected')
  })
  await goto(page, '/console/pubsub?project=p1')
  await expect(page.getByRole('navigation', { name: 'Services' })).toBeVisible()
  expect(documents(requests)).toHaveLength(2)
  await expect.poll(() => styled(page)).toBe(true)
})

test('a page whose bundle loaded clears the reload count, even if its stylesheet failed', async ({
  page,
  requests,
}) => {
  await abort(page, bundle, 1)
  // The reloaded page, whose bundle loads, is the one whose stylesheet fails.
  let failed = false
  await page.route(stylesheet, (route) => {
    if (failed || documents(requests).length < 2) return route.continue()
    failed = true
    return route.abort('internetdisconnected')
  })
  await goto(page, '/console/pubsub?project=p1')
  await expect(page.getByRole('navigation', { name: 'Services' })).toBeVisible()
  await expect.poll(() => styled(page), { timeout: 10_000 }).toBe(true)
  expect(failed).toBe(true)
  expect(documents(requests)).toHaveLength(2)
  expect(
    await page.evaluate(() => sessionStorage.getItem('gcpemu.boot:/console/pubsub')),
  ).toBeNull()
})

test('without sessionStorage, a stylesheet is still fetched again without a reload', async ({
  page,
  requests,
}) => {
  await denySessionStorage(page)
  await abort(page, stylesheet, 1)
  await goto(page, '/console/pubsub?project=p1')
  await expect.poll(() => styled(page)).toBe(true)
  expect(documents(requests)).toHaveLength(1)
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
