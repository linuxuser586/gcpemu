import type { APIRequestContext, Page } from '@playwright/test'

import { expect, projectId, test } from './fixtures'

// The Cloud SQL view (SRS 4.8.3, FR-UI-011) against real PostgreSQL
// servers. Instances need a container runtime, so the tests that create
// one skip without it.

const allowed = /^\/(console\/|_emu\/v1\/|[a-z]+\/v\d[a-z0-9]*\/)/

async function reachable(request: APIRequestContext) {
  const info = (await (await request.get('/_emu/v1/info')).json()) as {
    runtime?: { reachable?: boolean }
  }
  return !!info.runtime?.reachable
}

const tab = (page: Page, name: string) =>
  page.getByRole('navigation', { name: 'Instance' }).getByRole('link', { name, exact: true })

test('Cloud SQL: the create form shows the API’s validation messages', async ({
  page,
  browserName,
}) => {
  const id = projectId('e2e-sqlform', browserName)
  await page.goto(`/console/sql?project=${id}`)
  await expect(page.getByText('No instances in this Project yet.')).toBeVisible()
  await page.getByRole('link', { name: 'Create instance' }).click()

  await page.getByLabel('Instance ID').fill('Bad_Name')
  await page.getByLabel('Database flags').fill('no_such_flag=1')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page.getByText(/Invalid instance name \(Bad_Name\)/)).toBeVisible()
  await expect(page.getByText('Invalid request: Invalid flag name: no_such_flag.')).toBeVisible()

  // The API rejects what the form cannot know is wrong.
  await page.getByLabel('Instance ID').fill('nowhere')
  await page.getByLabel('Database flags').fill('')
  await page.getByLabel('Region').fill('mars-north1')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(
    page.getByRole('alert').filter({ hasText: 'Invalid region (mars-north1)' }),
  ).toBeVisible()

  // The JSON editor holds the whole request body.
  await page.getByRole('tab', { name: 'JSON' }).click()
  const body = JSON.parse(await page.getByLabel('Request body').inputValue()) as {
    name: string
    settings: { dataApiAccess: string }
  }
  expect(body.name).toBe('nowhere')
  expect(body.settings.dataApiAccess).toBe('DISALLOW_DATA_API')
})

test('Cloud SQL: an instance’s lifecycle, query runner and backups', async ({
  page,
  request,
  browserName,
  requests,
}, testInfo) => {
  test.skip(!(await reachable(request)), 'needs a container runtime')
  test.setTimeout(6 * 60_000)
  const id = projectId('e2e-sql', browserName)
  const name = `pg${testInfo.retry}`
  const heading = page.getByRole('heading', { level: 1 })
  const state = (s: string) => expect(heading.getByText(s, { exact: true }))

  // Create (FR-SQL-001) with the form, a flag and the Data API on.
  await page.goto(`/console/sql/create?project=${id}`)
  await page.getByLabel('Instance ID').fill(name)
  await page.getByLabel('Database version').selectOption('POSTGRES_17')
  await page.getByLabel('Password of the postgres user').fill('rootpw')
  await page.getByLabel('Edition').selectOption('ENTERPRISE')
  await page.getByLabel('Database flags').fill('work_mem=8192')
  await page.getByLabel(/Data API access/).check()
  await page.getByLabel('Labels').fill('team=e2e')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/sql/instances/${name}\\?project=${id}$`))
  await expect(heading).toContainText(name)
  await state('RUNNABLE').toBeVisible({ timeout: 2 * 60_000 })

  // Connection name and strings.
  const connection = `${id}:us-central1:${name}`
  await expect(page.getByText(connection, { exact: true })).toBeVisible()
  await expect(page.getByText(/^psql "host=127\.0\.0\.1 port=\d+ user=postgres/)).toBeVisible()
  await expect(
    page.getByText(
      `cloud-sql-proxy --sqladmin-api-endpoint ${new URL(page.url()).origin}/ ${connection}`,
    ),
  ).toBeVisible()
  await expect(page.getByLabel('Instance details')).toContainText('team=e2e')

  // Flags.
  await tab(page, 'Flags').click()
  const flag = page.getByTestId('flag')
  await expect(flag).toContainText('work_mem')
  await expect(flag).toContainText('8192')

  // Databases and users (FR-SQL-002).
  await tab(page, 'Databases').click()
  await page.getByRole('link', { name: 'Create database' }).click()
  await page.getByLabel('Name').fill('app')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page.getByTestId('database').filter({ hasText: 'app' })).toBeVisible()

  await tab(page, 'Users').click()
  await page.getByRole('link', { name: 'Add user' }).click()
  await page.getByLabel('Name').fill('cloudsqlnope')
  await page.getByRole('button', { name: 'Add' }).click()
  await expect(
    page.getByText('Invalid request: User name (cloudsqlnope) is reserved.'),
  ).toBeVisible()
  await page.getByLabel('Name').fill('reader')
  await page.getByLabel('Password').fill('pw1')
  await page.getByRole('button', { name: 'Add' }).click()
  await expect(page.getByTestId('user').filter({ hasText: 'reader' })).toBeVisible()
  await page.getByRole('link', { name: 'Edit user reader' }).click()
  await page.getByLabel('New password').fill('pw2')
  await page.getByRole('button', { name: 'Save' }).click()
  await expect(page.getByTestId('user').filter({ hasText: 'reader' })).toBeVisible()

  // The query runner (executeSql), read-only by default.
  await tab(page, 'Query').click()
  await page.getByLabel('Database').selectOption('app')
  const sql = page.getByLabel('SQL')
  const run = page.getByRole('button', { name: 'Run' })
  const results = page.getByRole('region', { name: 'Results' })
  await sql.fill('CREATE TABLE items (id int, name text)')
  await run.click()
  await expect(results.getByRole('alert')).toContainText('read-only transaction')
  await page.getByLabel('Read-only').uncheck()
  await sql.fill(
    "CREATE TABLE items (id int, name text);\nINSERT INTO items VALUES (1, 'one'), (2, NULL);",
  )
  await run.click()
  await expect(results.getByTestId('result')).toHaveCount(2)
  await expect(results).toContainText('INSERT 0 2')
  await page.getByLabel('Read-only').check()
  await sql.fill('SELECT id, name FROM items ORDER BY id')
  await sql.press('Control+Enter')
  const table = results.getByRole('table', { name: 'Result 1' })
  await expect(table).toContainText('INT4')
  await expect(table.getByRole('row')).toHaveCount(3)
  await expect(table.getByRole('row').nth(2)).toContainText('NULL')

  // Backups and restore (FR-SQL-008).
  await tab(page, 'Backups').click()
  await page.getByRole('button', { name: 'Create backup' }).click()
  await page.getByRole('dialog').getByLabel('Description').fill('before')
  await page.getByRole('dialog').getByRole('button', { name: 'Create' }).click()
  const backup = page.getByTestId('backup').filter({ hasText: 'before' })
  await expect(backup).toContainText('SUCCESSFUL', { timeout: 60_000 })

  await tab(page, 'Query').click()
  await page.getByLabel('Database').selectOption('app')
  await page.getByLabel('Read-only').uncheck()
  await sql.fill('DROP TABLE items')
  await run.click()
  await expect(results).toContainText('DROP TABLE')

  await tab(page, 'Backups').click()
  await backup.getByRole('button', { name: /^Restore backup/ }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Restore' }).click()
  await expect(page.getByRole('dialog')).toBeHidden()
  await tab(page, 'Query').click()
  await page.getByLabel('Database').selectOption('app')
  await sql.fill('SELECT id FROM items')
  // The restore runs as an Operation; the table is back once it is done.
  await expect(async () => {
    await run.click()
    await expect(results).toContainText('SELECT 2', { timeout: 2000 })
  }).toPass({ timeout: 2 * 60_000 })

  // Stop, start and restart.
  await page.getByRole('button', { name: 'Stop', exact: true }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Stop instance' }).click()
  await state('STOPPED').toBeVisible({ timeout: 60_000 })
  await expect(page.getByText(/The instance is stopped/)).toBeVisible()
  await page.getByRole('button', { name: 'Start', exact: true }).click()
  await state('RUNNABLE').toBeVisible({ timeout: 2 * 60_000 })
  await page.getByRole('button', { name: 'Restart', exact: true }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Restart instance' }).click()
  await state('RUNNABLE').toBeVisible({ timeout: 2 * 60_000 })

  // Edit (instances.patch): a label and a flag.
  await page.getByRole('link', { name: 'Edit', exact: true }).click()
  await page.getByLabel('Labels').fill('env=test')
  await page.getByLabel('Database flags').fill('work_mem=16384')
  await page.getByRole('button', { name: 'Save' }).click()
  const details = page.getByLabel('Instance details')
  await expect(details).toContainText('env=test', { timeout: 60_000 })
  await expect(details).not.toContainText('team=e2e')
  await expect(details).toContainText('work_mem=16384')

  // Delete a user, a database, then the instance.
  await tab(page, 'Users').click()
  await page.getByRole('button', { name: 'Delete user reader' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete user' }).click()
  await expect(page.getByTestId('user').filter({ hasText: 'reader' })).toHaveCount(0)
  await tab(page, 'Databases').click()
  await page.getByRole('button', { name: 'Delete database app' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete database' }).click()
  await expect(page.getByTestId('database').filter({ hasText: 'app' })).toHaveCount(0)

  await page.getByRole('button', { name: 'Delete', exact: true }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete instance' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/sql\\?project=${id}`))
  await expect(page.getByTestId('instance').filter({ hasText: name })).toHaveCount(0, {
    timeout: 60_000,
  })

  // FR-UI-003, 004: only public APIs and /_emu/v1, without credentials.
  for (const r of requests) {
    const url = new URL(r.url())
    expect(url.pathname, r.url()).toMatch(allowed)
    expect(await r.headerValue('authorization'), r.url()).toBeNull()
  }
  expect(requests.some((r) => new URL(r.url()).pathname.endsWith('/executeSql'))).toBe(true)
})
