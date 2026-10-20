import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, expect, it } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import type { IssuanceConfig } from './api'
import { issuanceConfigRecipe } from './copy'
import { fakeCertManager, type Res, type Sent } from './fakeCertManager'

const P = 'alpha-project'
const G = `projects/${P}/locations/global`
const R = `projects/${P}/locations/us-central1`
const POOL = `projects/${P}/locations/us-central1/caPools/web-pool`

const web: IssuanceConfig = {
  name: `${G}/certificateIssuanceConfigs/web`,
  description: 'Private web certificates',
  createTime: '2026-10-01T10:00:00Z',
  certificateAuthorityConfig: { certificateAuthorityServiceConfig: { caPool: POOL } },
  lifetime: '2592000s',
  rotationWindowPercentage: 66,
  keyAlgorithm: 'ECDSA_P256',
}

let resources: Res[] = []
let requests: Sent[] = []

beforeEach(() => {
  requests = []
  fake.info.services = ['iam', 'certs']
  resources = [
    structuredClone(web) as Res,
    {
      name: `${R}/certificateIssuanceConfigs/internal`,
      certificateAuthorityConfig: { certificateAuthorityServiceConfig: { caPool: POOL } },
      lifetime: '1814400s',
      rotationWindowPercentage: 50,
    },
  ]
  server.use(
    fakeCertManager(resources, requests, { inUse: [web.name] }),
    http.get(/\/certificatemanager\/v1\/projects\/[^/]+\/locations$/, () =>
      HttpResponse.json({
        locations: [
          { name: G, locationId: 'global' },
          { name: R, locationId: 'us-central1' },
        ],
      }),
    ),
    http.get(`*/certificatemanager/v1/projects/${P}/locations/-/certificates`, () =>
      HttpResponse.json({
        certificates: [
          {
            name: `${G}/certificates/shop`,
            managed: { domains: ['shop.internal'], issuanceConfig: web.name },
          },
          { name: `${G}/certificates/self` },
        ],
      }),
    ),
  )
})

const sent = (method: string, path: RegExp) =>
  requests.filter((r) => r.method === method && path.test(r.path))

it('lists the Project’s certificate issuance configs of every location', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs?project=${P}`)
  await user.click(await screen.findByRole('link', { name: 'Issuance configs' }))
  const table = await screen.findByRole('table', { name: 'Certificate issuance configs' })
  const rows = within(table).getAllByTestId('issuance-config')
  expect(rows).toHaveLength(2)
  expect(rows[0]).toHaveTextContent(/web.*global.*web-pool.*30 days.*ECDSA_P256.*Private web/)
  expect(rows[1]).toHaveTextContent(/internal.*us-central1.*web-pool.*21 days.*Unspecified/)
  await user.click(within(table).getByRole('link', { name: 'internal' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe(
      '/certs/locations/us-central1/certificateIssuanceConfigs/internal',
    ),
  )
})

it('shows a certificate issuance config with the certificates it issues, and no edit', async () => {
  renderApp(`/certs/locations/global/certificateIssuanceConfigs/web?project=${P}`)
  const details = await screen.findByLabelText('Certificate issuance config details')
  expect(details).toHaveTextContent(POOL)
  expect(details).toHaveTextContent('30 days')
  expect(details).toHaveTextContent(/Rotation window\s*66%/)
  expect(screen.getByText(/cannot be changed/)).toBeInTheDocument()
  expect(screen.queryByRole('link', { name: /Edit/ })).toBeNull()
  const users = await screen.findByRole('table', { name: 'Certificates' })
  expect(within(users).getByRole('link', { name: 'shop' })).toHaveAttribute(
    'href',
    `/certs/locations/global/certificates/shop?project=${P}`,
  )
  expect(within(users).queryByRole('link', { name: 'self' })).toBeNull()
})

it('creates a certificate issuance config, as the API checks it', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/issuanceConfigs?project=${P}`)
  await user.click(await screen.findByRole('link', { name: 'Create certificate issuance config' }))
  await user.type(await screen.findByLabelText('Name'), 'mesh')
  await user.clear(screen.getByLabelText('Lifetime (days)'))
  await user.clear(screen.getByLabelText('Rotation window (%)'))
  await user.type(screen.getByLabelText('Rotation window (%)'), '100')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(
    await screen.findByText(
      'Field certificate_authority_config.certificate_authority_service_config.ca_pool is required.',
    ),
  ).toBeInTheDocument()
  expect(screen.getByText('Field lifetime is required.')).toBeInTheDocument()
  expect(
    screen.getByText('Field rotation_window_percentage must be between 1 and 99.'),
  ).toBeInTheDocument()

  await user.type(screen.getByLabelText('CA pool'), POOL)
  await user.type(screen.getByLabelText('Lifetime (days)'), '14')
  await user.clear(screen.getByLabelText('Rotation window (%)'))
  await user.type(screen.getByLabelText('Rotation window (%)'), '50')
  await user.selectOptions(screen.getByLabelText('Key algorithm'), 'ECDSA_P256')
  await user.selectOptions(await screen.findByLabelText('Location'), 'us-central1')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe(
      '/certs/locations/us-central1/certificateIssuanceConfigs/mesh',
    ),
  )
  const [post] = sent('POST', /us-central1\/certificateIssuanceConfigs$/)
  expect(post!.search.get('certificateIssuanceConfigId')).toBe('mesh')
  expect(post!.body).toEqual({
    certificateAuthorityConfig: { certificateAuthorityServiceConfig: { caPool: POOL } },
    lifetime: '1209600s',
    rotationWindowPercentage: 50,
    keyAlgorithm: 'ECDSA_P256',
  })
})

it('deletes a certificate issuance config once no certificate uses it', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(
    `/certs/locations/global/certificateIssuanceConfigs/web?project=${P}`,
  )
  const name = 'Delete certificate issuance config'
  const del = async () => {
    await user.click(await screen.findByRole('button', { name }))
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name }))
  }
  await del()
  expect(await screen.findByText(/is in use by/)).toBeInTheDocument()
  await user.keyboard('{Escape}')

  await user.click(screen.getByRole('link', { name: 'Certificate issuance configs' }))
  await user.click(await screen.findByRole('link', { name: 'internal' }))
  await del()
  await waitFor(() => expect(router.state.location.pathname).toBe('/certs/issuanceConfigs'))
  expect(sent('DELETE', /us-central1\/certificateIssuanceConfigs\/internal$/)).toHaveLength(1)
})

it('copies a certificate issuance config as gcloud, REST and OpenTofu', () => {
  const r = issuanceConfigRecipe(web, P)
  expect(r.gcloud).toBe(
    [
      'gcloud certificate-manager issuance-configs create web \\',
      `    --project=${P} \\`,
      `    --ca-pool=${POOL} \\`,
      '    --lifetime=2592000s \\',
      '    --rotation-window-percentage=66 \\',
      '    --key-algorithm=ecdsa-p256 \\',
      "    --description='Private web certificates'",
    ].join('\n'),
  )
  expect(r.rest).toEqual({
    method: 'POST',
    path: `/certificatemanager/v1/${G}/certificateIssuanceConfigs?certificateIssuanceConfigId=web`,
    body: {
      description: 'Private web certificates',
      certificateAuthorityConfig: { certificateAuthorityServiceConfig: { caPool: POOL } },
      lifetime: '2592000s',
      rotationWindowPercentage: 66,
      keyAlgorithm: 'ECDSA_P256',
    },
  })
  expect(r.tofu).toBe(
    [
      'resource "google_certificate_manager_certificate_issuance_config" "web" {',
      `  project                    = "${P}"`,
      '  name                       = "web"',
      '  description                = "Private web certificates"',
      '  lifetime                   = "2592000s"',
      '  rotation_window_percentage = 66',
      '  key_algorithm              = "ECDSA_P256"',
      '  certificate_authority_config {',
      '    certificate_authority_service_config {',
      `      ca_pool = "${POOL}"`,
      '    }',
      '  }',
      '}',
    ].join('\n'),
  )
})
