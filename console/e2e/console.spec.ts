import { expect, projectId, test } from './fixtures'

// The paths the console may call: its own bundle, the admin API and public
// GCP API paths ("/<api>/v<n>/...") on the gateway it was served from.
const allowed = /^\/(console\/|_emu\/v1\/|[a-z]+\/v\d[a-z0-9]*\/)/

test.describe('FR-UI-002 serving', () => {
  test('/console redirects and deep links survive a hard reload', async ({ page }) => {
    await page.goto('/console?project=deep-link-project')
    await expect(page).toHaveURL(/\/console\/\?project=deep-link-project$/)

    await page.goto('/console/pubsub?project=deep-link-project')
    await expect(page.getByRole('heading', { name: 'Pub/Sub' })).toBeVisible()
    await page.reload()
    await expect(page).toHaveURL(/\/console\/pubsub\?project=deep-link-project$/)
    await expect(page.getByRole('heading', { name: 'Pub/Sub' })).toBeVisible()
    await expect(page.getByRole('button', { name: 'Project' })).toContainText('deep-link-project')
  })

  test('caching and CSP headers', async ({ request }) => {
    const index = await request.get('/console/')
    expect(index.headers()['cache-control']).toBe('no-cache')
    expect(index.headers()['content-security-policy']).toBe("default-src 'self'")
    const asset = /\/console\/(assets\/[^"]+\.js)/.exec(await index.text())?.[1]
    expect(asset).toBeTruthy()
    const js = await request.get(`/console/${asset}`)
    expect(js.headers()['cache-control']).toBe('public, max-age=31536000, immutable')
  })
})

test('FR-UI-001, 003, 004: works offline and calls only public APIs and /_emu/v1, without credentials', async ({
  page,
  requests,
  baseURL,
}) => {
  const origin = new URL(baseURL!).origin
  const violations: string[] = []
  page.on('console', (m) => {
    if (m.type() === 'error') violations.push(m.text())
  })
  // Nothing outside the gateway is reachable, as when offline.
  await page.route('**/*', (route) =>
    new URL(route.request().url()).origin === origin ? route.continue() : route.abort(),
  )
  await page.goto('/console/')
  await expect(page.getByTestId('ready-gcs')).toContainText('Ready')
  await page.getByRole('button', { name: 'Project' }).click()
  await expect(page.getByText('Projects on this Instance')).toBeVisible()
  await page.keyboard.press('Escape')
  await page
    .getByRole('navigation', { name: 'Services' })
    .getByRole('link', { name: 'Cloud Storage' })
    .click()
  await expect(page.getByRole('heading', { name: 'Cloud Storage' })).toBeVisible()

  expect(requests.length).toBeGreaterThan(3)
  for (const r of requests) {
    const url = new URL(r.url())
    expect(url.origin, r.url()).toBe(origin)
    expect(url.pathname, r.url()).toMatch(allowed)
    expect(await r.headerValue('authorization'), r.url()).toBeNull()
  }
  expect(violations).toEqual([])
})

test.describe('FR-UI-005 project switcher', () => {
  test('lists Projects and scopes the URL to the chosen one', async ({
    page,
    request,
    browserName,
  }) => {
    const id = projectId('e2e-pick', browserName)
    // A Service request referencing the Project creates it.
    expect((await request.put(`/pubsub/v1/projects/${id}/topics/topic-a`, { data: {} })).ok()).toBe(
      true,
    )

    await page.goto('/console/')
    await page.getByRole('button', { name: 'Project' }).click()
    await page.getByRole('list', { name: 'Projects' }).getByRole('button', { name: id }).click()
    await expect(page).toHaveURL(new RegExp(`\\?project=${id}$`))
    await page
      .getByRole('navigation', { name: 'Services' })
      .getByRole('link', { name: 'Pub/Sub' })
      .click()
    await expect(page).toHaveURL(new RegExp(`/console/pubsub\\?project=${id}$`))
  })

  test('an unknown Project is "not yet created", and choosing it creates nothing', async ({
    page,
    request,
    browserName,
  }) => {
    const id = projectId('e2e-unknown', browserName)
    await page.goto('/console/')
    await page.getByRole('button', { name: 'Project' }).click()
    await expect(page.getByText('Projects on this Instance')).toBeVisible()
    await page.getByLabel('Project ID').fill(id)
    await expect(page.getByText(/is not yet created/)).toBeVisible()
    await page.getByRole('button', { name: 'Open' }).click()
    await expect(page).toHaveURL(new RegExp(`\\?project=${id}$`))
    await expect(page.getByText('not yet created')).toBeVisible()

    const search = await request.get('/cloudresourcemanager/v3/projects:search')
    const { projects = [] } = (await search.json()) as { projects?: { projectId: string }[] }
    expect(projects.map((p) => p.projectId)).not.toContain(id)
  })
})

test('FR-UI-010 dashboard', async ({ page, request, context, browserName }) => {
  const id = projectId('e2e-dash', browserName)
  await page.goto('/console/')
  for (const svc of ['iam', 'gcs', 'pubsub']) {
    await expect(page.getByTestId(`ready-${svc}`)).toContainText('Ready')
  }
  const endpoints = page.getByRole('table', { name: 'Service endpoints' })
  await expect(endpoints.getByRole('cell', { name: 'gateway', exact: true })).toBeVisible()
  await expect(endpoints).toContainText(new URL(page.url()).host)
  await expect(page.getByRole('heading', { name: 'Container runtime' })).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Managed containers' })).toBeVisible()

  // Resource counts follow the Instance through /_emu/v1/events.
  const buckets = page
    .getByRole('region', { name: 'Cloud Storage' })
    .getByText('Buckets', { exact: true })
    .locator('xpath=following-sibling::dd[1]')
  const before = Number(await buckets.textContent())
  const created = await request.post(`/storage/v1/b?project=${id}`, {
    data: { name: `${id}-bucket` },
  })
  expect(created.ok()).toBe(true)
  // Other tests create buckets in parallel.
  await expect.poll(async () => Number(await buckets.textContent())).toBeGreaterThan(before)

  const env = page.getByTestId('env-output')
  await expect(env).toContainText('export STORAGE_EMULATOR_HOST=')
  if (browserName === 'chromium') {
    await context.grantPermissions(['clipboard-read', 'clipboard-write'])
    await page.getByRole('button', { name: 'Copy' }).click()
    await expect(page.getByRole('button', { name: 'Copied' })).toBeVisible()
    expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(await env.textContent())
  }
})

test('FR-UI-006: a bucket created through the public API shows up within 1 s', async ({
  page,
  request,
  browserName,
}) => {
  const id = projectId('e2e-live', browserName)
  const name = `${id}-bucket`
  const stream = page.waitForResponse((r) => new URL(r.url()).pathname === '/_emu/v1/events')
  await page.goto(`/console/gcs?project=${id}`)
  expect((await stream).headers()['content-type']).toBe('text/event-stream')
  await expect(page.getByText('No buckets in this Project yet.')).toBeVisible()

  // The stream reports the new bucket; the bucket list does not poll.
  // The server subscribes a stream before it answers, so once this one
  // is open it cannot miss the event.
  type Win = { bucketEvent: Promise<unknown> }
  await page.evaluate(async (key) => {
    const es = new EventSource('/_emu/v1/events')
    ;(window as unknown as Win).bucketEvent = new Promise((resolve) => {
      es.addEventListener('resource', (e: MessageEvent<string>) => {
        const data = JSON.parse(e.data) as { namespace: string; key: string }
        if (data.namespace === 'gcs/buckets' && data.key === key) {
          es.close()
          resolve(data)
        }
      })
    })
    await new Promise((resolve) => es.addEventListener('open', resolve, { once: true }))
  }, name)
  const created = await request.post(`/storage/v1/b?project=${id}`, { data: { name } })
  expect(created.ok()).toBe(true)
  await expect(page.getByRole('link', { name, exact: true })).toBeVisible({ timeout: 1000 })
  expect(await page.evaluate(() => (window as unknown as Win).bucketEvent)).toEqual({
    service: 'gcs',
    namespace: 'gcs/buckets',
    key: name,
  })
})

test('a deep link to a disabled Service says how to enable it', async ({ page }) => {
  await page.goto('/console/nat/routers?project=whatever-project')
  await expect(
    page.getByRole('heading', { name: 'Service nat is not enabled on this Instance' }),
  ).toBeVisible()
  await expect(page.getByText(/gcpemu start --services .*nat/)).toBeVisible()
})

test('FR-UI-012 Operations: live status, duration and the resulting resource or error', async ({
  page,
  request,
  browserName,
}) => {
  const id = projectId('e2e-ops', browserName)
  await page.goto(`/console/operations?project=${id}`)
  await expect(page.getByRole('heading', { name: 'Operations' })).toBeVisible()
  await expect(page.getByText('No Operations yet.')).toBeVisible()

  // Both inserts are accepted; the second fails once the first has
  // created the network, as in GCP.
  const insert = () =>
    request.post(`/compute/v1/projects/${id}/global/networks`, {
      data: { name: 'vpc', autoCreateSubnetworks: false },
    })
  expect((await insert()).ok()).toBe(true)
  expect((await insert()).ok()).toBe(true)

  const ops = page.getByRole('table', { name: 'Operations' }).getByTestId('operation')
  await expect(ops).toHaveCount(2)
  await expect(ops.filter({ hasText: 'Running' })).toHaveCount(2)

  const done = ops.filter({ hasText: 'Done' })
  const failed = ops.filter({ hasText: 'Failed' })
  await expect(done).toHaveCount(1, { timeout: 10_000 })
  await expect(failed).toHaveCount(1)
  await expect(done).toContainText('VPC network')
  await expect(done).toContainText('insert')
  await expect(done).toContainText(`projects/${id}/global/networks/vpc`)
  await expect(done.getByRole('cell').last()).toHaveText(/^[2-9]\.\d s$/)
  await expect(failed).toContainText('already exists')

  await failed.getByRole('button', { name: 'Show details' }).click()
  const json = page.getByLabel('Operation JSON')
  await expect(json).toContainText('"kind": "compute#operation"')
  await expect(json).toContainText('"status": "DONE"')

  await page.getByLabel('Status').selectOption('failed')
  await expect(page).toHaveURL(/[?&]status=failed\b/)
  await expect(ops).toHaveCount(1)
  await page.reload()
  await expect(ops).toHaveCount(1)
  await expect(ops).toContainText('already exists')
})
