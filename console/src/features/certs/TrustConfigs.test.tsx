import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, expect, it } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import type { TrustConfig } from './api'
import { trustConfigRecipe } from './copy'
import { fakeCertManager, type Res, type Sent } from './fakeCertManager'
import { INTERMEDIATE_PEM, ROOT_PEM } from './testcerts'

const P = 'alpha-project'
const G = `projects/${P}/locations/global`
const R = `projects/${P}/locations/us-central1`

const corp: TrustConfig = {
  name: `${G}/trustConfigs/corp`,
  description: 'Corporate CA',
  labels: { team: 'sec' },
  etag: 'e-1',
  createTime: '2026-10-01T10:00:00Z',
  trustStores: [
    {
      trustAnchors: [{ pemCertificate: ROOT_PEM }],
      intermediateCas: [{ pemCertificate: INTERMEDIATE_PEM }],
    },
  ],
}

let resources: Res[] = []
let requests: Sent[] = []

beforeEach(() => {
  requests = []
  fake.info.services = ['iam', 'certs', 'lb']
  resources = [
    structuredClone(corp) as Res,
    {
      name: `${R}/trustConfigs/edge`,
      etag: 'e-2',
      allowlistedCertificates: [{ pemCertificate: ROOT_PEM }],
    },
  ]
  server.use(
    fakeCertManager(resources, requests, { inUse: [`${G}/trustConfigs/corp`] }),
    http.get(/\/certificatemanager\/v1\/projects\/[^/]+\/locations$/, () =>
      HttpResponse.json({
        locations: [
          { name: G, locationId: 'global' },
          { name: R, locationId: 'us-central1' },
        ],
      }),
    ),
    http.get(
      /\/networksecurity\/v1\/projects\/[^/]+\/locations\/-\/(serverTlsPolicies|backendAuthenticationConfigs)$/,
      ({ request }) =>
        HttpResponse.json(
          request.url.endsWith('/serverTlsPolicies')
            ? {
                serverTlsPolicies: [
                  {
                    name: `${G}/serverTlsPolicies/partners`,
                    mtlsPolicy: {
                      clientValidationMode: 'REJECT_INVALID',
                      clientValidationTrustConfig: corp.name,
                    },
                  },
                ],
              }
            : {},
        ),
    ),
  )
})

const sent = (method: string, path: RegExp) =>
  requests.filter((r) => r.method === method && path.test(r.path))

it('lists the Project’s trust configs of every location, with how many certificates each has', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs?project=${P}`)
  await user.click(await screen.findByRole('link', { name: 'Trust configs' }))
  const table = await screen.findByRole('table', { name: 'Trust configs' })
  const rows = within(table).getAllByTestId('trust-config')
  expect(rows).toHaveLength(2)
  expect(rows[0]).toHaveTextContent(/corp.*global.*1.*1.*0.*Corporate CA/)
  expect(rows[1]).toHaveTextContent(/edge.*us-central1.*0.*0.*1/)
  await user.click(within(table).getByRole('link', { name: 'corp' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/certs/locations/global/trustConfigs/corp'),
  )
})

it('shows each certificate of a trust config with its subject and expiry, and what uses it', async () => {
  renderApp(`/certs/locations/global/trustConfigs/corp?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Certificates' })
  const rows = within(table).getAllByTestId('trust-certificate')
  expect(rows).toHaveLength(2)
  const expires = (iso: string) => `Expires ${new Date(iso).toLocaleString()}`
  expect(rows[0]).toHaveTextContent('Trust anchor')
  expect(rows[0]).toHaveTextContent('C=US, O=Example Corp, CN=Example Root CA')
  expect(rows[0]).not.toHaveTextContent('Issued by')
  expect(rows[0]).toHaveTextContent(expires('2036-01-01T00:00:00Z'))
  expect(rows[1]).toHaveTextContent('Intermediate CA')
  expect(rows[1]).toHaveTextContent('O=Example Corp, OU=Edge, CN=Example Issuing CA')
  expect(rows[1]).toHaveTextContent('Issued by C=US, O=Example Corp, CN=Example Root CA')
  expect(rows[1]).toHaveTextContent(expires('2051-01-01T00:00:00Z'))
  expect(await screen.findByTestId('trust-user')).toHaveTextContent('partners')
  expect(screen.getByLabelText('Trust config details')).toHaveTextContent('team=sec')
  expect(screen.getByRole('link', { name: 'Trust configs' })).toHaveAttribute(
    'href',
    `/certs/trustConfigs?project=${P}`,
  )
})

it('creates a trust config from pasted and uploaded PEM, showing what each certificate is', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/trustConfigs?project=${P}`)
  await user.click(await screen.findByRole('link', { name: 'Create trust config' }))
  await user.type(await screen.findByLabelText('Name'), 'partners')
  await user.type(screen.getByLabelText('Trust anchor 1'), 'not a certificate')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(
    await screen.findByText(
      'Invalid PEM certificate in trust_stores.trust_anchors.pem_certificate: no CERTIFICATE block found.',
    ),
  ).toBeInTheDocument()

  // The anchor is read from a file, the intermediate CA pasted.
  await user.upload(
    screen.getByLabelText('Read trust anchor 1 from a file'),
    new File([ROOT_PEM], 'root.pem', { type: 'application/x-pem-file' }),
  )
  await waitFor(() => expect(screen.getByLabelText('Trust anchor 1')).toHaveValue(ROOT_PEM))
  const anchors = screen.getByRole('group', { name: 'Trust anchors' })
  expect(within(anchors).getByTestId('pem-summary')).toHaveTextContent('CN=Example Root CA')
  await user.click(screen.getByRole('button', { name: 'Add intermediate CA' }))
  await user.click(screen.getByLabelText('Intermediate CA 1'))
  await user.paste(INTERMEDIATE_PEM)
  const inters = screen.getByRole('group', { name: 'Intermediate CAs' })
  expect(within(inters).getByTestId('pem-summary')).toHaveTextContent(
    'Issued by C=US, O=Example Corp, CN=Example Root CA',
  )
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/certs/locations/global/trustConfigs/partners'),
  )
  const [post] = sent('POST', /global\/trustConfigs$/)
  expect(post!.search.get('trustConfigId')).toBe('partners')
  expect(post!.body).toEqual({
    trustStores: [
      {
        trustAnchors: [{ pemCertificate: ROOT_PEM }],
        intermediateCas: [{ pemCertificate: INTERMEDIATE_PEM }],
      },
    ],
  })
})

it('edits a trust config with its etag, sending the fields that changed', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/locations/global/trustConfigs/corp?project=${P}`)
  await user.click(await screen.findByRole('link', { name: 'Edit trust config' }))
  expect(await screen.findByLabelText('Trust anchor 1')).toHaveValue(ROOT_PEM)
  expect(screen.getByLabelText('Intermediate CA 1')).toHaveValue(INTERMEDIATE_PEM)
  await user.click(screen.getByRole('button', { name: 'Remove intermediate CA 1' }))
  await user.clear(screen.getByLabelText('Description'))
  await user.type(screen.getByLabelText('Description'), 'Root only')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/certs/locations/global/trustConfigs/corp'),
  )
  const [patch] = sent('PATCH', /trustConfigs\/corp$/)
  expect(patch!.search.get('updateMask')).toBe('description,trust_stores')
  expect(patch!.body).toEqual({
    etag: 'e-1',
    description: 'Root only',
    trustStores: [{ trustAnchors: [{ pemCertificate: ROOT_PEM }] }],
  })
  const table = await screen.findByRole('table', { name: 'Certificates' })
  await waitFor(() => expect(within(table).getAllByTestId('trust-certificate')).toHaveLength(1))
})

it('says when the trust config changed since the edit form read it', async () => {
  const user = userEvent.setup()
  renderApp(`/certs/locations/global/trustConfigs/corp/edit?project=${P}`)
  await user.type(await screen.findByLabelText('Description'), ' (old)')
  resources[0]!.etag = 'e-9'
  await user.click(screen.getByRole('button', { name: 'Save' }))
  expect(
    await screen.findByText(/The etag "e-1" does not match the current etag/),
  ).toBeInTheDocument()
})

it('keeps the etag the edit form read when the trust config is read again in the background', async () => {
  const user = userEvent.setup()
  const { queryClient } = renderApp(`/certs/locations/global/trustConfigs/corp/edit?project=${P}`)
  await user.type(await screen.findByLabelText('Description'), ' (old)')
  resources[0]!.etag = 'e-9'
  resources[0]!.description = 'Changed elsewhere'
  await queryClient.refetchQueries({ queryKey: ['certs', 'trustConfig'] })
  await user.click(screen.getByRole('button', { name: 'Save' }))
  expect(
    await screen.findByText(/The etag "e-1" does not match the current etag/),
  ).toBeInTheDocument()
})

it('deletes a trust config once nothing uses it', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/locations/global/trustConfigs/corp?project=${P}`)
  const del = async () => {
    await user.click(await screen.findByRole('button', { name: 'Delete trust config' }))
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Delete trust config' }))
  }
  await del()
  expect(await screen.findByText(/is in use by/)).toBeInTheDocument()
  await user.keyboard('{Escape}')

  await user.click(screen.getByRole('link', { name: 'Trust configs' }))
  await user.click(await screen.findByRole('link', { name: 'edge' }))
  await del()
  await waitFor(() => expect(router.state.location.pathname).toBe('/certs/trustConfigs'))
  expect(sent('DELETE', /us-central1\/trustConfigs\/edge$/)).toHaveLength(1)
})

it('copies a trust config as a gcloud import, REST and OpenTofu', () => {
  const r = trustConfigRecipe(corp, P)
  const body = {
    description: 'Corporate CA',
    labels: { team: 'sec' },
    trustStores: corp.trustStores,
  }
  expect(r.gcloud).toBe(
    [
      'gcloud certificate-manager trust-configs import corp \\',
      `    --project=${P} \\`,
      `    --location=global <<'EOF'`,
      JSON.stringify(body, null, 2),
      'EOF',
    ].join('\n'),
  )
  expect(r.rest).toEqual({
    method: 'POST',
    path: `/certificatemanager/v1/${G}/trustConfigs?trustConfigId=corp`,
    body,
  })
  const pem = (s: string) => JSON.stringify(s)
  expect(r.tofu).toContain('resource "google_certificate_manager_trust_config" "corp" {')
  expect(r.tofu).toContain('  location    = "global"\n')
  expect(r.tofu).toContain(
    `  trust_stores {\n    trust_anchors {\n      pem_certificate = ${pem(ROOT_PEM)}\n    }\n    intermediate_cas {\n      pem_certificate = ${pem(INTERMEDIATE_PEM)}\n    }\n  }`,
  )
  expect(r.tofu).not.toContain('etag')
})
