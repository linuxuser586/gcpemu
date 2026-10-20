import { expect, expectConsoleRequests, projectId, test } from './fixtures'

// The VPC network view (SRS 4.8.3, FR-UI-011) against the Instance's
// compute Service: a custom mode network and a subnetwork are created,
// edited and deleted in the console, through the public compute API.

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
