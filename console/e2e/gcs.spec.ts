import { readFileSync } from 'node:fs'

import type { Page } from '@playwright/test'

import { expect, projectId, test } from './fixtures'

// The Cloud Storage view (SRS 4.8.3, FR-UI-011) against the Instance's
// gcs, pubsub and iam Services. Each attempt has its own Project, so a
// retry does not meet the buckets and topics of the one before.

// The console's paths: its bundle, the admin API, public GCP API paths
// ("/<api>/v<n>/...") and the JSON API's upload and download paths.
const allowed = /^\/(console\/|_emu\/v1\/|(upload\/|download\/)?[a-z]+\/v\d[a-z0-9]*\/)/

const objectsTable = (page: Page) => page.getByRole('table', { name: 'Objects' })

test('Cloud Storage: a bucket’s lifecycle with the API’s validation messages', async ({
  page,
  browserName,
  requests,
}, testInfo) => {
  const id = projectId(`e2e-gcsb${testInfo.retry}`, browserName)
  const name = `${id}-life`
  await page.goto(`/console/gcs?project=${id}`)
  await expect(page.getByText('No buckets in this Project yet.')).toBeVisible()
  await page.getByRole('link', { name: 'Create bucket' }).click()

  await page.getByLabel('Name').fill('Bad_Name')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page.getByText("Invalid bucket name: 'Bad_Name'")).toBeVisible()

  // The API rejects what the form cannot know is wrong.
  await page.getByLabel('Name').fill(name)
  await page.getByLabel('Location').fill('mars-north1')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(
    page.getByRole('alert').filter({ hasText: 'The specified location constraint is not valid.' }),
  ).toBeVisible()

  await page.getByLabel('Location').fill('us-east1')
  await page.getByLabel('Labels').fill('team=e2e')
  await page.getByRole('tab', { name: 'JSON' }).click()
  const body = JSON.parse(await page.getByLabel('Request body').inputValue()) as {
    name: string
    location: string
  }
  expect(body).toMatchObject({ name, location: 'US-EAST1' })
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/gcs/b/${name}\\?project=${id}$`))
  await expect(page.getByText('No objects here yet.')).toBeVisible()

  // Edit: turn versioning on and change the labels.
  await page.getByRole('link', { name: 'Edit bucket' }).click()
  await page.getByLabel('Object versioning').check()
  await page.getByLabel('Labels').fill('team=platform')
  await page.getByRole('button', { name: 'Save' }).click()
  const details = page.getByLabel('Bucket details')
  await expect(details).toContainText('US-EAST1 (region)')
  await expect(details).toContainText('team=platform')
  await expect(
    details.getByText('Object versioning').locator('xpath=following-sibling::dd[1]'),
  ).toHaveText('On')

  // The bucket list shows it; then delete it.
  await page.getByRole('link', { name: 'Buckets' }).click()
  await expect(page.getByTestId('bucket').filter({ hasText: name })).toBeVisible()
  await page.getByRole('link', { name }).click()
  await page.getByRole('button', { name: 'Delete bucket' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete bucket' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/gcs\\?project=${id}$`))
  await expect(page.getByText('No buckets in this Project yet.')).toBeVisible()

  for (const r of requests) {
    expect(new URL(r.url()).pathname, r.url()).toMatch(allowed)
    expect(await r.headerValue('authorization'), r.url()).toBeNull()
  }
})

test('Cloud Storage: upload, browse, download, generations and a signed URL', async ({
  page,
  request,
  browserName,
  requests,
}, testInfo) => {
  const id = projectId(`e2e-gcso${testInfo.retry}`, browserName)
  const bucket = `${id}-objects`
  expect(
    (
      await request.post(`/storage/v1/b?project=${id}`, {
        data: { name: bucket, versioning: { enabled: true } },
      })
    ).ok(),
  ).toBe(true)
  await page.goto(`/console/gcs/b/${bucket}?project=${id}`)
  await expect(page.getByText('No objects here yet.')).toBeVisible()

  // Upload with the file chooser.
  await page.getByLabel('Files to upload').setInputFiles({
    name: 'notes.txt',
    mimeType: 'text/plain',
    buffer: Buffer.from('version one'),
  })
  await expect(page.getByTestId('upload')).toHaveAttribute('data-status', 'done')
  await expect(
    objectsTable(page).getByRole('link', { name: 'notes.txt', exact: true }),
  ).toBeVisible()

  // Drag and drop a file into a folder made by its name.
  const dropped = await page.evaluateHandle(() => {
    const dt = new DataTransfer()
    dt.items.add(new File(['dropped'], 'dropped.txt', { type: 'text/plain' }))
    return dt
  })
  const browser = page.getByTestId('object-browser')
  await browser.dispatchEvent('dragover', { dataTransfer: dropped })
  await browser.dispatchEvent('drop', { dataTransfer: dropped })
  await expect(
    objectsTable(page).getByRole('link', { name: 'dropped.txt', exact: true }),
  ).toBeVisible()
  expect(
    (
      await request.post(`/upload/storage/v1/b/${bucket}/o?uploadType=media&name=docs/a/deep.txt`, {
        data: 'deep',
        headers: { 'Content-Type': 'text/plain' },
      })
    ).ok(),
  ).toBe(true)

  // Browse by prefix.
  await page.reload()
  await objectsTable(page).getByRole('link', { name: 'docs/', exact: true }).click()
  await expect(page).toHaveURL(/prefix=docs%2F/)
  await objectsTable(page).getByRole('link', { name: 'a/', exact: true }).click()
  await expect(
    objectsTable(page).getByRole('link', { name: 'deep.txt', exact: true }),
  ).toBeVisible()
  await page
    .getByRole('navigation', { name: 'Folder' })
    .getByRole('button', { name: bucket })
    .click()
  await expect(
    objectsTable(page).getByRole('link', { name: 'notes.txt', exact: true }),
  ).toBeVisible()

  // A second upload makes a new generation.
  await page.getByLabel('Files to upload').setInputFiles({
    name: 'notes.txt',
    mimeType: 'text/plain',
    buffer: Buffer.from('version two'),
  })
  await expect(page.getByTestId('upload')).toHaveAttribute('data-status', 'done')
  await objectsTable(page).getByRole('link', { name: 'notes.txt', exact: true }).click()
  await expect(page).toHaveURL(new RegExp(`/console/gcs/b/${bucket}/o/notes.txt\\?project=${id}$`))
  const gens = page.getByTestId('generation')
  await expect(gens).toHaveCount(2)
  await expect(gens.first()).toContainText('Live')
  await expect(gens.nth(1)).toContainText('Noncurrent')

  // Download the live version.
  const [download] = await Promise.all([
    page.waitForEvent('download'),
    page.getByRole('link', { name: 'Download', exact: true }).click(),
  ])
  expect(download.suggestedFilename()).toBe('notes.txt')
  expect(readFileSync(await download.path(), 'utf8')).toBe('version two')

  // Edit metadata.
  await page.getByRole('link', { name: 'Edit metadata' }).click()
  await page.getByLabel('Cache control').fill('no-store')
  await page.getByLabel('Custom metadata').fill('owner: e2e')
  await page.getByRole('button', { name: 'Save' }).click()
  const details = page.getByLabel('Object details')
  await expect(details).toContainText('no-store')
  await expect(details).toContainText('owner: e2e')

  // Delete the live version, then restore the old generation.
  await page.getByRole('button', { name: 'Delete', exact: true }).click()
  await page.getByRole('button', { name: 'Delete object' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/gcs/b/${bucket}\\?project=${id}$`))
  await page.goto(`/console/gcs/b/${bucket}/o/notes.txt?project=${id}`)
  await expect(page.getByText(/no live version/)).toBeVisible()
  await expect(gens).toHaveCount(2)
  const oldest = (await gens.nth(1).locator('td').first().textContent())!
  await page.getByRole('button', { name: `Restore generation ${oldest}` }).click()
  await page.getByRole('button', { name: 'Restore', exact: true }).click()
  await expect(gens).toHaveCount(3)
  await expect(gens.first()).toContainText('Live')
  const restored = await request.get(`/storage/v1/b/${bucket}/o/notes.txt?alt=media`)
  expect(await restored.text()).toBe('version one')

  // A V4 signed URL, signed by a service account through signBlob, reads it.
  const sa = (await (
    await request.post(`/iam/v1/projects/${id}/serviceAccounts`, {
      data: { accountId: 'signer' },
    })
  ).json()) as { email: string }
  await page.getByRole('button', { name: 'Signed URL' }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByLabel('Service account').fill(sa.email)
  await dialog.getByLabel('Expires after (seconds)').fill('600')
  await dialog.getByRole('button', { name: 'Generate' }).click()
  const url = await dialog.getByLabel('Signed URL').inputValue()
  expect(url).toMatch(new RegExp(`/${bucket}/notes\\.txt\\?X-Goog-Algorithm=GOOG4-RSA-SHA256&`))
  const signed = await request.get(url)
  expect(signed.status()).toBe(200)
  expect(await signed.text()).toBe('version one')
  const tampered = await request.get(url.replace(/X-Goog-Signature=../, 'X-Goog-Signature=00'))
  expect(tampered.status()).toBe(403)

  for (const r of requests) {
    expect(new URL(r.url()).pathname, r.url()).toMatch(allowed)
    expect(await r.headerValue('authorization'), r.url()).toBeNull()
  }
})

test('Cloud Storage: notifications to a Pub/Sub topic', async ({
  page,
  request,
  browserName,
}, testInfo) => {
  const id = projectId(`e2e-gcsn${testInfo.retry}`, browserName)
  const bucket = `${id}-notify`
  expect((await request.put(`/pubsub/v1/projects/${id}/topics/uploads`, { data: {} })).ok()).toBe(
    true,
  )
  expect((await request.post(`/storage/v1/b?project=${id}`, { data: { name: bucket } })).ok()).toBe(
    true,
  )
  await page.goto(`/console/gcs/b/${bucket}/notifications?project=${id}`)
  await expect(page.getByText('This bucket sends no notifications.')).toBeVisible()
  await page.getByRole('link', { name: 'Create notification' }).click()

  const topic = page.getByLabel('Topic')
  await topic.fill('uploads')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page.getByText('Invalid Cloud Pub/Sub topic name: uploads')).toBeVisible()

  await topic.fill(`projects/${id}/topics/missing`)
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(
    page.getByRole('alert').filter({ hasText: /topics\/missing does not exist/ }),
  ).toBeVisible()

  await topic.fill(`projects/${id}/topics/uploads`)
  await page.getByLabel('OBJECT_FINALIZE').check()
  await page.getByLabel('Object name prefix').fill('in/')
  await page.getByRole('button', { name: 'Create' }).click()
  const details = page.getByLabel('Notification details')
  await expect(details).toContainText(`projects/${id}/topics/uploads`)
  await expect(details).toContainText('OBJECT_FINALIZE')

  await page.getByRole('link', { name: 'Notifications' }).click()
  await expect(page.getByTestId('notification')).toHaveCount(1)
  await page.getByTestId('notification').getByRole('link').click()
  await page.getByRole('button', { name: 'Delete', exact: true }).click()
  await page.getByRole('button', { name: 'Delete notification' }).click()
  await expect(page.getByText('This bucket sends no notifications.')).toBeVisible()
})
