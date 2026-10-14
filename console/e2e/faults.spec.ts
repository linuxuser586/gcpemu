import type { APIRequestContext, Page } from '@playwright/test'

import { expect, expectConsoleRequests, test } from './fixtures'

// FR-UI-016: Fault rules are the Instance's and every browser shares one
// Instance, so each test's rule matches only requests for a bucket named
// after its browser and test, and is removed afterwards.

const allowed = /^\/(console\/|_emu\/v1\/|[a-z]+\/v\d[a-z0-9]*\/)/

/** bucketStatus is the HTTP status of storage buckets.get of a bucket that does not exist. */
async function bucketStatus(request: APIRequestContext, bucket: string) {
  return (await request.get(`/storage/v1/b/${bucket}`)).status()
}

const ruleRow = (page: Page, id: string) =>
  page
    .getByRole('table', { name: 'Fault rules' })
    .getByTestId('fault')
    .and(page.locator(`[data-id="${id}"]`))

test.describe('FR-UI-016 fault rules', () => {
  const ids: string[] = []
  test.afterEach(async ({ request }) => {
    for (const id of ids.splice(0)) await request.delete(`/_emu/v1/faults/${id}`)
  })

  test('adds a rule, sees its hits, disables, enables and removes it', async ({
    page,
    request,
    requests,
    browserName,
  }) => {
    const id = `e2e-fault-${browserName}`
    const bucket = `fault-form-${browserName}`
    ids.push(id)
    expect(await bucketStatus(request, bucket)).toBe(404)

    await page.goto('/console/')
    await page
      .getByRole('navigation', { name: 'Services' })
      .getByRole('link', { name: 'Fault rules' })
      .click()
    await expect(page.getByRole('heading', { name: 'Fault rules' })).toBeVisible()
    await page.getByRole('link', { name: 'Add rule' }).click()
    await expect(page.getByRole('heading', { name: 'Add fault rule' })).toBeVisible()
    await page.getByLabel('Service', { exact: true }).selectOption({ label: 'Cloud Storage' })
    await page.getByLabel('ID', { exact: true }).fill(id)
    await page.getByLabel('Method').fill(`^GET .*/b/${bucket}$`)
    await page.getByLabel('Error code').selectOption('UNAVAILABLE')
    await page.getByLabel('HTTP status').fill('418')
    await page.getByRole('button', { name: 'Add rule' }).click()
    await expect(page.getByText(`Added rule ${id}.`)).toBeVisible()

    const row = ruleRow(page, id)
    const toggle = row.getByRole('switch', { name: `Rule ${id} enabled` })
    await expect(toggle).toBeChecked()
    await expect(row).toContainText('UNAVAILABLE (HTTP 418)')
    await expect(row.getByLabel('0 hits')).toBeVisible()

    // The list polls while the rule can fire.
    expect(await bucketStatus(request, bucket)).toBe(418)
    expect(await bucketStatus(request, bucket)).toBe(418)
    await expect(row.getByLabel('2 hits')).toBeVisible({ timeout: 10_000 })

    await toggle.click()
    await expect(page.getByText(`Disabled rule ${id}.`)).toBeVisible()
    await expect(toggle).not.toBeChecked()
    expect(await bucketStatus(request, bucket)).toBe(404)
    await page.reload()
    await expect(toggle).not.toBeChecked()
    await expect(row.getByLabel('2 hits')).toBeVisible()

    await toggle.click()
    await expect(page.getByText(`Enabled rule ${id}.`)).toBeVisible()
    await expect(toggle).toBeChecked()
    expect(await bucketStatus(request, bucket)).toBe(418)
    await expect(row.getByLabel('3 hits')).toBeVisible({ timeout: 10_000 })

    await row.getByRole('button', { name: `Remove rule ${id}` }).click()
    await page.getByRole('dialog').getByRole('button', { name: 'Remove' }).click()
    await expect(page.getByText(`Removed rule ${id}.`)).toBeVisible()
    await expect(row).toHaveCount(0)
    expect(await bucketStatus(request, bucket)).toBe(404)

    await expectConsoleRequests(requests, { allowed })
  })

  test('adds a rule from the JSON editor, and shows the API’s errors', async ({
    page,
    request,
    browserName,
  }) => {
    const id = `e2e-fault-json-${browserName}`
    const bucket = `fault-json-${browserName}`
    ids.push(id)

    await page.goto('/console/faults/create')
    await page.getByRole('tab', { name: 'JSON' }).click()
    const body = page.getByLabel('Request body')
    await body.fill(JSON.stringify({ id, method: '(', code: 'ABORTED' }))
    await page.getByRole('button', { name: 'Add rule' }).click()
    await expect(page.getByRole('alert')).toContainText('method: error parsing regexp')

    await body.fill(
      JSON.stringify({
        id,
        service: 'gcs',
        method: `/b/${bucket}$`,
        latency: '10ms',
        code: 'NOT_FOUND',
        count: 1,
        disabled: true,
      }),
    )
    await page.getByRole('button', { name: 'Add rule' }).click()
    await expect(page.getByText(`Added rule ${id}, disabled.`)).toBeVisible()
    const row = ruleRow(page, id)
    await expect(row.getByRole('switch')).not.toBeChecked()
    await expect(row).toContainText('Delay 10ms, then NOT_FOUND')
    await expect(row).toContainText('Every match, at most 1 time')
    await expect(row.getByLabel('0 hits')).toHaveText('0 / 1')

    // An ID is unique.
    const dup = await request.post('/_emu/v1/faults', { data: { id, drop: true } })
    expect(dup.status()).toBe(400)
    expect(await dup.text()).toContain('already exists')
  })
})
