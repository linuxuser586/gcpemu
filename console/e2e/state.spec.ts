import { readFile } from 'node:fs/promises'

import { expect, expectConsoleRequests, test } from './fixtures'
import { startInstance, type Instance } from './instance'

// FR-UI-017: restoring a State snapshot and a Reset change all of an
// Instance's state, so these tests run against an Instance of their own
// per browser rather than the shared one, in order.

const allowed = /^\/(console\/|_emu\/v1\/|[a-z]+\/v\d[a-z0-9]*\/)/

test.describe.configure({ mode: 'serial' })

test.describe('FR-UI-017 State snapshots, Reset and seed files', () => {
  let inst: Instance
  test.beforeAll(async () => {
    inst = await startInstance(['gcs', 'pubsub'])
  })
  test.afterAll(async () => {
    await inst?.stop()
  })

  const object = (bucket: string, name: string) =>
    `${inst.url}/storage/v1/b/${bucket}/o/${encodeURIComponent(name)}?alt=media`

  test('saves a State snapshot and restores it', async ({ page, request, requests }) => {
    expect(
      (
        await request.post(`${inst.url}/storage/v1/b?project=state-e2e`, { data: { name: 'kept' } })
      ).status(),
    ).toBe(200)
    expect(
      (
        await request.post(`${inst.url}/upload/storage/v1/b/kept/o?uploadType=media&name=a.txt`, {
          data: 'snapshot data',
          headers: { 'Content-Type': 'text/plain' },
        })
      ).status(),
    ).toBe(200)

    await page.goto(`${inst.url}/console/`)
    await page
      .getByRole('navigation', { name: 'Services' })
      .getByRole('link', { name: 'State' })
      .click()
    await expect(page.getByRole('heading', { name: 'State', exact: true })).toBeVisible()
    await expect(page.getByText('No State snapshots saved.')).toBeVisible()

    await page.getByRole('link', { name: 'Save snapshot' }).click()
    await expect(page.getByRole('heading', { name: 'Save State snapshot' })).toBeVisible()
    await page.getByLabel('Name').fill('fixture')
    await page.getByRole('button', { name: 'Save snapshot' }).click()
    await expect(page.getByText('Saved State snapshot fixture.')).toBeVisible()
    const row = page.getByTestId('snapshot').and(page.locator('[data-name="fixture"]'))
    await expect(row).toContainText('Cloud Storage')

    // Saving under a taken name replaces it; the JSON editor sends the same body.
    await page.getByRole('link', { name: 'Save snapshot' }).click()
    await page.getByLabel('Name').fill('fixture')
    await expect(page.getByText('Replaces the State snapshot fixture.')).toBeVisible()
    await page.getByRole('tab', { name: 'JSON' }).click()
    await expect(page.getByLabel('Request body')).toHaveValue(/"name": "fixture"/)
    await page.getByRole('button', { name: 'Replace snapshot' }).click()
    // The first save's toast may still show.
    await expect(page.getByText('Saved State snapshot fixture.').last()).toBeVisible()
    await expect(page.getByTestId('snapshot')).toHaveCount(1)

    expect((await request.delete(`${inst.url}/storage/v1/b/kept/o/a.txt`)).status()).toBe(204)
    expect((await request.get(object('kept', 'a.txt'))).status()).toBe(404)

    await row.getByRole('button', { name: 'Restore fixture' }).click()
    await page.getByRole('dialog').getByRole('button', { name: 'Restore' }).click()
    await expect(page.getByText('Restored State snapshot fixture.')).toBeVisible()
    const got = await request.get(object('kept', 'a.txt'))
    expect(got.status()).toBe(200)
    expect(await got.text()).toBe('snapshot data')

    await expectConsoleRequests(requests, { allowed })
  })

  test('imports and exports a seed file', async ({ page, request }) => {
    await page.goto(`${inst.url}/console/state`)
    const form = page.getByRole('form', { name: 'Import seed file' })
    const yaml = form.getByRole('textbox', { name: 'YAML' })

    await yaml.fill('nope:\n  things: []\n')
    await form.getByRole('button', { name: 'Apply seed file' }).click()
    await expect(form.getByRole('alert')).toContainText('seed section "nope": service not running')

    await form.getByLabel('Seed file').setInputFiles({
      name: 'seed.yaml',
      mimeType: 'application/yaml',
      buffer: Buffer.from(
        [
          'gcs:',
          '  buckets:',
          '    - name: seeded',
          '      project: state-e2e',
          '      objects:',
          '        - name: hello.txt',
          '          content: hi from the seed',
          'pubsub:',
          '  topics:',
          '    - name: seeded-topic',
          '      project: state-e2e',
          '',
        ].join('\n'),
      ),
    })
    await expect(yaml).toHaveValue(/seeded-topic/)
    await form.getByRole('button', { name: 'Apply seed file' }).click()
    await expect(page.getByText('Applied the seed file.')).toBeVisible()
    expect(await (await request.get(object('seeded', 'hello.txt'))).text()).toBe('hi from the seed')

    const [download] = await Promise.all([
      page.waitForEvent('download'),
      page.getByRole('button', { name: 'Export seed file' }).click(),
    ])
    expect(download.suggestedFilename()).toBe('seed.yaml')
    const exported = await readFile(await download.path(), 'utf8')
    expect(exported).toContain('name: seeded')
    expect(exported).toContain('content: hi from the seed')
    expect(exported).toContain('name: seeded-topic')
  })

  test('resets the Instance, keeping State snapshots', async ({ page, request }) => {
    await page.goto(`${inst.url}/console/state`)
    await page.getByRole('button', { name: 'Reset Instance' }).click()
    await page.getByRole('dialog').getByRole('button', { name: 'Reset' }).click()
    await expect(page.getByText('Reset the Instance.')).toBeVisible()
    expect((await request.get(`${inst.url}/storage/v1/b/kept`)).status()).toBe(404)
    expect((await request.get(`${inst.url}/storage/v1/b/seeded`)).status()).toBe(404)

    const row = page.getByTestId('snapshot').and(page.locator('[data-name="fixture"]'))
    await expect(row).toBeVisible()
    await row.getByRole('button', { name: 'Delete fixture' }).click()
    await page.getByRole('dialog').getByRole('button', { name: 'Delete' }).click()
    await expect(page.getByText('Deleted State snapshot fixture.')).toBeVisible()
    await expect(page.getByText('No State snapshots saved.')).toBeVisible()
  })
})
