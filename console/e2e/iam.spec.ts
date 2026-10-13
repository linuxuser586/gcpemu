import { readFileSync } from 'node:fs'

import type { Page } from '@playwright/test'

import { expect, expectConsoleRequests, projectId, test } from './fixtures'

// The IAM view (SRS 4.8.3, FR-UI-011) against the Instance's iam and gcs
// Services. Each attempt has its own Project, so a retry does not meet the
// service accounts and roles of the one before.

// The console's paths: its bundle, the admin API, public GCP API paths and
// the OAuth2 token endpoint the policy simulator gets its token from.
const allowed = /^\/(console\/|_emu\/v1\/|oauth2\/(token|revoke)$|[a-z]+\/v\d[a-z0-9]*\/)/

const keyRows = (page: Page) => page.getByTestId('key')

test('IAM: a service account’s lifecycle, its keys, a grant and the policy simulator', async ({
  page,
  request,
  browserName,
  requests,
}, testInfo) => {
  const id = projectId(`e2e-iam${testInfo.retry}`, browserName)
  const email = `runner@${id}.iam.gserviceaccount.com`
  const bucket = `${id}-iam`
  expect((await request.post(`/storage/v1/b?project=${id}`, { data: { name: bucket } })).ok()).toBe(
    true,
  )
  expect((await request.put(`/pubsub/v1/projects/${id}/topics/events`, { data: {} })).ok()).toBe(
    true,
  )

  await page.goto(`/console/iam/serviceAccounts?project=${id}`)
  await expect(page.getByText('No service accounts in this Project yet.')).toBeVisible()
  await page.getByRole('link', { name: 'Create service account' }).click()
  await page.getByLabel('Service account ID').fill('Runner')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page.getByText('Invalid account ID: "Runner".')).toBeVisible()
  await page.getByLabel('Service account ID').fill('runner')
  await page.getByLabel('Display name').fill('CI runner')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page.getByRole('heading', { name: email })).toBeVisible()
  await expect(page.getByLabel('Service account details')).toContainText('CI runner')

  // Edit sends the changed field.
  await page.getByRole('link', { name: 'Edit service account' }).click()
  await page.getByLabel('Description').fill('Runs the e2e tests')
  await page.getByRole('button', { name: 'Save' }).click()
  await expect(page.getByLabel('Service account details')).toContainText('Runs the e2e tests')

  // A JSON key is created and its key file downloaded, once.
  await page.getByRole('link', { name: 'Keys' }).click()
  const [download] = await Promise.all([
    page.waitForEvent('download'),
    page.getByRole('button', { name: 'Create JSON key' }).click(),
  ])
  const file = JSON.parse(readFileSync(await download.path(), 'utf8')) as Record<string, string>
  expect(file.type).toBe('service_account')
  expect(file.client_email).toBe(email)
  const key = keyRows(page).filter({ hasText: file.private_key_id! })
  await expect(key).toContainText('User-managed')
  await key.getByRole('button', { name: `Disable key ${file.private_key_id}` }).click()
  await expect(key).toContainText('Disabled')
  await key.getByRole('button', { name: `Delete key ${file.private_key_id}` }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete key' }).click()
  await expect(key).toHaveCount(0)

  // Grant the account Storage Admin on the Project: the API rejects a role
  // it does not know, then accepts the grant.
  await page.getByRole('link', { name: 'Policies' }).click()
  await page.getByRole('button', { name: 'Grant access' }).click()
  const grant = page.getByRole('region', { name: 'Grant access' })
  await grant.getByLabel('Role').fill('roles/storage.nonexistent')
  await grant.getByLabel('Members').fill(`serviceAccount:${email}`)
  await grant.getByRole('button', { name: 'Save' }).click()
  await expect(
    grant.getByText('Role roles/storage.nonexistent is not supported for this resource.'),
  ).toBeVisible()
  await grant.getByLabel('Role').fill('roles/storage.admin')
  await grant.getByRole('button', { name: 'Save' }).click()
  const binding = page.getByTestId('binding').filter({ hasText: 'roles/storage.admin' })
  await expect(binding).toContainText(`serviceAccount:${email}`)

  // The simulator: the account holds bucket permissions through the
  // Project's binding; a user without roles holds none.
  await page.getByRole('link', { name: 'Policy simulator' }).click()
  await page.getByLabel('Principal').fill(`serviceAccount:${email}`)
  await page.getByLabel('Resource type').selectOption('bucket')
  await page.getByLabel('Bucket').fill(bucket)
  await page.getByLabel('Permissions').fill('storage.buckets.get\nstorage.objects.get')
  await page.getByRole('button', { name: 'Test permissions' }).click()
  await expect(page.getByText('holds 2 of 2 permissions')).toBeVisible()
  await page.getByLabel('Principal').fill('user:nobody@example.com')
  await page.getByRole('button', { name: 'Test permissions' }).click()
  await expect(page.getByText('holds 0 of 2 permissions')).toBeVisible()
  await expect(page.getByTestId('result').filter({ hasText: 'storage.objects.get' })).toContainText(
    'Denied',
  )
  // Pub/Sub answers from the policies too, not from what audit lets through.
  await page.getByLabel('Resource type').selectOption('topic')
  await page.getByLabel('Topic').fill('events')
  await page.getByLabel('Permissions').fill('pubsub.topics.publish')
  await page.getByRole('button', { name: 'Test permissions' }).click()
  await expect(page.getByText('holds 0 of 1 permissions')).toBeVisible()

  // Delete the account, then undelete it by its unique ID.
  await page.getByRole('link', { name: 'Service accounts' }).click()
  await page.getByRole('link', { name: email }).click()
  await page.getByRole('link', { name: 'Details' }).click()
  const uniqueId = (await page
    .getByLabel('Service account details')
    .locator('dt:text-is("Unique ID") + dd')
    .textContent())!
  await page.getByRole('button', { name: 'Delete service account' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete service account' }).click()
  await expect(page.getByText('No service accounts in this Project yet.')).toBeVisible()
  await page.getByRole('button', { name: 'Undelete' }).click()
  await page.getByRole('dialog').getByLabel('Unique ID').fill(uniqueId)
  await page.getByRole('dialog').getByRole('button', { name: 'Undelete' }).click()
  await expect(page.getByRole('heading', { name: email })).toBeVisible()

  // Only the simulator's testIamPermissions carries credentials: the token
  // it got for the Principal.
  await expectConsoleRequests(requests, {
    allowed,
    credentialed: /(\/iam\/testPermissions|:testIamPermissions)$/,
  })
})

test('IAM: a custom role’s lifecycle and the IAM enforcement level', async ({
  page,
  browserName,
}, testInfo) => {
  const id = projectId(`e2e-iamr${testInfo.retry}`, browserName)
  await page.goto(`/console/iam/roles?project=${id}`)
  await expect(page.getByText('No custom roles in this Project yet.')).toBeVisible()
  await expect(
    page.getByRole('table', { name: 'Predefined roles' }).getByText('roles/storage.admin'),
  ).toBeVisible()

  await page.getByRole('link', { name: 'Create role' }).click()
  await page.getByLabel('Role ID').fill('objectReader')
  await page.getByLabel('Title').fill('Object reader')
  await page.getByLabel('Permissions').fill('storage.objects.get\nstorage.objects.list')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/iam/customRoles/objectReader\\?project=${id}$`))
  await expect(page.getByRole('list', { name: 'Permissions' }).getByRole('listitem')).toHaveCount(2)

  await page.getByRole('link', { name: 'Edit role' }).click()
  await page.getByLabel('Launch stage').selectOption('BETA')
  await page.getByRole('button', { name: 'Save' }).click()
  await expect(page.getByLabel('Role details')).toContainText('BETA')

  // A custom role can be granted; deleted, it cannot until undeleted.
  await page.getByRole('button', { name: 'Delete role' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete role' }).click()
  await page.getByRole('button', { name: 'Undelete role' }).click()
  await expect(page.getByRole('button', { name: 'Delete role' })).toBeVisible()
  await page.getByRole('navigation', { name: 'IAM' }).getByRole('link', { name: 'Roles' }).click()
  await expect(
    page
      .getByRole('table', { name: 'Custom roles' })
      .getByRole('link', { name: 'Object reader', exact: true }),
  ).toBeVisible()

  // The enforcement level. Other tests share the Instance and act as the
  // Default principal, which off and audit both allow; enforce is left to
  // the component tests.
  const level = page.getByLabel('IAM enforcement level')
  await expect(level).toHaveValue(/audit|off/)
  try {
    await level.selectOption('off')
    await expect(page.getByText('IAM enforcement level is now off.')).toBeVisible()
  } finally {
    await level.selectOption('audit')
  }
  await expect(page.getByText('IAM enforcement level is now audit.')).toBeVisible()
})

test('IAM: the audit view lists denied calls and grants the smallest role', async ({
  page,
  request,
  browserName,
}, testInfo) => {
  const id = projectId(`e2e-iama${testInfo.retry}`, browserName)
  const who = `user:auditee-${browserName}-${testInfo.retry}@example.com`
  expect((await request.put(`/pubsub/v1/projects/${id}/topics/orders`, { data: {} })).ok()).toBe(
    true,
  )
  const token = await request.post('/oauth2/token', {
    form: { grant_type: 'refresh_token', refresh_token: who },
  })
  const { access_token } = (await token.json()) as { access_token: string }
  const as = { Authorization: `Bearer ${access_token}` }

  // denied makes a call as the Principal until the Instance logs it denied:
  // another test turns IAM off for a moment, and then nothing is checked.
  const denied = (permission: string, call: () => Promise<unknown>) =>
    expect
      .poll(async () => {
        await call()
        const res = await request.get(`/_emu/v1/iam/denials?project=${id}`)
        const { denials } = (await res.json()) as { denials: { permission: string }[] }
        return denials.some((d) => d.permission === permission)
      })
      .toBe(true)

  await denied('pubsub.topics.publish', () =>
    request.post(`/pubsub/v1/projects/${id}/topics/orders:publish`, {
      headers: as,
      data: { messages: [{ data: btoa('hi') }] },
    }),
  )

  await page.goto(`/console/iam/audit?project=${id}`)
  await expect(page.getByRole('heading', { name: 'IAM audit' })).toBeVisible()
  const publish = page.getByTestId('denial').filter({ hasText: 'pubsub.topics.publish' })
  await expect(publish).toContainText(who)
  await expect(publish).toContainText(`//pubsub.googleapis.com/projects/${id}/topics/orders`)
  await expect(publish).toContainText(':publish')
  await expect(publish.getByRole('link', { name: /roles\/pubsub\.publisher/ })).toBeVisible()

  // A later denied call appears without a reload, through the event stream.
  await denied('pubsub.topics.get', () =>
    request.get(`/pubsub/v1/projects/${id}/topics/orders`, { headers: as }),
  )
  const get = page.getByTestId('denial').filter({ hasText: 'pubsub.topics.get' })
  await expect(get).toBeVisible()
  await expect(get).toContainText('roles/pubsub.viewer')

  // Filtering keeps the matching calls, in the URL.
  await page.getByRole('searchbox', { name: 'Filter' }).fill('publish')
  await expect(page.getByTestId('denial')).toHaveCount(1)
  await expect(page).toHaveURL(/q=publish/)
  await page.getByRole('searchbox', { name: 'Filter' }).fill('')

  // One click grants the role on the Project, and the call is granted.
  await publish
    .getByRole('button', { name: `Grant roles/pubsub.publisher to ${who} on Project ${id}` })
    .click()
  await expect(
    page.getByText(`Granted roles/pubsub.publisher to ${who} on Project ${id}.`),
  ).toBeVisible()
  await expect(publish).toContainText('Granted')
  await expect(get.getByRole('button', { name: /^Grant/ })).toBeVisible()
  await page.getByLabel('Status').selectOption('missing')
  await expect(page.getByTestId('denial')).toHaveCount(1)
  await expect(page.getByTestId('denial')).toContainText('pubsub.topics.get')

  // The Project's policy holds the binding.
  await get.getByRole('link', { name: `Policy of ${id}` }).click()
  await expect(
    page.getByTestId('binding').filter({ hasText: 'roles/pubsub.publisher' }),
  ).toContainText(who)
})
