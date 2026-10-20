import type { APIRequestContext } from '@playwright/test'

import { expect, expectConsoleRequests, projectId, test } from './fixtures'

// The VPC network view (SRS 4.8.3, FR-UI-011) against the Instance's
// compute Service: a custom mode network and a subnetwork, a network's
// firewall rules, routes and peerings, addresses, and a private services
// access connection are created, edited and deleted in the console,
// through the public compute and servicenetworking APIs.
// A subnetwork's VPC container network needs a container runtime, so that
// test skips without one.

const allowed = /^\/(console\/|_emu\/v1\/|[a-z]+\/v\d[a-z0-9]*\/)/

test('VPC network: create, edit and delete a network and a subnetwork', async ({
  page,
  browserName,
  requests,
}, testInfo) => {
  // Six compute Operations of 2 s each (--lro-latency), and the pages between them.
  test.setTimeout(2 * 60_000)
  const id = projectId(`e2e-vpc${testInfo.retry}`, browserName)
  await page.goto(`/console/compute?project=${id}`)
  await expect(page.getByRole('heading', { name: 'VPC network' })).toBeVisible()

  // Create a custom mode network; the form checks what the API checks.
  await page.getByRole('link', { name: 'Create network' }).click()
  await expect(page).toHaveURL(/\/console\/compute\/create-network\?/)
  await page.getByLabel('Name').fill('Main')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page.getByText("Invalid value for field 'resource.name': 'Main'")).toBeVisible()
  await page.getByLabel('Name').fill('main')
  await page.getByLabel('Subnet creation mode').selectOption('custom')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/compute/global/networks/main\\?project=${id}$`))
  await expect(page.getByLabel('Network details')).toContainText('Custom')
  await expect(page.getByText('No subnetworks in this network yet')).toBeVisible()

  // Add a subnetwork to it.
  await page.getByRole('link', { name: 'Add subnetwork' }).click()
  await expect(page).toHaveURL(/\/console\/compute\/create-subnetwork\?/)
  await expect(page.getByRole('combobox', { name: 'Network', exact: true })).toHaveValue('main')
  await page.getByLabel('Name').fill('apps')
  await page.getByLabel('Primary range').fill('10.40.0.0/24')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(
    new RegExp(`/console/compute/regions/us-central1/subnetworks/apps\\?project=${id}$`),
  )
  const details = page.getByLabel('Subnetwork details')
  await expect(details).toContainText('10.40.0.0/24')
  await expect(details).toContainText('10.40.0.1')

  // Edit it: a secondary range and a description, then a wider range.
  await page.getByRole('link', { name: 'Edit subnetwork' }).click()
  await expect(page).toHaveURL(/\/subnetworks\/apps\/edit\?/)
  await page.getByLabel('Description').fill('Apps')
  await page.getByLabel('Secondary ranges').fill('pods 10.41.0.0/16')
  await page.getByRole('button', { name: 'Save' }).click()
  await expect(details).toContainText('pods 10.41.0.0/16')
  await expect(details).toContainText('Apps')
  await page.getByRole('button', { name: 'Expand range' }).click()
  await page.getByRole('dialog').getByLabel('New range').fill('10.40.0.0/22')
  await page.getByRole('dialog').getByRole('button', { name: 'Expand' }).click()
  await expect(details).toContainText('10.40.0.0/22')

  // The network lists it.
  await page.getByLabel('Subnetwork details').getByRole('link', { name: 'main' }).click()
  const row = page.getByRole('table', { name: 'Subnetworks' }).getByTestId('subnetwork')
  await expect(row).toContainText('apps')
  await expect(row).toContainText('10.40.0.0/22')

  // Delete the subnetwork, then the network.
  await row.getByRole('link', { name: 'apps' }).click()
  await page.getByRole('button', { name: 'Delete subnetwork' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete subnetwork' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/compute/global/networks/main\\?project=${id}$`))
  await expect(page.getByText('No subnetworks in this network yet')).toBeVisible()
  await page.getByRole('button', { name: 'Delete network' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete network' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/compute\\?project=${id}$`))
  await expect(page.getByRole('heading', { name: 'VPC network' })).toBeVisible()
  await expect(page.getByRole('link', { name: 'main', exact: true })).toHaveCount(0)

  await expectConsoleRequests(requests, { allowed })
})

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

test('VPC network: a subnetwork’s container network', async ({
  page,
  request,
  browserName,
  requests,
}, testInfo) => {
  const info = (await (await request.get('/_emu/v1/info')).json()) as {
    runtime?: { reachable?: boolean }
  }
  test.skip(!info.runtime?.reachable, 'needs a container runtime')
  test.setTimeout(2 * 60_000)
  const id = projectId(`e2e-vpcnet${testInfo.retry}`, browserName)
  const sp = `projects/${id}/regions/us-central1/subnetworks/apps`
  await compute(request, 'POST', `projects/${id}/global/networks`, {
    name: 'main',
    autoCreateSubnetworks: false,
  })
  await compute(request, 'POST', `projects/${id}/regions/us-central1/subnetworks`, {
    name: 'apps',
    network: `projects/${id}/global/networks/main`,
    ipCidrRange: '10.43.0.0/24',
  })

  // None until something uses the subnetwork.
  await page.goto(`/console/compute/regions/us-central1/subnetworks/apps?project=${id}`)
  const panel = page.getByRole('region', { name: 'Container network' })
  await expect(panel).toContainText('No container network yet')

  // Asking for its Host address realises it, as a Managed container would.
  await request.get(`/_emu/v1/host/address?subnetwork=${encodeURIComponent(sp)}`)
  await page.reload()
  const details = panel.getByLabel('Container network details')
  await expect(details).toContainText('Declared range10.43.0.0/24')
  await expect(details).toContainText(/gcpemu-.*-vpc-main-apps-/)
  await expect(details).toContainText('Gateway address10.43.0.1')

  // Its network sums it up.
  await page.goto(`/console/compute/global/networks/main?project=${id}`)
  const row = page
    .getByRole('table', { name: 'Container networks' })
    .getByTestId('container-network')
  await expect(row).toContainText('apps')
  await expect(row).toContainText('10.43.0.0/24')

  // Nothing is deleted: on a VM runtime the Host address keeps the
  // subnetwork in use, and the Instance is Ephemeral.
  await expectConsoleRequests(requests, { allowed })
})

test('VPC network: create, edit and delete a firewall rule, create and delete a route', async ({
  page,
  request,
  browserName,
  requests,
}, testInfo) => {
  // Five compute Operations of 2 s each (--lro-latency), and the pages between them.
  test.setTimeout(2 * 60_000)
  const id = projectId(`e2e-vpcfw${testInfo.retry}`, browserName)
  await compute(request, 'POST', `projects/${id}/global/networks`, {
    name: 'main',
    autoCreateSubnetworks: false,
  })
  await page.goto(`/console/compute/global/networks/main?project=${id}`)
  await expect(page.getByText('Recorded: stored and returned, not enforced')).toBeVisible()
  const defaultRoute = page.getByRole('table', { name: 'Routes' }).getByTestId('route')
  await expect(defaultRoute).toContainText('System-generated')
  await expect(defaultRoute).toContainText('Gateway default-internet-gateway')
  await expect(page.getByText('No firewall rules in this network.')).toBeVisible()

  // Create a firewall rule on the network.
  await page.getByRole('link', { name: 'Add firewall rule' }).click()
  await expect(page).toHaveURL(/\/console\/compute\/create-firewall\?/)
  await expect(page.getByRole('combobox', { name: 'Network', exact: true })).toHaveValue('main')
  await page.getByLabel('Name').fill('allow-web')
  await page.getByLabel('Protocols and ports').fill('tcp:80,tcp:443')
  await page.getByLabel('Source ranges').fill('10.0.0.0/8')
  await page.getByLabel('Target tags').fill('web')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(
    new RegExp(`/console/compute/global/firewalls/allow-web\\?project=${id}$`),
  )
  const details = page.getByLabel('Firewall rule details')
  await expect(details).toContainText('tcp:80,tcp:443')
  await expect(details).toContainText('10.0.0.0/8')
  await expect(details).toContainText('Tags: web')

  // Edit it: deny instead, at the highest priority.
  await page.getByRole('link', { name: 'Edit firewall rule' }).click()
  await expect(page).toHaveURL(/\/firewalls\/allow-web\/edit\?/)
  await page.getByLabel('Action').selectOption('DENY')
  await page.getByLabel('Protocols and ports').fill('tcp:23')
  await page.getByLabel('Priority').fill('0')
  await page.getByRole('button', { name: 'Save' }).click()
  await expect(details).toContainText('Deny')
  await expect(details).toContainText('tcp:23')
  await expect(details).toContainText('Priority0')

  // Delete it.
  await page.getByRole('button', { name: 'Delete firewall rule' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete firewall rule' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/compute/firewalls\\?project=${id}$`))
  await expect(page.getByText('No firewall rules in this Project yet.')).toBeVisible()

  // Create a route from the Routes tab.
  await page.getByRole('link', { name: 'Routes' }).click()
  await page.getByRole('link', { name: 'Create route' }).click()
  await expect(page).toHaveURL(/\/console\/compute\/create-route\?/)
  await page.getByLabel('Name').fill('via-ip')
  await page.getByRole('combobox', { name: 'Network', exact: true }).fill('main')
  await page.getByLabel('Destination range').fill('10.60.0.0/16')
  await page.getByLabel('Next hop', { exact: true }).selectOption('nextHopIp')
  await page.getByLabel('Next hop IP address').fill('10.0.0.5')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/compute/global/routes/via-ip\\?project=${id}$`))
  await expect(page.getByLabel('Route details')).toContainText('IP 10.0.0.5')

  // Delete it; the network's default route stays, read-only.
  await page.getByRole('button', { name: 'Delete route' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete route' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/compute/routes\\?project=${id}$`))
  const rows = page.getByRole('table', { name: 'Routes' }).getByTestId('route')
  await expect(rows).toHaveCount(1)
  await rows.getByRole('link').first().click()
  await expect(page.getByLabel('Route details')).toContainText('0.0.0.0/0')
  await expect(page.getByText(/^System-generated: made by GCP/)).toBeVisible()
  await expect(page.getByRole('button', { name: 'Delete route' })).toHaveCount(0)

  await expectConsoleRequests(requests, { allowed })
})

test('VPC network: reserve and release a regional external and a global address', async ({
  page,
  browserName,
  requests,
}, testInfo) => {
  // Four compute Operations of 2 s each (--lro-latency), and the pages between them.
  test.setTimeout(2 * 60_000)
  const id = projectId(`e2e-vpcip${testInfo.retry}`, browserName)
  await page.goto(`/console/compute/addresses?project=${id}`)
  await expect(page.getByText('No reserved addresses in this Project yet.')).toBeVisible()

  // Reserve a regional external address; the form checks what the API checks.
  await page.getByRole('link', { name: 'Reserve address' }).click()
  await expect(page).toHaveURL(/\/console\/compute\/create-address\?/)
  await page.getByLabel('Name').fill('Web_ip')
  await page.getByRole('button', { name: 'Reserve' }).click()
  await expect(page.getByText("Invalid value for field 'resource.name': 'Web_ip'")).toBeVisible()
  await page.getByLabel('Name').fill('web-ip')
  await page.getByRole('button', { name: 'Reserve' }).click()
  await expect(page).toHaveURL(
    new RegExp(`/console/compute/regions/us-central1/addresses/web-ip\\?project=${id}$`),
  )
  const details = page.getByLabel('Address details')
  await expect(details).toContainText('RESERVED')
  // Regional external addresses come from 34.16.0.0/12.
  await expect(details).toContainText(/34\.(1[6-9]|2\d|3[01])\./)

  // Reserve a global one.
  await page.goto(`/console/compute/create-address?project=${id}`)
  await page.getByLabel('Name').fill('lb-ip')
  await page.getByLabel('Scope').selectOption('global')
  await page.getByRole('button', { name: 'Reserve' }).click()
  await expect(page).toHaveURL(
    new RegExp(`/console/compute/global/addresses/lb-ip\\?project=${id}$`),
  )
  await expect(details).toContainText('Global')
  // Global ones from 34.120.0.0/14.
  await expect(details).toContainText(/34\.12[0-3]\./)

  // Both are listed.
  await page.goto(`/console/compute/addresses?project=${id}`)
  const rows = page.getByRole('table', { name: 'IP addresses' }).getByTestId('address')
  await expect(rows).toHaveCount(2)
  await expect(rows.nth(0)).toContainText('lb-ip')
  await expect(rows.nth(0)).toContainText('Global')
  await expect(rows.nth(1)).toContainText('web-ip')
  await expect(rows.nth(1)).toContainText('us-central1')

  // Release both.
  for (const name of ['lb-ip', 'web-ip']) {
    await rows.getByRole('link', { name }).click()
    await page.getByRole('button', { name: 'Release address' }).click()
    await page.getByRole('dialog').getByRole('button', { name: 'Release address' }).click()
    await expect(page).toHaveURL(new RegExp(`/console/compute/addresses\\?project=${id}$`))
  }
  await expect(page.getByText('No reserved addresses in this Project yet.')).toBeVisible()

  await expectConsoleRequests(requests, { allowed })
})

test('VPC network: peer two networks; allocate a private services access range and connect it', async ({
  page,
  request,
  browserName,
  requests,
}, testInfo) => {
  // Seven compute and servicenetworking Operations of 2 s each (--lro-latency), and the pages between them.
  test.setTimeout(3 * 60_000)
  const id = projectId(`e2e-vpcpeer${testInfo.retry}`, browserName)
  for (const name of ['main', 'hub']) {
    await compute(request, 'POST', `projects/${id}/global/networks`, {
      name,
      autoCreateSubnetworks: false,
    })
  }
  await page.goto(`/console/compute/global/networks/main?project=${id}`)
  await expect(page.getByText('No peerings in this network.')).toBeVisible()

  // Peer main with hub; it is active once hub peers back.
  await page.getByRole('link', { name: 'Add peering' }).click()
  await expect(page).toHaveURL(/\/networks\/main\/add-peering\?/)
  await page.getByLabel('Name').fill('main-to-hub')
  await page.getByRole('combobox', { name: 'Peer network' }).fill('hub')
  await page.getByLabel('Import custom routes').check()
  await page.getByRole('button', { name: 'Add' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/compute/global/networks/main\\?project=${id}$`))
  const peering = page.getByRole('table', { name: 'Peerings' }).getByTestId('peering')
  await expect(peering).toContainText('INACTIVE')
  await expect(peering).toContainText('Import')
  await compute(request, 'POST', `projects/${id}/global/networks/hub/addPeering`, {
    networkPeering: { name: 'hub-to-main', network: `projects/${id}/global/networks/main` },
  })
  await page.reload()
  await expect(peering).toContainText('ACTIVE')
  await expect(peering).not.toContainText('INACTIVE')

  // Remove it.
  await peering.getByRole('button', { name: 'Remove peering main-to-hub' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Remove peering' }).click()
  await expect(page.getByText('No peerings in this network.')).toBeVisible()

  // Allocate a private services access range from the network.
  await page.getByRole('link', { name: 'Allocate range' }).click()
  await expect(page).toHaveURL(/\/console\/compute\/create-address\?/)
  await page.getByLabel('Name').fill('psa-range')
  await page.getByLabel('Prefix length').fill('16')
  await page.getByRole('button', { name: 'Reserve' }).click()
  await expect(page).toHaveURL(
    new RegExp(`/console/compute/global/addresses/psa-range\\?project=${id}$`),
  )
  await expect(page.getByLabel('Address details')).toContainText('VPC_PEERING')

  // Connect the network over it.
  await page.goto(`/console/compute/global/networks/main?project=${id}`)
  const range = page.getByRole('table', { name: 'Allocated ranges' }).getByTestId('allocation')
  await expect(range).toContainText('/16')
  await expect(range).toContainText('Not connected')
  await page.getByRole('link', { name: 'Connect', exact: true }).click()
  await expect(page).toHaveURL(/\/networks\/main\/connection\?/)
  await page.getByLabel(/psa-range/).check()
  await page.getByRole('button', { name: 'Connect' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/compute/global/networks/main\\?project=${id}$`))
  await expect(page.getByLabel('Connection details')).toContainText('psa-range')
  await expect(range).toContainText('Connected')
  await expect(peering).toContainText('servicenetworking-googleapis-com')

  // Delete the connection, then release the range.
  await page.getByRole('button', { name: 'Delete connection' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete connection' }).click()
  await expect(page.getByText(/No private services access connection/)).toBeVisible()
  await range.getByRole('link', { name: 'psa-range' }).click()
  await page.getByRole('button', { name: 'Release address' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Release address' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/compute/addresses\\?project=${id}$`))
  await expect(page.getByText('No reserved addresses in this Project yet.')).toBeVisible()

  await expectConsoleRequests(requests, { allowed })
})
