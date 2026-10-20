import { expect, expectConsoleRequests, projectId, test } from './fixtures'
import { startInstance, type Instance } from './instance'

// FR-UI-013: the Request log is the Instance's and keeps only its last
// 2000 entries. On the shared Instance every other spec's traffic, and the
// DNS, NAT, load balancer and Cloud SQL entries written on their own, can
// evict this test's requests before the reload reads the log back, so it
// runs against an Instance of its own per browser. Pub/Sub runs only so the
// Service filter offers it; neither Service writes to the log unprompted.

const allowed = /^\/(console\/|_emu\/v1\/|[a-z]+\/v\d[a-z0-9]*\/)/

let inst: Instance
test.beforeAll(async () => {
  inst = await startInstance(['gcs', 'pubsub'])
})
test.afterAll(async () => {
  await inst?.stop()
})

test('tails the Request log live, filters it and expands a request to its bodies', async ({
  page,
  request,
  requests,
  browserName,
}) => {
  const id = projectId('reqlog', browserName)
  const bucket = `${id}-${Date.now()}`

  expect(
    (await request.post(`${inst.url}/storage/v1/b?project=${id}`, { data: { name: bucket } })).ok(),
  ).toBe(true)

  await page.goto(`${inst.url}/console/`)
  await page
    .getByRole('navigation', { name: 'Services' })
    .getByRole('link', { name: 'Request log' })
    .click()
  await expect(page.getByRole('heading', { name: 'Request log' })).toBeVisible()
  await page.getByRole('searchbox', { name: 'Resource' }).fill(`buckets/${bucket}`)
  await expect(page).toHaveURL(/resource=buckets/)

  // The view is open while the requests are made: they arrive live.
  const patched = await request.patch(`${inst.url}/storage/v1/b/${bucket}`, {
    data: { labels: { browser: browserName } },
  })
  expect(patched.ok()).toBe(true)
  expect((await request.get(`${inst.url}/storage/v1/b/${bucket}/o/missing`)).status()).toBe(404)

  const rows = page.getByRole('table', { name: 'Requests' }).getByTestId('request')
  await expect(rows).toHaveCount(2)
  await expect(rows.first()).toContainText('GET')
  await expect(rows.first()).toContainText('/o/missing')
  await expect(rows.first()).toContainText('404')
  await expect(rows.last()).toContainText('PATCH')

  await page.getByRole('searchbox', { name: 'Status' }).fill('4xx')
  await expect(rows).toHaveCount(1)
  await expect(rows.first()).toContainText('/o/missing')
  await page.getByRole('searchbox', { name: 'Status' }).fill('')
  await page.getByRole('searchbox', { name: 'Method' }).fill('patch')
  await expect(rows).toHaveCount(1)
  await page.getByRole('combobox', { name: 'Service' }).selectOption({ label: 'Pub/Sub' })
  await expect(page.getByText('No requests match the filters.')).toBeVisible()
  await page.getByRole('combobox', { name: 'Service' }).selectOption({ label: 'Cloud Storage' })
  await page.getByRole('searchbox', { name: 'Principal' }).fill('user:dev@example.com')
  await expect(rows).toHaveCount(1)

  // The filters survive a reload, and the log is read back from the Instance.
  await page.reload()
  await expect(page.getByRole('searchbox', { name: 'Method' })).toHaveValue('patch')
  await expect(rows).toHaveCount(1)

  await rows
    .first()
    .getByRole('button', { name: /^Details of PATCH/ })
    .click()
  const details = page.getByRole('region', { name: /^Details of PATCH/ })
  await expect(details.getByRole('region', { name: 'Request body' })).toContainText(
    `"browser": "${browserName}"`,
  )
  await expect(details.getByRole('region', { name: 'Response body' })).toContainText(
    '"kind": "storage#bucket"',
  )
  await details.getByRole('button', { name: 'Close details' }).click()
  await expect(details).toHaveCount(0)

  await expectConsoleRequests(requests, { allowed })
})
