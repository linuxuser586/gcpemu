import type { APIRequestContext } from '@playwright/test'

import { expect, expectConsoleRequests, projectId, test } from './fixtures'

// The Cloud Build view (SRS 4.8.3, FR-UI-011) against the Instance's
// build Service. Cloud Build is ready only with a container runtime, so
// the Instance runs it on Linux (emulator.ts) and these tests skip
// without it. Each attempt has its own Project.

const allowed = /^\/(console\/|_emu\/v1\/|[a-z]+\/v\d[a-z0-9]*\/)/

async function hasBuild(request: APIRequestContext) {
  const info = (await (await request.get('/_emu/v1/info')).json()) as {
    services: string[]
    runtime?: { reachable?: boolean }
  }
  return info.services.includes('build') && !!info.runtime?.reachable
}

/** startBuild creates a global Build whose step prints a line a second. */
async function startBuild(request: APIRequestContext, project: string, lines: number) {
  const res = await request.post(`/cloudbuild/v1/projects/${project}/builds`, {
    data: {
      steps: [
        {
          id: 'count',
          name: 'busybox:1.37',
          entrypoint: 'sh',
          args: ['-c', `for i in $(seq 1 ${lines}); do echo line $i; sleep 1; done`],
        },
      ],
    },
  })
  expect(res.ok(), await res.text()).toBe(true)
  const op = (await res.json()) as { metadata: { build: { id: string; logUrl: string } } }
  return op.metadata.build
}

test('Cloud Build: a Build’s live step log, final status, cancel and retry', async ({
  page,
  request,
  requests,
  browserName,
}, testInfo) => {
  test.skip(!(await hasBuild(request)), 'needs Cloud Build and a container runtime')
  test.setTimeout(180_000)
  const id = projectId(`e2e-cb${testInfo.retry}`, browserName)

  // The logUrl lands on the Build, in its Project.
  const b = await startBuild(request, id, 6)
  const url = new URL(b.logUrl)
  await page.goto(url.pathname)
  await expect(page).toHaveURL(new RegExp(`/builds/${b.id}\\?project=${id}`))
  await expect(page.getByRole('heading', { name: b.id })).toBeVisible()

  // Its step's log grows while it runs, and it ends without a reload.
  await page.getByRole('button', { name: 'Show the log of step 0' }).click()
  const log = page.getByLabel('Log of step 0', { exact: true })
  await expect(log).toContainText('line 2', { timeout: 120_000 })
  await expect(log).not.toContainText('line 6')
  await expect(log).toContainText('line 6', { timeout: 30_000 })
  await expect(page.getByTestId('build-status')).toHaveText('SUCCESS', { timeout: 30_000 })
  await expect(page.getByTestId('step')).toContainText('SUCCESS')

  // Retry starts a new Build; cancel stops it.
  await page.getByRole('button', { name: 'Retry Build' }).click()
  await expect(page.getByRole('heading', { name: b.id })).toHaveCount(0)
  await expect(page.getByTestId('build-status')).toHaveText(/QUEUED|WORKING/)
  const retried = new URL(page.url()).pathname.split('/').at(-1) ?? ''
  await page.getByRole('button', { name: 'Cancel Build' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Cancel Build' }).click()
  await expect(page.getByTestId('build-status')).toHaveText('CANCELLED', { timeout: 30_000 })

  // Both Builds are listed, and the actions are in the Request log.
  await page.getByRole('link', { name: 'Builds', exact: true }).click()
  const rows = page.getByRole('table', { name: 'Builds' }).getByTestId('build')
  await expect(rows.filter({ hasText: b.id })).toContainText('SUCCESS')
  await expect(rows.filter({ hasText: retried })).toContainText('CANCELLED')
  await page.goto(`/console/requests?project=${id}`)
  await page.getByRole('combobox', { name: 'Service' }).selectOption({ label: 'Cloud Build' })
  const logged = page.getByRole('table', { name: 'Requests' }).getByTestId('request')
  await expect(logged.filter({ hasText: ':retry' })).toHaveCount(1)
  await expect(logged.filter({ hasText: ':cancel' })).toHaveCount(1)

  await expectConsoleRequests(requests, { allowed })
})

test('Cloud Build: tabs by keyboard, Build triggers and pruning the Build daemon', async ({
  page,
  request,
  requests,
  browserName,
}) => {
  test.skip(!(await hasBuild(request)), 'needs Cloud Build and a container runtime')
  test.setTimeout(120_000)
  const id = projectId('e2e-cb-tabs', browserName)
  await page.goto(`/console/build?project=${id}`)
  await expect(page.getByText('No Builds in global yet.')).toBeVisible()

  const tabs = page.getByRole('navigation', { name: 'Cloud Build' })
  await tabs.getByRole('link', { name: 'Builds' }).focus()
  await page.keyboard.press('Tab')
  await expect(tabs.getByRole('link', { name: 'Build triggers' })).toBeFocused()
  await page.keyboard.press('Enter')
  await expect(page).toHaveURL(new RegExp(`/console/build/triggers\\?project=${id}`))
  // Build triggers answer UNIMPLEMENTED until the Instance emulates them.
  await expect(
    page
      .getByText('Build triggers are not available on this Instance yet.')
      .or(page.getByText('No Build triggers in global yet.')),
  ).toBeVisible()

  await tabs.getByRole('link', { name: 'Build daemon' }).focus()
  await page.keyboard.press('Enter')
  const details = page.getByLabel('Build daemon details')
  await expect(details).toContainText('Cache size')
  await page.getByRole('button', { name: 'Prune cache' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Prune cache' }).click()
  await expect(page.getByText(/^Pruned the Build daemon: freed /)).toBeVisible({ timeout: 90_000 })
  await expect(details).toContainText('Running')

  await page.goto(`/console/requests`)
  await page.getByRole('combobox', { name: 'Service' }).selectOption({ label: 'Cloud Build' })
  await expect(
    page
      .getByRole('table', { name: 'Requests' })
      .getByTestId('request')
      .filter({ hasText: 'prune' }),
  ).not.toHaveCount(0)

  await expectConsoleRequests(requests, { allowed })
})
