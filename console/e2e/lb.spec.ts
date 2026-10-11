import type { APIRequestContext, Page } from '@playwright/test'

import { expect, projectId, test } from './fixtures'

// The Load Balancer view (SRS 4.8.3, FR-UI-011) against a real Instance:
// a load balancer built with the forms, its local listener, the route
// tree, "test a URL" and an edit of its URL map. Nothing needs a
// container runtime: the backend NEG's endpoint is never dialled.

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

/** create fills a kind's create form with fill and submits it. */
async function create(page: Page, id: string, coll: string, fill: () => Promise<void>) {
  await page.goto(`/console/lb/create/${coll}?project=${id}`)
  await fill()
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/lb/global/${coll}/`), { timeout: 15_000 })
}

test('Load Balancer: the create forms show the API’s validation messages', async ({
  page,
  browserName,
}) => {
  const id = projectId('e2e-lbform', browserName)
  await page.goto(`/console/lb?project=${id}`)
  await expect(page.getByText('No forwarding rules in this Project yet.')).toBeVisible()
  await page.getByRole('link', { name: 'Health checks' }).click()
  await page.getByRole('link', { name: 'Create health check' }).click()

  await page.getByLabel('Name').fill('Bad_Name')
  await page.getByLabel('Check interval (seconds)').fill('500')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page.getByText(/Invalid value for field 'resource.name': 'Bad_Name'/)).toBeVisible()
  await expect(
    page.getByText(
      "Invalid value for field 'resource.checkIntervalSec': '500'. Must be between 1 and 300.",
    ),
  ).toBeVisible()

  // The JSON editor holds the whole request body.
  await page.getByLabel('Name').fill('hc')
  await page.getByLabel('Check interval (seconds)').fill('10')
  await page.getByLabel('Protocol').selectOption('TCP')
  await page.getByRole('tab', { name: 'JSON' }).click()
  const body = JSON.parse(await page.getByLabel('Request body').inputValue()) as Record<
    string,
    unknown
  >
  expect(body).toMatchObject({
    name: 'hc',
    type: 'TCP',
    checkIntervalSec: 10,
    tcpHealthCheck: { port: 80 },
  })
  expect(body.httpHealthCheck).toBeUndefined()

  // The API rejects what the form cannot know is wrong.
  await page.goto(`/console/lb/create/forwardingRules?project=${id}`)
  await page.getByLabel('Name').fill('web')
  await page.getByLabel('Port').fill('9999')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page.getByText("Required field 'resource.target' not specified")).toBeVisible()
  await page.getByRole('tab', { name: 'JSON' }).click()
  await page
    .getByLabel('Request body')
    .fill(
      JSON.stringify({ name: 'web', portRange: '9999', target: 'global/targetHttpProxies/none' }),
    )
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(
    page.getByRole('alert').filter({
      hasText: `The resource 'projects/${id}/global/targetHttpProxies/none' was not found`,
    }),
  ).toBeVisible()
})

test('Load Balancer: build a load balancer, test URLs and edit its URL map', async ({
  page,
  request,
  browserName,
  requests,
}) => {
  test.setTimeout(3 * 60_000)
  const id = projectId('e2e-lb', browserName)
  const scope = `projects/${id}`
  // The backend: a NEG (a compute resource) with one endpoint.
  await compute(request, `${scope}/global/networks`, { name: 'vpc', autoCreateSubnetworks: false })
  await compute(request, `${scope}/zones/us-central1-a/networkEndpointGroups`, {
    name: 'web-neg',
    networkEndpointType: 'GCE_VM_IP_PORT',
    network: 'global/networks/vpc',
    defaultPort: 8000,
  })
  await compute(
    request,
    `${scope}/zones/us-central1-a/networkEndpointGroups/web-neg/attachNetworkEndpoints`,
    { networkEndpoints: [{ ipAddress: '10.0.0.5', port: 8000 }] },
  )

  // Each resource with its form.
  await create(page, id, 'backendServices', async () => {
    await page.getByLabel('Name').fill('api')
    await page.getByLabel('Backends').selectOption({ label: 'web-neg (us-central1-a)' })
    await page.getByLabel('Custom response headers').fill('X-Served-By:gcpemu')
  })
  await expect(page.getByRole('heading', { name: 'api' })).toBeVisible()
  const endpoint = page.getByTestId('endpoint')
  await expect(endpoint).toContainText('10.0.0.5:8000')
  await expect(endpoint).toContainText('HEALTHY')

  await create(page, id, 'backendBuckets', async () => {
    await page.getByLabel('Name').fill('static')
    await page.getByLabel('Cloud Storage bucket').fill('static-assets')
    await page.getByLabel('Cloud CDN').check()
  })

  await create(page, id, 'urlMaps', async () => {
    await page.getByLabel('Name').fill('web-map')
    await page
      .getByLabel('Default backend', { exact: true })
      .selectOption({ label: 'static (backend bucket)' })
    await page.getByRole('button', { name: 'Add path matcher' }).click()
    await page.getByLabel('Path matcher name').fill('app')
    await page.getByLabel('Hosts').fill('app.example.test')
    await page
      .getByLabel('Default backend of the matcher')
      .selectOption({ label: 'static (backend bucket)' })
    await page.getByLabel('Path rules').fill('/api/* = global/backendServices/api')
  })
  const tree = page.getByRole('list', { name: 'Route tree' })
  await expect(tree.getByTestId('host-rule')).toContainText('app.example.test')
  await expect(tree.getByTestId('path-rule')).toContainText('/api/*')

  await create(page, id, 'targetHttpProxies', async () => {
    await page.getByLabel('Name').fill('web-proxy')
    await page.getByLabel('URL map').selectOption({ label: 'web-map' })
  })
  await create(page, id, 'forwardingRules', async () => {
    await page.getByLabel('Name').fill('web')
    await page.getByLabel('Port').fill('8080')
    await page.getByLabel('Target proxy').selectOption({ label: 'web-proxy (target HTTP proxy)' })
  })

  // The forwarding rule listens on this machine (FR-LB-002).
  const listener = page.getByRole('region', { name: 'Local listener' })
  await expect(listener.getByText(/^127\.0\.0\.\d+:\d+$/)).toBeVisible()
  await expect(listener.getByText(/^curl http:\/\/127\.0\.0\.\d+:\d+\/$/)).toBeVisible()
  await page.goto(`/console/lb?project=${id}`)
  await expect(page.getByTestId('resource')).toContainText(/127\.0\.0\.\d+:\d+/)
  await expect(page.getByTestId('resource')).toContainText(':8080')

  // Test a URL: the route and backend that would serve it.
  await page.goto(`/console/lb/global/urlMaps/web-map?project=${id}`)
  const url = page.getByLabel('URL', { exact: true })
  const result = page.getByRole('status', { name: 'Route' })
  await url.fill('http://app.example.test/api/users?page=2')
  await page.getByRole('button', { name: 'Test' }).click()
  await expect(result).toContainText('Path rule of path matcher app (/api/*)')
  await expect(result.getByRole('link', { name: 'api' })).toBeVisible()
  await expect(result.getByRole('list', { name: 'Backend endpoints' })).toContainText('HEALTHY')
  await expect(tree.getByTestId('path-rule')).toContainText('Matched')

  await url.fill('http://other.test/')
  await page.getByRole('button', { name: 'Test' }).click()
  await expect(result).toContainText('The URL map’s default')
  await expect(result.getByRole('link', { name: 'static' })).toBeVisible()
  await expect(tree.getByTestId('map-default')).toContainText('Matched')

  // Edit the URL map: a path rule more.
  await page.getByRole('link', { name: 'Edit URL map' }).click()
  await page
    .getByLabel('Path rules')
    .fill('/api/* = global/backendServices/api\n/assets/* = global/backendBuckets/static')
  await page.getByRole('button', { name: 'Save' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/lb/global/urlMaps/web-map\\?project=${id}$`))
  await expect(tree.getByTestId('path-rule')).toHaveCount(2)
  await url.fill('app.example.test/assets/app.css')
  await page.getByRole('button', { name: 'Test' }).click()
  await expect(result).toContainText('Path rule of path matcher app (/assets/*)')

  // A URL map in use cannot be deleted; its forwarding rule can.
  await page.getByRole('button', { name: 'Delete' }).click()
  await page.getByRole('button', { name: 'Delete URL map' }).click()
  await expect(page.getByText(/is already being used by/)).toBeVisible()
  await page.keyboard.press('Escape')
  await page.goto(`/console/lb/global/forwardingRules/web?project=${id}`)
  await page.getByRole('button', { name: 'Delete' }).click()
  await page.getByRole('button', { name: 'Delete forwarding rule' }).click()
  await expect(page.getByText('No forwarding rules in this Project yet.')).toBeVisible({
    timeout: 15_000,
  })

  // FR-UI-003: only public APIs and the admin API.
  for (const r of requests) {
    const u = new URL(r.url())
    if (u.origin !== new URL(page.url()).origin) continue
    expect(u.pathname).toMatch(allowed)
  }
})

test('Load Balancer traffic: a live trace of each request, filtered by path', async ({
  page,
  request,
  browserName,
  requests,
}) => {
  test.setTimeout(3 * 60_000)
  const id = projectId('e2e-lbtraffic', browserName)
  const scope = `projects/${id}`
  const bucket = `lbtraffic-e2e-${browserName}`
  // A load balancer in front of a CDN-enabled bucket, with a redirect.
  expect((await request.post(`/storage/v1/b?project=${id}`, { data: { name: bucket } })).ok()).toBe(
    true,
  )
  expect(
    (
      await request.post(`/upload/storage/v1/b/${bucket}/o?uploadType=media&name=app.css`, {
        headers: { 'Content-Type': 'text/css' },
        data: 'body{}',
      })
    ).ok(),
  ).toBe(true)
  await compute(request, `${scope}/global/backendBuckets`, {
    name: 'static',
    bucketName: bucket,
    enableCdn: true,
  })
  await compute(request, `${scope}/global/urlMaps`, {
    name: 'web-map',
    defaultService: 'global/backendBuckets/static',
    hostRules: [{ hosts: ['*'], pathMatcher: 'app' }],
    pathMatchers: [
      {
        name: 'app',
        defaultService: 'global/backendBuckets/static',
        pathRules: [{ paths: ['/old'], urlRedirect: { pathRedirect: '/app.css' } }],
      },
    ],
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

  await page.goto(`/console/lb?project=${id}`)
  await page.getByRole('link', { name: 'Traffic' }).click()
  await expect(page.getByText(/No requests to this Project/)).toBeVisible()

  // Requests appear as they are served, without a reload (FR-UI-006).
  const rows = page.getByTestId('request')
  expect((await request.get(`http://${listener}/app.css`)).status()).toBe(200)
  expect((await request.get(`http://${listener}/app.css`)).status()).toBe(200)
  expect((await request.get(`http://${listener}/old`, { maxRedirects: 0 })).status()).toBe(301)
  await expect(rows).toHaveCount(3)
  await expect(rows.nth(0)).toContainText('/old')
  await expect(rows.nth(0)).toContainText('301')
  await expect(rows.nth(0)).toContainText('Path rule /old')
  await expect(rows.nth(0)).toContainText(`→ http://${listener}/app.css`)
  await expect(rows.nth(1)).toContainText('/app.css')
  await expect(rows.nth(1)).toContainText('Default of app')
  await expect(rows.nth(1)).toContainText('hit')
  await expect(rows.nth(1).getByRole('link', { name: 'static' })).toBeVisible()
  await expect(rows.nth(1).getByRole('link', { name: 'web' })).toBeVisible()
  await expect(rows.nth(2)).toContainText('miss')

  // The whole trace of one request.
  await rows
    .nth(2)
    .getByRole('button', { name: /Details of GET/ })
    .click()
  const trace = page.getByLabel(/^Trace of GET/)
  await expect(trace).toContainText('response_sent_by_backend')
  await expect(trace).toContainText(`storage.googleapis.com/${bucket}`)
  await expect(trace.getByRole('link', { name: 'web-proxy' })).toBeVisible()

  // The path filter is in the link, and survives a reload.
  await page.getByLabel('Path').fill('/old')
  await expect(rows).toHaveCount(1)
  await expect(page).toHaveURL(/[?&]path=%2Fold/)
  await page.reload()
  await expect(page.getByLabel('Path')).toHaveValue('/old')
  await expect(rows).toHaveCount(1)
  await page.getByLabel('Host').fill('nowhere.test')
  await expect(page.getByText('No requests match the filters.')).toBeVisible()

  // FR-UI-003: only public APIs and the admin API.
  for (const r of requests) {
    const u = new URL(r.url())
    if (u.origin !== new URL(page.url()).origin) continue
    expect(u.pathname).toMatch(allowed)
  }
})
