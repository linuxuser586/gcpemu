import { fileURLToPath } from 'node:url'

import type { APIRequestContext } from '@playwright/test'

import { expect, expectConsoleRequests, projectId, test } from './fixtures'

// The Certificate Manager view (SRS 4.8.3, FR-LB-004) against the
// Instance's certs, dns and lb Services: a self-managed certificate is
// uploaded from files, and a Google-managed one stays PROVISIONING until
// the console's link to the fix adds its DNS authorization's CNAME, and a
// certificate map serves a target HTTPS proxy made in the Load Balancer
// view. Each
// attempt has its own Project, so a retry does not meet the resources of
// the one before.

// The console's paths: its bundle, the admin API and public GCP API paths.
const allowed = /^\/(console\/|_emu\/v1\/|[a-z]+\/v\d[a-z0-9]*\/)/

// A self-signed certificate for self.e2e.test and its key, made for this
// test only (valid until 2126).
const testdata = (f: string) => fileURLToPath(new URL(`./testdata/${f}`, import.meta.url))

/** compute calls the compute API and waits for the Operation. */
async function compute(api: APIRequestContext, path: string, data: unknown) {
  const res = await api.post(`/compute/v1/${path}`, { data })
  expect(res.ok(), await res.text()).toBe(true)
  const op = (await res.json()) as { selfLink: string }
  const rel = op.selfLink.slice(op.selfLink.indexOf('projects/'))
  const done = (await (await api.post(`/compute/v1/${rel}/wait`)).json()) as {
    status: string
    error?: unknown
  }
  expect(done.status).toBe('DONE')
  expect(done.error).toBeUndefined()
}

/** createDnsAuthorization creates one through the API and waits for its operation. */
async function createDnsAuthorization(api: APIRequestContext, project: string, domain: string) {
  const parent = `projects/${project}/locations/global`
  const res = await api.post(
    `/certificatemanager/v1/${parent}/dnsAuthorizations?dnsAuthorizationId=e2e`,
    { data: { domain } },
  )
  expect(res.ok(), await res.text()).toBe(true)
  let op = (await res.json()) as { name: string; done?: boolean; error?: unknown }
  while (!op.done) {
    op = (await (await api.get(`/certificatemanager/v1/${op.name}`)).json()) as typeof op
  }
  expect(op.error).toBeUndefined()
}

test('Certificate Manager: upload a certificate, and follow a managed one’s authorization to ACTIVE', async ({
  page,
  browserName,
  requests,
}, testInfo) => {
  const id = projectId(`e2e-cm${testInfo.retry}`, browserName)
  const domain = `${id}.e2e.test`
  await page.goto(`/console/certs?project=${id}`)
  await expect(page.getByText('No certificates in this Project yet.')).toBeVisible()

  // A self-managed certificate, its certificate and key picked from files.
  await page.getByRole('link', { name: 'Create certificate' }).click()
  await page.getByLabel('Name').fill('uploaded')
  await page.getByLabel('Certificate file').setInputFiles(testdata('self-cert.pem'))
  await page.getByLabel('Private key file').setInputFiles(testdata('self-key.pem'))
  await expect(page.getByLabel('PEM private key')).toHaveValue(/BEGIN PRIVATE KEY/)
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(
    new RegExp(`/console/certs/locations/global/certificates/uploaded\\?project=${id}$`),
  )
  const details = page.getByLabel('Certificate details')
  await expect(details).toContainText('Self-managed')
  await expect(details).toContainText('self.e2e.test')
  await expect(details).toContainText('ACTIVE')
  await expect(page.getByText(/PRIVATE KEY/)).toHaveCount(0)

  // A public zone for the domain, and a DNS authorization of it.
  const zone = await page.request.post(`/dns/v1/projects/${id}/managedZones`, {
    data: { name: 'e2e', dnsName: `${domain}.`, description: 'e2e', visibility: 'public' },
  })
  expect(zone.ok(), await zone.text()).toBe(true)
  await createDnsAuthorization(page.request, id, domain)

  // A Google-managed certificate whose CNAME is missing.
  await page.getByRole('link', { name: 'Certificates', exact: true }).first().click()
  await page.getByRole('link', { name: 'Create certificate' }).click()
  await page.getByLabel('Name').fill('managed')
  await page.getByLabel('Type').selectOption('dns')
  await page.getByLabel('Domains').fill(domain)
  await page.getByRole('checkbox', { name: /e2e/ }).check()
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(/\/certificates\/managed\?/)
  await expect(page.getByLabel('Certificate details')).toContainText('PROVISIONING')
  await expect(page.getByRole('alert')).toContainText('AUTHORIZATION_ISSUE')
  const row = page.getByTestId('authorization').filter({ hasText: domain })
  await expect(row).toContainText(`CNAME _acme-challenge.${domain}.`)

  // The link opens the record set form with the authorization's record.
  await row.getByRole('link', { name: 'Create the CNAME record' }).click()
  await expect(page).toHaveURL(/\/console\/dns\/zones\/e2e\/rrsets\/create\?/)
  await expect(page.getByLabel('DNS name')).toHaveValue('_acme-challenge')
  await expect(page.getByLabel('Type')).toHaveValue('CNAME')
  await expect(page.getByLabel('Data')).toHaveValue(/\.authorize\.certificatemanager\.goog\.$/)
  await page.getByRole('button', { name: 'Add' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/dns/zones/e2e\\?project=${id}$`))

  // Back on the certificate, it becomes ACTIVE.
  await page.goto(`/console/certs/locations/global/certificates/managed?project=${id}`)
  await expect(page.getByLabel('Certificate details')).toContainText('ACTIVE', {
    timeout: 15_000,
  })
  await expect(page.getByTestId('authorization').filter({ hasText: domain })).toContainText(
    'AUTHORIZED',
  )
  await expect(page.getByRole('alert')).toHaveCount(0)

  await expectConsoleRequests(requests, { allowed })
})

test('Certificate Manager: a certificate map serves an HTTPS proxy made in the Load Balancer view', async ({
  page,
  browserName,
  requests,
}, testInfo) => {
  const id = projectId(`e2e-cmap${testInfo.retry}`, browserName)
  const scope = `projects/${id}`

  // A self-managed certificate, and a URL map for the proxy.
  await page.goto(`/console/certs/create?project=${id}`)
  await page.getByLabel('Name').fill('uploaded')
  await page.getByLabel('Certificate file').setInputFiles(testdata('self-cert.pem'))
  await page.getByLabel('Private key file').setInputFiles(testdata('self-key.pem'))
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(/\/certificates\/uploaded\?/)
  await compute(page.request, `${scope}/global/urlMaps`, {
    name: 'web-map',
    defaultUrlRedirect: { hostRedirect: 'example.com' },
  })

  // A map with an entry for the certificate.
  await page.getByRole('link', { name: 'Certificates', exact: true }).first().click()
  await page.getByRole('link', { name: 'Certificate maps' }).click()
  await expect(page.getByText('No certificate maps in this Project yet.')).toBeVisible()
  await page.getByRole('link', { name: 'Create certificate map' }).click()
  await page.getByLabel('Name').fill('main')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(
    new RegExp(`/console/certs/locations/global/certificateMaps/main\\?project=${id}$`),
  )
  await expect(page.getByText('No target proxy serves this certificate map yet.')).toBeVisible()
  await page.getByRole('link', { name: 'Add entry' }).click()
  await page.getByLabel('Name', { exact: true }).fill('self')
  await page.getByLabel('Hostname').fill('self.e2e.test')
  await page.getByRole('checkbox', { name: /uploaded/ }).check()
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(/\/certificateMaps\/main\?/)
  await expect(page.getByTestId('certificate-map-entry')).toContainText(
    /self.*self\.e2e\.test.*uploaded.*ACTIVE/,
  )

  // An HTTPS proxy that serves the map, made in the Load Balancer view.
  await page.goto(`/console/lb/create/targetHttpsProxies?project=${id}`)
  await page.getByLabel('Name').fill('web-https')
  await page.getByLabel('URL map').selectOption({ label: 'web-map' })
  await page.getByLabel('Certificate map').selectOption({ label: 'main' })
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(/\/console\/lb\/global\/targetHttpsProxies\/web-https\?/, {
    timeout: 15_000,
  })
  await compute(page.request, `${scope}/global/forwardingRules`, {
    name: 'web-443',
    portRange: '443',
    loadBalancingScheme: 'EXTERNAL_MANAGED',
    target: 'global/targetHttpsProxies/web-https',
  })

  // The proxy links to the map, and the map lists the proxy as a target.
  await page.getByLabel('target HTTPS proxy details').getByRole('link', { name: 'main' }).click()
  await expect(page).toHaveURL(/\/certificateMaps\/main\?/)
  const target = page.getByTestId('gclb-target')
  await expect(target).toContainText('web-https')
  await expect(target.getByRole('link', { name: 'web-443' })).toBeVisible()
  await target.getByRole('link', { name: 'web-https' }).click()
  await expect(page).toHaveURL(/\/console\/lb\/global\/targetHttpsProxies\/web-https\?/)

  await expectConsoleRequests(requests, { allowed })
})
