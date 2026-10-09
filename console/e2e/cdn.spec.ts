import type { APIRequestContext } from '@playwright/test'

import { expect, projectId, test } from './fixtures'

// The Cloud CDN view (SRS 4.8.3, FR-UI-011) against a real Instance: an
// origin added with the form, traffic through a load balancer in front of
// a Cloud Storage bucket, its hit ratio and cached entries, invalidation
// and purging. Nothing needs a container runtime.
//
// The browsers run in parallel against one Instance, whose cache "Purge
// all" clears: steps that need cached entries send their traffic again
// until they see it.

const allowed = /^\/(console\/|_emu\/v1\/|[a-z]+\/v\d[a-z0-9]*\/)/

/** compute calls the compute API and waits for the Operation. */
async function compute(request: APIRequestContext, path: string, data: unknown) {
  const res = await request.post(`/compute/v1/${path}`, { data })
  expect(res.ok(), await res.text()).toBe(true)
  const op = (await res.json()) as { selfLink: string }
  const rel = op.selfLink.slice(op.selfLink.indexOf('projects/'))
  const done = (await (await request.post(`/compute/v1/${rel}/wait`)).json()) as {
    status: string
    error?: unknown
  }
  expect(done.status).toBe('DONE')
  expect(done.error).toBeUndefined()
}

test('Cloud CDN: the cache settings form shows the API’s validation messages', async ({
  page,
  request,
  browserName,
}) => {
  const id = projectId('e2e-cdnform', browserName)
  await compute(request, `projects/${id}/global/backendBuckets`, {
    name: 'assets',
    bucketName: `cdn-form-${browserName}`,
  })
  await page.goto(`/console/cdn?project=${id}`)
  await expect(page.getByText('No origins in this Project yet')).toBeVisible()
  await page.getByRole('link', { name: 'Add origin' }).click()
  await page.getByLabel('Backend').selectOption({ label: 'assets (backend bucket)' })

  await page.getByLabel('Default TTL (seconds)').fill('90000')
  await page.getByLabel('Negative caching').check()
  await page.getByLabel('Negative caching TTLs').fill('500=60')
  await page.getByRole('button', { name: 'Add origin' }).click()
  await expect(
    page.getByText(
      "Invalid value for field 'resource.cdnPolicy.defaultTtl': '90000'. Default TTL must be less than or equal to max TTL.",
    ),
  ).toBeVisible()
  await expect(
    page.getByText(/'resource.cdnPolicy.negativeCachingPolicy\[0\].code': '500'/),
  ).toBeVisible()

  // The JSON editor holds the whole patch.
  await page.getByLabel('Default TTL (seconds)').fill('600')
  await page.getByLabel('Negative caching TTLs').fill('404=30')
  await page.getByRole('tab', { name: 'JSON' }).click()
  const body = JSON.parse(await page.getByLabel('Request body').inputValue()) as Record<
    string,
    unknown
  >
  expect(body).toMatchObject({
    enableCdn: true,
    cdnPolicy: {
      defaultTtl: 600,
      negativeCaching: true,
      negativeCachingPolicy: [{ code: 404, ttl: 30 }],
    },
  })

  // The API rejects what the form cannot know is wrong.
  await page.getByLabel('Request body').fill(
    JSON.stringify({
      enableCdn: true,
      cdnPolicy: { negativeCachingPolicy: [{ code: 404, ttl: 30 }] },
    }),
  )
  await page.getByRole('button', { name: 'Add origin' }).click()
  await expect(
    page.getByText('Negative caching policy requires negative caching to be enabled.'),
  ).toBeVisible()
  await page.getByRole('tab', { name: 'Form' }).click()
  await page.getByLabel('Negative caching').check()
  await page.getByRole('button', { name: 'Add origin' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/cdn/global/backendBuckets/assets\\?`), {
    timeout: 15_000,
  })
  await expect(page.getByLabel('Origin cache settings')).toContainText('404: 30 s')
})

test('Cloud CDN: traffic, hit ratio, cached entries, invalidation and purge', async ({
  page,
  request,
  browserName,
  requests,
}) => {
  test.setTimeout(3 * 60_000)
  const id = projectId('e2e-cdn', browserName)
  const scope = `projects/${id}`
  const bucket = `cdn-e2e-${browserName}`
  // A load balancer in front of a bucket holding an image.
  expect((await request.post(`/storage/v1/b?project=${id}`, { data: { name: bucket } })).ok()).toBe(
    true,
  )
  expect(
    (
      await request.post(`/upload/storage/v1/b/${bucket}/o?uploadType=media&name=logo.png`, {
        headers: { 'Content-Type': 'image/png' },
        data: Buffer.from('not really a png'),
      })
    ).ok(),
  ).toBe(true)
  await compute(request, `${scope}/global/backendBuckets`, { name: 'static', bucketName: bucket })
  await compute(request, `${scope}/global/urlMaps`, {
    name: 'web-map',
    defaultService: 'global/backendBuckets/static',
  })
  await compute(request, `${scope}/global/targetHttpProxies`, {
    name: 'web-proxy',
    urlMap: 'global/urlMaps/web-map',
  })
  await compute(request, `${scope}/global/forwardingRules`, {
    name: 'web',
    portRange: '8080',
    loadBalancingScheme: 'EXTERNAL_MANAGED',
    target: 'global/targetHttpProxies/web-proxy',
  })
  let listener = ''
  await expect(async () => {
    const res = (await (await request.get(`/_emu/v1/lb/listeners?project=${id}`)).json()) as {
      listeners: { listener: string }[]
    }
    listener = res.listeners[0]?.listener ?? ''
    expect(listener).not.toBe('')
  }).toPass()
  const get = async () => {
    const res = await request.get(`http://${listener}/logo.png`)
    expect(res.status()).toBe(200)
    return res.headers()
  }

  // Add the bucket as an origin with the form.
  await page.goto(`/console/cdn/add?project=${id}`)
  await page.getByLabel('Backend').selectOption({ label: 'static (backend bucket)' })
  await page.getByRole('button', { name: 'Add origin' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/cdn/global/backendBuckets/static\\?`), {
    timeout: 15_000,
  })

  // A miss, then a hit: the origin's traffic and its cached entry.
  const entries = page.getByTestId('entry')
  await expect(async () => {
    await get()
    expect((await get()).age).toBeDefined()
    await expect(entries).toHaveCount(1, { timeout: 3_000 })
  }).toPass({ timeout: 30_000 })
  await expect(entries).toContainText('/logo.png')
  await expect(entries).toContainText('image/png')
  await expect(entries).toContainText('Fresh')
  const traffic = page.getByLabel('Origin traffic')
  await expect(traffic).toContainText(/Hit ratio\d/)
  await expect(traffic).not.toContainText('Hits0')
  await expect(traffic).not.toContainText('Misses0')

  // The list shows the origin with its entry and hit ratio.
  await page.goto(`/console/cdn?project=${id}`)
  const row = page.getByTestId('origin')
  await expect(row).toContainText('static')
  await expect(row).toContainText('%')

  // Invalidate the path through the URL map.
  await page.getByRole('link', { name: 'static' }).click()
  await expect(entries).toHaveCount(1)
  const inv = page.getByRole('region', { name: 'Invalidate cached content' })
  await inv.getByLabel('Path').fill('/logo.png')
  await inv.getByRole('button', { name: 'Invalidate' }).click()
  await expect(page.getByText('Invalidated /logo.png through URL map web-map.')).toBeVisible({
    timeout: 15_000,
  })
  await expect(page.getByText('Nothing cached for this origin yet.')).toBeVisible()

  // Fill it again, then purge the whole cache.
  await expect(async () => {
    await get()
    await expect(entries).toHaveCount(1, { timeout: 3_000 })
  }).toPass({ timeout: 30_000 })
  await page.goto(`/console/cdn?project=${id}`)
  await page.getByRole('button', { name: 'Purge all' }).click()
  await page.getByRole('button', { name: 'Purge cache' }).click()
  await expect(page.getByText(/^Purged \d+ cached entr(y|ies)\.$/)).toBeVisible()
  await page.getByRole('link', { name: 'static' }).click()
  await expect(page.getByText('Nothing cached for this origin yet.')).toBeVisible()

  // Removing the origin turns Cloud CDN off: responses are no longer cached.
  await page.getByRole('button', { name: 'Remove origin' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Remove origin' }).click()
  await expect(page.getByText('No origins in this Project yet')).toBeVisible({ timeout: 15_000 })
  await get()
  expect((await get()).age).toBeUndefined()

  // FR-UI-003: only public APIs and the admin API.
  for (const r of requests) {
    const u = new URL(r.url())
    if (u.origin !== new URL(page.url()).origin) continue
    expect(u.pathname).toMatch(allowed)
  }
})
