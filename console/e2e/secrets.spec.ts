import type { Page } from '@playwright/test'

import { expect, projectId, test } from './fixtures'

// The Secret Manager view (SRS 4.8.3, FR-UI-011) against the Instance's
// secrets and pubsub Services. Each attempt has its own Project, so a
// retry does not meet the secrets of the one before.

// The console's paths: its bundle, the admin API and public GCP API paths.
const allowed = /^\/(console\/|_emu\/v1\/|[a-z]+\/v\d[a-z0-9]*\/)/

const versionRow = (page: Page, id: string) =>
  page.getByTestId('version').filter({ has: page.getByRole('cell', { name: id, exact: true }) })

test('Secret Manager: a secret’s lifecycle, its versions and their values', async ({
  page,
  browserName,
  requests,
}, testInfo) => {
  const id = projectId(`e2e-sm${testInfo.retry}`, browserName)
  await page.goto(`/console/secrets?project=${id}`)
  await expect(page.getByText('No global secrets in this Project yet.')).toBeVisible()
  await page.getByRole('link', { name: 'Create secret' }).click()

  await page.getByLabel('Name').fill('db password')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page.getByText('Secret ID "db password" is invalid')).toBeVisible()

  // The API rejects what the form cannot know is wrong: a missing topic.
  await page.getByLabel('Name').fill('db-password')
  await page.getByLabel('Secret value').fill('first-value')
  await page.getByLabel('Labels').fill('team=e2e')
  await page.getByLabel('Pub/Sub topics').fill(`projects/${id}/topics/missing`)
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(
    page
      .getByRole('alert')
      .filter({ hasText: `Pub/Sub topic projects/${id}/topics/missing not found.` }),
  ).toBeVisible()
  await page.getByLabel('Pub/Sub topics').fill('')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/secrets/secrets/db-password\\?project=${id}$`))

  // Version 1 holds the value, masked until revealed.
  await expect(versionRow(page, '1')).toContainText('latest')
  await versionRow(page, '1').getByRole('button', { name: 'View value of version 1' }).click()
  const dialog = page.getByRole('dialog')
  await expect(dialog.getByLabel('Secret value')).not.toContainText('first-value')
  await dialog.getByRole('button', { name: 'Reveal' }).click()
  await expect(dialog.getByLabel('Secret value')).toHaveText('first-value')
  await page.keyboard.press('Escape')

  // A second version becomes latest; disable, enable and destroy the first.
  await page.getByRole('button', { name: 'Add version' }).click()
  await page.getByRole('dialog').getByLabel('Secret value').fill('second-value')
  await page.getByRole('dialog').getByRole('button', { name: 'Add version' }).click()
  await expect(versionRow(page, '2')).toContainText('latest')
  await page.getByRole('button', { name: 'Disable version 1' }).click()
  await expect(versionRow(page, '1')).toContainText('DISABLED')
  await page.getByRole('button', { name: 'Enable version 1' }).click()
  await expect(versionRow(page, '1')).toContainText('ENABLED')
  await page.getByRole('button', { name: 'Destroy version 1' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Destroy version' }).click()
  await expect(versionRow(page, '1')).toContainText('DESTROYED')

  // Edit: an alias to version 2 and new labels.
  await page.getByRole('link', { name: 'Edit secret' }).click()
  await page.getByLabel('Labels').fill('team=platform')
  await page.getByLabel('Version aliases').fill('prod=2')
  await page.getByRole('button', { name: 'Save' }).click()
  const details = page.getByLabel('Secret details')
  await expect(details).toContainText('team=platform')
  await expect(details).toContainText('prod=2')
  await page.getByRole('link', { name: 'Versions' }).click()
  await expect(versionRow(page, '2')).toContainText('prod')

  // The list shows it; then delete it.
  await page.getByRole('link', { name: 'Secrets', exact: true }).click()
  await expect(page.getByTestId('secret').filter({ hasText: 'db-password' })).toBeVisible()
  await page.getByRole('link', { name: 'db-password' }).click()
  await page.getByRole('button', { name: 'Delete secret' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete secret' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/secrets\\?project=${id}$`))
  await expect(page.getByText('No global secrets in this Project yet.')).toBeVisible()

  for (const r of requests) {
    expect(new URL(r.url()).pathname, r.url()).toMatch(allowed)
    expect(await r.headerValue('authorization'), r.url()).toBeNull()
  }
})

test('Secret Manager: regional secrets live in their location', async ({
  page,
  browserName,
}, testInfo) => {
  const id = projectId(`e2e-smr${testInfo.retry}`, browserName)
  await page.goto(`/console/secrets?project=${id}`)
  await page.getByLabel('Location').selectOption('us-central1')
  await expect(page.getByText('No secrets in us-central1 yet.')).toBeVisible()
  await page.getByRole('link', { name: 'Create secret' }).click()
  await expect(page.getByLabel('Location')).toHaveValue('us-central1')
  await expect(page.getByLabel('Replication')).toHaveCount(0)
  await page.getByLabel('Name').fill('regional')
  await page.getByLabel('Secret value').fill('in-iowa')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(
    new RegExp(`/console/secrets/locations/us-central1/secrets/regional\\?project=${id}`),
  )
  await page.getByRole('link', { name: 'Overview' }).click()
  await expect(page.getByLabel('Secret details')).toContainText('Regional (us-central1)')
  await page.getByRole('link', { name: 'Secrets', exact: true }).click()
  await expect(page.getByTestId('secret').filter({ hasText: 'regional' })).toBeVisible()
  await page.getByLabel('Location').selectOption('')
  await expect(page.getByText('No global secrets in this Project yet.')).toBeVisible()
})
