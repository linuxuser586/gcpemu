import type { Page } from '@playwright/test'

import { expect, expectConsoleRequests, projectId, test } from './fixtures'

// The Cloud DNS view (SRS 4.8.3, FR-UI-011) against the Instance's dns
// Service and its DNS server. Each attempt has its own Project and DNS
// names, so that browsers and retries do not answer for each other.

// The console's paths: its bundle, the admin API and public GCP API paths.
const allowed = /^\/(console\/|_emu\/v1\/|[a-z]+\/v\d[a-z0-9]*\/)/

const rrsetRow = (page: Page, name: string, type: string) =>
  page
    .getByTestId('rrset')
    .filter({ has: page.getByRole('cell', { name, exact: true }) })
    .filter({ has: page.getByRole('cell', { name: type, exact: true }) })

test('Cloud DNS: a zone’s record sets through changes, and the query box', async ({
  page,
  browserName,
  requests,
}, testInfo) => {
  const id = projectId(`e2e-dns${testInfo.retry}`, browserName)
  const domain = `${id}.test.`
  await page.goto(`/console/dns?project=${id}`)
  await expect(page.getByText('No managed zones in this Project yet.')).toBeVisible()
  await page.getByRole('link', { name: 'Create zone' }).click()

  // The form checks what the API checks, with its messages.
  await page.getByLabel('Zone name').fill('app')
  await page.getByLabel('DNS name').fill(domain.slice(0, -1))
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(
    page.getByText(`Invalid value for 'entity.managedZone.dnsName': '${domain.slice(0, -1)}'`),
  ).toBeVisible()
  await page.getByLabel('DNS name').fill(domain)
  await page.getByLabel('Description').fill('e2e zone')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/dns/zones/app\\?project=${id}$`))
  await expect(rrsetRow(page, domain, 'SOA')).toBeVisible()
  await expect(rrsetRow(page, domain, 'NS')).toBeVisible()

  // Add a record set; the API rejects data the form cannot judge.
  await page.getByRole('link', { name: 'Add record set' }).click()
  await page.getByLabel('DNS name').fill('www')
  await page.getByLabel('Data').fill('not-an-ip')
  await page.getByRole('button', { name: 'Add' }).click()
  await expect(
    page
      .getByRole('alert')
      .filter({ hasText: "Invalid value for 'entity.change.additions[0].rrdata[0]': 'not-an-ip'" })
      .first(),
  ).toBeVisible()
  await page.getByLabel('Data').fill('192.0.2.10')
  await page.getByRole('button', { name: 'Add' }).click()
  await expect(rrsetRow(page, `www.${domain}`, 'A')).toContainText('192.0.2.10')

  // The DNS server answers with it.
  await rrsetRow(page, `www.${domain}`, 'A')
    .getByRole('link', { name: `Query www.${domain} (A)` })
    .click()
  const response = page.getByRole('region', { name: 'DNS response' })
  await expect(response.getByText('NOERROR')).toBeVisible()
  await expect(response.getByRole('table', { name: 'Answer' })).toContainText('192.0.2.10')
  await response.getByRole('link', { name: `projects/${id}/managedZones/app` }).click()

  // Edit it: one change replaces it, and the answer follows.
  await page.getByRole('link', { name: `Edit www.${domain} (A)` }).click()
  await page.getByLabel('Data').fill('192.0.2.20\n192.0.2.21')
  await page.getByRole('button', { name: 'Save' }).click()
  await expect(rrsetRow(page, `www.${domain}`, 'A')).toContainText('192.0.2.21')
  await page.getByRole('link', { name: 'Query', exact: true }).click()
  await page.getByLabel('Name', { exact: true }).fill(`www.${domain}`)
  await page.getByLabel('Type').selectOption('A')
  await page.getByRole('button', { name: 'Resolve' }).click()
  await expect(response.getByRole('table', { name: 'Answer' })).toContainText('192.0.2.21')
  await expect(response.getByRole('table', { name: 'Answer' })).not.toContainText('192.0.2.10')

  // The change history has the zone's creation and both edits.
  await page.goto(`/console/dns/zones/app/changes?project=${id}`)
  await expect(page.getByTestId('change')).toHaveCount(3)
  await expect(page.getByTestId('change').first()).toContainText('+ www.')
  await expect(page.getByTestId('change').first()).toContainText('192.0.2.21')

  // Deleting the zone deletes its record sets first.
  await page.getByRole('button', { name: 'Delete zone' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete zone' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/dns\\?project=${id}$`))
  await expect(page.getByText('No managed zones in this Project yet.')).toBeVisible()

  await expectConsoleRequests(requests, { allowed })
})

test('Cloud DNS: a private zone answers only on its networks', async ({
  page,
  browserName,
}, testInfo) => {
  const id = projectId(`e2e-dnsp${testInfo.retry}`, browserName)
  const domain = `${id}.internal.`
  const network = `projects/${id}/global/networks/vpc-e2e`
  await page.goto(`/console/dns/create?project=${id}`)
  await page.getByLabel('Zone name').fill('corp')
  await page.getByLabel('DNS name').fill(domain)
  await page.getByLabel('Visibility').selectOption('private')
  await page.getByLabel('Networks').fill('vpc-e2e')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/dns/zones/corp\\?project=${id}$`))

  await page.getByRole('link', { name: 'Add record set' }).click()
  await page.getByLabel('DNS name').fill('db')
  await page.getByLabel('Data').fill('10.0.0.5')
  await page.getByRole('button', { name: 'Add' }).click()
  await expect(rrsetRow(page, `db.${domain}`, 'A')).toContainText('10.0.0.5')

  await page.getByRole('link', { name: 'Overview' }).click()
  await expect(page.getByRole('list', { name: 'Network bindings' })).toContainText('vpc-e2e')

  // From the bound network the zone answers; from the host it is unseen.
  await page.goto(
    `/console/dns/query?project=${id}&name=db.${domain}&type=A&network=${encodeURIComponent(network)}`,
  )
  const response = page.getByRole('region', { name: 'DNS response' })
  await expect(response.getByText('NOERROR')).toBeVisible()
  await expect(response.getByRole('table', { name: 'Answer' })).toContainText('10.0.0.5')
  await page.getByLabel('From').selectOption('')
  await page.getByRole('button', { name: 'Resolve' }).click()
  await expect(response).toContainText('Host resolver (forwarded)')
  await expect(response.getByRole('table', { name: 'Answer' })).toHaveCount(0)
})
