import type { APIRequestContext } from '@playwright/test'

import { expect, expectConsoleRequests, projectId, test } from './fixtures'
import { startInstance, type Instance } from './instance'

// Rotate CA in the Certificate Manager view: the Instance CA is replaced
// and a Google-managed certificate is re-issued from the new root.
// Rotation changes the whole Instance, and the shared one's other specs
// serve and verify TLS with its CA, so this runs against an Instance of
// its own per browser.

const allowed = /^\/(console\/|_emu\/v1\/|[a-z]+\/v\d[a-z0-9]*\/)/

let inst: Instance
test.beforeAll(async () => {
  inst = await startInstance(['certs'])
})
test.afterAll(async () => {
  await inst?.stop()
})

/** create creates a Certificate Manager resource and waits for its operation. */
async function create(
  api: APIRequestContext,
  parent: string,
  coll: string,
  q: string,
  data: unknown,
) {
  const res = await api.post(`${inst.url}/certificatemanager/v1/${parent}/${coll}?${q}`, { data })
  expect(res.ok(), await res.text()).toBe(true)
  let op = (await res.json()) as { name: string; done?: boolean; error?: unknown }
  while (!op.done) {
    op = (await (await api.get(`${inst.url}/certificatemanager/v1/${op.name}`)).json()) as typeof op
  }
  expect(op.error).toBeUndefined()
}

test('Certificate Manager: Rotate CA re-issues a Google-managed certificate', async ({
  page,
  browserName,
  requests,
}) => {
  const id = projectId('e2e-ca', browserName)
  const parent = `projects/${id}/locations/global`
  // A certificate issued through a certificate issuance config is ACTIVE at once.
  await create(
    page.request,
    parent,
    'certificateIssuanceConfigs',
    'certificateIssuanceConfigId=private',
    {
      certificateAuthorityConfig: {
        certificateAuthorityServiceConfig: {
          caPool: `projects/${id}/locations/us-central1/caPools/pool`,
        },
      },
      lifetime: '2592000s',
      rotationWindowPercentage: 66,
    },
  )
  await create(page.request, parent, 'certificates', 'certificateId=managed', {
    managed: {
      domains: [`${id}.e2e.test`],
      issuanceConfig: `${parent}/certificateIssuanceConfigs/private`,
    },
  })

  await page.goto(`${inst.url}/console/certs/locations/global/certificates/managed?project=${id}`)
  await expect(page.getByLabel('Certificate details')).toContainText('ACTIVE')
  const pem = page.getByLabel('PEM certificate')
  await expect(pem).toContainText('BEGIN CERTIFICATE')
  const before = await pem.textContent()

  await page.getByRole('link', { name: 'Certificates', exact: true }).click()
  await page.getByRole('button', { name: 'Rotate CA' }).click()
  const dialog = page.getByRole('dialog', { name: 'Rotate the Instance CA?' })
  await expect(dialog).toContainText('gcpemu ca install')
  await dialog.getByRole('button', { name: 'Rotate CA' }).click()
  await expect(dialog).toBeHidden()
  await expect(
    page.getByRole('status').filter({ hasText: 'Rotated the Instance CA' }),
  ).toContainText(/Run gcpemu ca install to trust the new CA at .*ca\.pem\./)

  // The certificate is still ACTIVE, with a certificate from the new CA.
  await page.getByRole('link', { name: 'managed' }).click()
  await expect(page.getByLabel('Certificate details')).toContainText('ACTIVE')
  await expect(pem).toContainText('BEGIN CERTIFICATE')
  await expect(pem).not.toHaveText(before!)

  await expectConsoleRequests(requests, { allowed })
})
