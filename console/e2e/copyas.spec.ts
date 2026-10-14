import type { APIRequestContext, Page } from '@playwright/test'

import { expect, expectConsoleRequests, projectId, test } from './fixtures'

// Copy as (FR-UI-015) against a real Instance: each resource's dialog
// shows the gcloud command, curl request and OpenTofu block, and the curl
// request, replayed after the resource is deleted, creates it again as it
// was. Each attempt has its own Project.

const allowed = /^\/(console\/|_emu\/v1\/|[a-z]+\/v\d[a-z0-9]*\/)/

interface Copied {
  gcloud: string
  curl: string
  tofu: string
}

/** copyAs opens the page's Copy as dialog and reads the resource in each form. */
async function copyAs(page: Page, what: string): Promise<Copied> {
  await page.getByRole('button', { name: `Copy ${what} as` }).click()
  const dialog = page.getByRole('dialog', { name: `Copy ${what} as` })
  const read = async (tab: string) => {
    await dialog.getByRole('tab', { name: tab }).click()
    return (await dialog.getByLabel(`${tab} for the ${what}`).textContent()) ?? ''
  }
  const out = {
    gcloud: await read('gcloud'),
    curl: await read('curl'),
    tofu: await read('OpenTofu'),
  }
  await dialog.getByRole('button', { name: 'Close' }).click()
  return out
}

/** replay sends the requests a copied curl text makes, in order. */
async function replay(api: APIRequestContext, text: string) {
  for (const curl of text.split(/\n\n(?=curl )/)) {
    const url = /^curl '?([^'\s]+)'?/.exec(curl)?.[1] ?? ''
    const method = /-X (\w+)/.exec(curl)?.[1] ?? ''
    const body = /-d '([\s\S]*)'$/.exec(curl)?.[1]?.replaceAll(`'\\''`, "'")
    const res = await api.fetch(url, {
      method,
      headers: body ? { 'Content-Type': 'application/json' } : {},
      data: body,
    })
    expect(res.ok(), `${method} ${url}: ${await res.text()}`).toBe(true)
  }
}

/** same is a resource without the fields the server sets when it is created, at any depth. */
function same(v: unknown, volatile: readonly string[] = []): unknown {
  if (Array.isArray(v)) return v.map((x) => same(x, volatile))
  if (typeof v !== 'object' || v === null) return v
  return Object.fromEntries(
    Object.entries(v)
      .filter(([k]) => !volatile.includes(k))
      .map(([k, x]) => [k, same(x, volatile)]),
  )
}

test('Copy as: a Pub/Sub subscription as gcloud, curl and OpenTofu', async ({
  page,
  browserName,
  context,
  requests,
}, testInfo) => {
  const id = projectId(`e2e-cp${testInfo.retry}`, browserName)
  const api = page.request
  const topic = `projects/${id}/topics/orders`
  const sub = `/pubsub/v1/projects/${id}/subscriptions/worker`
  expect((await api.put(`/pubsub/v1/${topic}`, { data: { labels: { team: 'e2e' } } })).ok()).toBe(
    true,
  )
  const created = await api.put(sub, {
    data: {
      topic,
      ackDeadlineSeconds: 30,
      labels: { team: 'e2e' },
      retainAckedMessages: true,
      filter: 'attributes.kind = "order"',
      retryPolicy: { minimumBackoff: '5s', maximumBackoff: '60s' },
    },
  })
  expect(created.ok()).toBe(true)
  const before = (await created.json()) as Record<string, unknown>

  await page.goto(`/console/pubsub/subscriptions/worker?project=${id}`)
  const copied = await copyAs(page, 'subscription')
  expect(copied.gcloud).toBe(
    [
      'gcloud pubsub subscriptions create worker \\',
      `    --project=${id} \\`,
      `    --topic=${topic} \\`,
      '    --ack-deadline=30 \\',
      '    --retain-acked-messages \\',
      `    --message-retention-duration=${String(before.messageRetentionDuration)} \\`,
      '    --labels=team=e2e \\',
      `    --expiration-period=${String((before.expirationPolicy as { ttl: string }).ttl)} \\`,
      `    --message-filter='attributes.kind = "order"' \\`,
      '    --min-retry-delay=5s \\',
      '    --max-retry-delay=60s',
    ].join('\n'),
  )
  expect(copied.curl).toContain(`curl ${new URL(sub, page.url()).href} \\\n    -X PUT`)
  expect(copied.tofu).toContain('resource "google_pubsub_subscription" "worker" {')
  expect(copied.tofu).toContain('filter                     = "attributes.kind = \\"order\\""')

  // The Copy button puts the form shown on the clipboard. Only Chromium
  // lets a test read the clipboard back.
  if (browserName === 'chromium') {
    await context.grantPermissions(['clipboard-read', 'clipboard-write'])
    await page.getByRole('button', { name: 'Copy subscription as' }).click()
    const dialog = page.getByRole('dialog', { name: 'Copy subscription as' })
    // The dialog opens on the form chosen last.
    await expect(dialog.getByRole('tab', { name: 'OpenTofu' })).toHaveAttribute(
      'aria-selected',
      'true',
    )
    await dialog.getByRole('button', { name: 'Copy' }).click()
    await expect(dialog.getByRole('button', { name: 'Copied' })).toBeVisible()
    // Windows' clipboard reads back with CRLF line endings.
    const clip = await page.evaluate(() => navigator.clipboard.readText())
    expect(clip.replaceAll('\r\n', '\n')).toBe(copied.tofu)
    await dialog.getByRole('button', { name: 'Close' }).click()
  }

  // The curl request recreates the subscription as it was.
  expect((await api.delete(sub)).ok()).toBe(true)
  await replay(api, copied.curl)
  const after = (await (await api.get(sub)).json()) as Record<string, unknown>
  expect(same(after)).toEqual(same(before))

  await expectConsoleRequests(requests, { allowed })
})

/** A resource another Service's view offers to copy, and how to make and find it. */
interface Case {
  service: string
  what: string
  /** page is the resource's page in the console. */
  page: (project: string) => string
  /** create makes the resource; it is POSTed to the path. */
  create: (project: string) => { path: string; data: unknown }
  /** at is the resource's API path, to read and delete it. */
  at: (project: string) => string
  /** tofu is the OpenTofu resource type. */
  tofu: string
  /** volatile names the fields the server sets anew on each create. */
  volatile: string[]
}

const CASES: Case[] = [
  {
    service: 'secrets',
    what: 'secret',
    page: (p) => `/console/secrets/secrets/api-key?project=${p}`,
    create: (p) => ({
      path: `/secretmanager/v1/projects/${p}/secrets?secretId=api-key`,
      data: { replication: { automatic: {} }, labels: { team: 'e2e' } },
    }),
    at: (p) => `/secretmanager/v1/projects/${p}/secrets/api-key`,
    tofu: 'google_secret_manager_secret',
    volatile: ['createTime', 'etag'],
  },
  {
    service: 'dns',
    what: 'zone',
    page: (p) => `/console/dns/zones/app/overview?project=${p}`,
    create: (p) => ({
      path: `/dns/v1/projects/${p}/managedZones`,
      data: { name: 'app', dnsName: 'app.example.com.', description: 'e2e', labels: { a: 'b' } },
    }),
    at: (p) => `/dns/v1/projects/${p}/managedZones/app`,
    tofu: 'google_dns_managed_zone',
    volatile: ['id', 'creationTime'],
  },
  {
    service: 'gcs',
    what: 'bucket',
    page: (p) => `/console/gcs/b/${p}-cp/configuration?project=${p}`,
    create: (p) => ({
      path: `/storage/v1/b?project=${p}`,
      data: { name: `${p}-cp`, labels: { team: 'e2e' }, versioning: { enabled: true } },
    }),
    at: (p) => `/storage/v1/b/${p}-cp`,
    tofu: 'google_storage_bucket',
    volatile: ['timeCreated', 'updated', 'etag', 'generation', 'metageneration', 'effectiveTime'],
  },
  {
    service: 'ar',
    what: 'repository',
    page: (p) => `/console/ar/locations/us-central1/repositories/images/overview?project=${p}`,
    create: (p) => ({
      path: `/artifactregistry/v1/projects/${p}/locations/us-central1/repositories?repositoryId=images`,
      data: { format: 'DOCKER', description: 'e2e', labels: { team: 'e2e' } },
    }),
    at: (p) => `/artifactregistry/v1/projects/${p}/locations/us-central1/repositories/images`,
    tofu: 'google_artifact_registry_repository',
    volatile: ['createTime', 'updateTime'],
  },
  {
    service: 'lb',
    what: 'health check',
    page: (p) => `/console/lb/global/healthChecks/hc?project=${p}`,
    create: (p) => ({
      path: `/compute/v1/projects/${p}/global/healthChecks`,
      data: {
        name: 'hc',
        type: 'HTTP',
        checkIntervalSec: 7,
        httpHealthCheck: { port: 8080, requestPath: '/healthz' },
      },
    }),
    at: (p) => `/compute/v1/projects/${p}/global/healthChecks/hc`,
    tofu: 'google_compute_health_check',
    volatile: ['id', 'creationTimestamp'],
  },
]

for (const c of CASES) {
  test(`Copy as: a ${c.what} (${c.service}) round-trips through its curl request`, async ({
    page,
    browserName,
    requests,
  }, testInfo) => {
    const id = projectId(`e2e-cp${c.service}${testInfo.retry}`, browserName)
    const api = page.request
    // Compute and Artifact Registry create through Operations: wait for the
    // resource, or for it to be gone.
    const read = () => api.get(c.at(id))
    const { path, data } = c.create(id)
    const made = await api.post(path, { data })
    expect(made.ok(), await made.text()).toBe(true)
    await expect.poll(async () => (await read()).status(), { timeout: 15_000 }).toBe(200)
    const before = (await (await read()).json()) as unknown

    await page.goto(c.page(id))
    const copied = await copyAs(page, c.what)
    expect(copied.gcloud).toMatch(/^gcloud /)
    expect(copied.gcloud).toContain(`--project=${id}`)
    expect(copied.tofu).toContain(`resource "${c.tofu}" `)
    expect(copied.tofu).toContain(`project`)

    expect((await api.delete(c.at(id))).ok()).toBe(true)
    await expect.poll(async () => (await read()).status(), { timeout: 15_000 }).toBe(404)
    await replay(api, copied.curl)
    await expect.poll(async () => (await read()).status(), { timeout: 15_000 }).toBe(200)
    const after = (await (await read()).json()) as unknown
    expect(same(after, c.volatile)).toEqual(same(before, c.volatile))

    await expectConsoleRequests(requests, { allowed })
  })
}
