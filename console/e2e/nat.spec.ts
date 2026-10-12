import type { APIRequestContext, Page } from '@playwright/test'

import { expect, projectId, test } from './fixtures'

// The Cloud NAT view (SRS 4.8.3, FR-UI-011) against the Instance's
// compute and nat Services. nat runs on Linux only (emulator.ts): its
// data plane needs a container runtime. The live translation log runs a
// private GKE cluster behind a NAT with the offline sink on, so that the
// test never reaches the internet. The sink is the Instance's: tests only
// turn it on.

const allowed = /^\/(console\/|_emu\/v1\/|[a-z]+\/v\d[a-z0-9]*\/)/

async function runtime(request: APIRequestContext) {
  const info = (await (await request.get('/_emu/v1/info')).json()) as {
    services: string[]
    runtime?: { reachable?: boolean }
  }
  return {
    nat: info.services.includes('nat'),
    gke: info.services.includes('gke'),
    reachable: !!info.runtime?.reachable,
  }
}

/** compute sends a compute v1 write and waits for its Operation. */
async function compute(request: APIRequestContext, method: string, path: string, data?: unknown) {
  const res = await request.fetch(`/compute/v1/${path}`, { method, data })
  expect(res.status(), await res.text()).toBe(200)
  const op = (await res.json()) as { selfLink: string }
  const wait = await request.post(
    `/compute/v1/${op.selfLink.slice(op.selfLink.indexOf('projects/'))}/wait`,
  )
  const done = (await wait.json()) as { error?: unknown }
  expect(done.error, JSON.stringify(done)).toBeUndefined()
}

/** sinkOn turns the offline sink on with its switch, if it is off. */
async function sinkOn(page: Page) {
  const sw = page.getByRole('switch', { name: 'Offline sink' })
  if ((await sw.getAttribute('aria-checked')) !== 'true') await sw.click()
  await expect(sw).toHaveAttribute('aria-checked', 'true')
  await expect(page.getByText(/TCP egress a Cloud NAT allows is recorded/)).toBeVisible()
}

test('Cloud NAT: a router’s NAT gateways, their coverage and the offline sink', async ({
  page,
  request,
  browserName,
  requests,
}, testInfo) => {
  test.skip(!(await runtime(request)).nat, 'Cloud NAT runs on Linux only')
  // Eight compute Operations of 2 s each (--lro-latency), and the pages between them.
  test.setTimeout(3 * 60_000)
  const id = projectId(`e2e-nat${testInfo.retry}`, browserName)
  await compute(request, 'POST', `projects/${id}/global/networks`, {
    name: 'vpc',
    autoCreateSubnetworks: false,
  })
  for (const [name, cidr] of [
    ['nodes', '10.30.0.0/24'],
    ['db', '10.30.1.0/24'],
  ] as const) {
    await compute(request, 'POST', `projects/${id}/regions/us-central1/subnetworks`, {
      name,
      network: `projects/${id}/global/networks/vpc`,
      ipCidrRange: cidr,
      ...(name === 'nodes'
        ? { secondaryIpRanges: [{ rangeName: 'pods', ipCidrRange: '10.31.0.0/16' }] }
        : {}),
    })
  }

  await page.goto(`/console/nat?project=${id}`)
  await expect(page.getByText('No Cloud Routers in this Project yet')).toBeVisible()
  await sinkOn(page)

  // Create a router; the form checks what the API checks, with its messages.
  await page.getByRole('link', { name: 'Create router' }).click()
  await page.getByLabel('Name').fill('Edge')
  await page.getByLabel('Network').fill('vpc')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page.getByText("Invalid value for field 'resource.name': 'Edge'")).toBeVisible()
  await page.getByLabel('Name').fill('edge')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(
    new RegExp(`/console/nat/regions/us-central1/routers/edge\\?project=${id}$`),
  )
  await expect(page.getByText('No NAT gateways on this router')).toBeVisible()

  // Add a NAT for one subnetwork's ranges.
  await page.getByRole('link', { name: 'Add NAT gateway' }).click()
  await page.getByLabel('Name').fill('nat-a')
  await page.getByLabel('NAT IP allocation').selectOption('MANUAL_ONLY')
  await page.getByRole('button', { name: 'Add' }).click()
  await expect(
    page.getByText(
      "Invalid value for field 'resource.nats[0].natIps': ''. At least one NAT IP is required with MANUAL_ONLY allocation.",
    ),
  ).toBeVisible()
  await page.getByLabel('NAT IP allocation').selectOption('AUTO_ONLY')
  await page.getByLabel('Source ranges').selectOption('LIST_OF_SUBNETWORKS')
  await page.getByLabel('Ranges of nodes').selectOption('ALL_IP_RANGES')
  await page.getByLabel('Logging').check()
  await page.getByRole('button', { name: 'Add' }).click()
  await expect(page).toHaveURL(
    new RegExp(`/console/nat/regions/us-central1/routers/edge/nats/nat-a\\?project=${id}$`),
  )
  const covered = page.getByRole('table', { name: 'Covered subnetworks' }).getByTestId('covered')
  await expect(covered).toHaveCount(1)
  await expect(covered).toContainText('10.30.0.0/24')
  await expect(covered).toContainText('10.31.0.0/16')
  const settings = page.getByLabel('NAT gateway settings')
  await expect(settings).toContainText('Automatic')
  await expect(settings).toContainText('Translations and errors')
  await expect(page.getByText('No endpoints have NAT ports yet.')).toBeVisible()

  // Edit it: the db subnetwork's primary range too, and more ports.
  await page.getByRole('link', { name: 'Edit NAT gateway' }).click()
  await page.getByLabel('Ranges of db').selectOption('PRIMARY_IP_RANGE')
  await page.getByLabel('Minimum ports per VM').fill('128')
  await page.getByRole('tab', { name: 'JSON' }).click()
  const body = JSON.parse(await page.getByLabel('Request body').inputValue()) as {
    minPortsPerVm: number
    subnetworks: { name: string }[]
  }
  expect(body.minPortsPerVm).toBe(128)
  expect(body.subnetworks.map((s) => s.name.split('/').pop())).toEqual(['nodes', 'db'])
  await page.getByRole('tab', { name: 'Form' }).click()
  await page.getByRole('button', { name: 'Save' }).click()
  await expect(settings).toContainText('128')
  await expect(covered).toHaveCount(2)

  // The router list shows the NAT and what it covers.
  await page.getByRole('link', { name: 'Cloud NAT', exact: true }).first().click()
  const row = page.getByTestId('router').filter({ hasText: 'edge' })
  await expect(row).toContainText('nat-a')
  await expect(row).toContainText('db, nodes')

  // Delete the NAT, then the router.
  await row.getByRole('link', { name: 'nat-a' }).click()
  await page.getByRole('button', { name: 'Delete NAT gateway' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete NAT gateway' }).click()
  await expect(page.getByText('No NAT gateways on this router')).toBeVisible()
  await page.getByRole('button', { name: 'Delete router' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete router' }).click()
  await expect(page.getByText('No Cloud Routers in this Project yet')).toBeVisible()

  for (const r of requests) {
    expect(new URL(r.url()).pathname, r.url()).toMatch(allowed)
    expect(await r.headerValue('authorization'), r.url()).toBeNull()
  }
})

test('Cloud NAT: a private cluster’s egress in the translation log', async ({
  page,
  request,
  browserName,
}, testInfo) => {
  const rt = await runtime(request)
  test.skip(!rt.nat || !rt.gke || !rt.reachable, 'needs Cloud NAT, GKE and a container runtime')
  test.setTimeout(10 * 60_000)
  const id = projectId(`e2e-natgke${testInfo.retry}`, browserName)
  const zone = 'us-central1-a'

  // A private cluster: its nodes reach the internet only through a NAT.
  const created = await request.post(`/container/v1/projects/${id}/zones/${zone}/clusters`, {
    data: {
      cluster: {
        name: 'priv',
        privateClusterConfig: { enablePrivateNodes: true, masterIpv4CidrBlock: '172.16.0.0/28' },
        nodePools: [{ name: 'np', initialNodeCount: 1 }],
      },
    },
  })
  expect(created.status(), await created.text()).toBe(200)

  // A NAT for the default network's us-central1 subnetwork, from the console.
  await page.goto(`/console/nat/create?project=${id}`)
  await page.getByLabel('Name').fill('egress')
  await page.getByLabel('Network').fill('default')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(/\/routers\/egress\?/)
  await page.getByRole('link', { name: 'Add NAT gateway' }).click()
  await page.getByLabel('Name').fill('nat')
  await page.getByRole('button', { name: 'Add' }).click()
  await expect(page).toHaveURL(/\/routers\/egress\/nats\/nat\?/)
  await page.goto(`/console/nat?project=${id}`)
  await sinkOn(page)

  // Once the cluster runs, a pod's requests leave through the NAT.
  const cluster = `/container/v1/projects/${id}/locations/${zone}/clusters/priv`
  await expect
    .poll(async () => ((await (await request.get(cluster)).json()) as { status?: string }).status, {
      timeout: 5 * 60_000,
    })
    .toBe('RUNNING')
  const gateway = `/connectgateway/v1/projects/${id}/locations/${zone}/gkeMemberships/priv`
  const pod = await request.post(`${gateway}/api/v1/namespaces/default/pods`, {
    data: {
      apiVersion: 'v1',
      kind: 'Pod',
      metadata: { name: 'egress' },
      spec: {
        containers: [
          {
            name: 'egress',
            image: 'rancher/mirrored-library-busybox:1.37.0',
            imagePullPolicy: 'IfNotPresent',
            command: [
              'sh',
              '-c',
              'while true; do wget -q -T 3 -O - http://192.0.2.1/; echo; sleep 2; done',
            ],
          },
        ],
      },
    },
  })
  expect(pod.status(), await pod.text()).toBe(201)

  await page.goto(`/console/nat/regions/us-central1/routers/egress/nats/nat?project=${id}`)
  // The node has NAT ports (as has the control plane, on the same subnetwork).
  await expect(
    page.getByRole('table', { name: 'Port usage' }).getByTestId('mapping').filter({
      hasText: 'gke-priv-np-',
    }),
  ).toHaveCount(1, { timeout: 2 * 60_000 })
  // The pod's requests are answered by the sink, live.
  const sunk = page
    .getByRole('table', { name: 'Connections' })
    .getByTestId('connection')
    .filter({ hasText: '192.0.2.1:80' })
  await expect(sunk.first()).toContainText('Offline sink', { timeout: 3 * 60_000 })
  await sunk
    .first()
    .getByRole('button', { name: /Details of/ })
    .click()
  await expect(page.getByLabel(/^Connection /)).toContainText('GET / HTTP/1.1')

  // The Project's translation log has them too, filtered by the URL.
  await page.goto(`/console/nat/translations?project=${id}&dst=192.0.2.1&outcome=OFFLINE_SINK`)
  await expect(page.getByTestId('connection').first()).toContainText('egress/nat')

  const deleted = await request.delete(cluster)
  expect(deleted.status(), await deleted.text()).toBe(200)
  await expect
    .poll(async () => (await request.get(cluster)).status(), { timeout: 2 * 60_000 })
    .toBe(404)
})
