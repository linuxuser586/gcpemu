import type { APIRequestContext } from '@playwright/test'

import { expect, expectConsoleRequests, projectId, test } from './fixtures'

// The VPC network view (SRS 4.8.3, FR-UI-011) against the Instance's
// compute Service: a custom mode network and a subnetwork are created,
// edited and deleted in the console, through the public compute API.
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
