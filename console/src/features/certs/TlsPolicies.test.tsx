import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, expect, it } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import type { Certificate } from './api'
import { tlsRecipe } from './copy'
import type { TlsResource } from './tls'

const P = 'alpha-project'
const G = `projects/${P}/locations/global`
const R = `projects/${P}/locations/us-central1`
const TRUST = `${G}/trustConfigs/corp`

// A small in-memory Network Security REST API for the three TLS policy
// kinds, checking what the Service checks; operations are done at once.
// Certificate Manager answers with trust configs and certificates, and
// compute with the target HTTPS proxies that use a server TLS policy.
let resources: TlsResource[] = []
let requests: { method: string; path: string; search: URLSearchParams; body: unknown }[] = []

const err = (code: number, status: string, message: string) =>
  HttpResponse.json({ error: { code, status, message } }, { status: code })

const done = (response: unknown) =>
  HttpResponse.json({ name: `${G}/operations/op-1`, done: true, response })

const certs: Certificate[] = [
  { name: `${G}/certificates/client`, scope: 'CLIENT_AUTH' },
  { name: `${G}/certificates/web` },
]

beforeEach(() => {
  requests = []
  fake.info.services = ['iam', 'certs', 'lb']
  resources = [
    {
      name: `${G}/serverTlsPolicies/mtls`,
      description: 'Partner mTLS',
      labels: { team: 'edge' },
      createTime: '2026-10-01T10:00:00Z',
      mtlsPolicy: { clientValidationMode: 'REJECT_INVALID', clientValidationTrustConfig: TRUST },
    },
    { name: `${R}/serverTlsPolicies/open`, allowOpen: true },
    { name: `${G}/clientTlsPolicies/backend`, sni: 'backend.example.com' },
    {
      name: `${G}/backendAuthenticationConfigs/origin`,
      trustConfig: TRUST,
      clientCertificate: `${G}/certificates/client`,
      wellKnownRoots: 'NONE',
    },
  ]
  server.use(
    http.all(/\/networksecurity\/v1\/(.*)$/, async ({ request }) => {
      const url = new URL(request.url)
      const text = request.method === 'GET' ? '' : await request.text()
      const body: unknown = text ? JSON.parse(text) : undefined
      const path = decodeURIComponent(url.pathname.replace(/^.*\/networksecurity\/v1\//, ''))
      requests.push({ method: request.method, path, search: url.searchParams, body })
      const m =
        /^projects\/([^/]+)\/locations\/([^/]+)\/(serverTlsPolicies|clientTlsPolicies|backendAuthenticationConfigs)(?:\/([^/]+))?$/.exec(
          path,
        )
      if (!m) return err(404, 'NOT_FOUND', `bad path ${path}`)
      const [, project, location, coll, id] = m
      if (!id) {
        if (request.method === 'GET') {
          const list = resources.filter(
            (r) =>
              r.name.includes(`/${coll}/`) &&
              (location === '-' || r.name.includes(`/locations/${location}/`)) &&
              r.name.startsWith(`projects/${project}/`),
          )
          return HttpResponse.json(list.length ? { [coll!]: list } : {})
        }
        const idParam = {
          serverTlsPolicies: 'serverTlsPolicyId',
          clientTlsPolicies: 'clientTlsPolicyId',
          backendAuthenticationConfigs: 'backendAuthenticationConfigId',
        }[coll!]!
        const name = `projects/${project}/locations/${location}/${coll}/${url.searchParams.get(idParam)}`
        if (resources.some((r) => r.name === name)) {
          return err(409, 'ALREADY_EXISTS', `Resource ${name} already exists.`)
        }
        const r = { ...(body as TlsResource), name }
        resources.push(r)
        return done(r)
      }
      const name = `projects/${project}/locations/${location}/${coll}/${id}`
      const r = resources.find((x) => x.name === name)
      if (!r) return err(404, 'NOT_FOUND', `Resource ${name} not found.`)
      if (request.method === 'DELETE') {
        resources = resources.filter((x) => x !== r)
        return done({})
      }
      if (request.method === 'PATCH') {
        Object.assign(r, body)
        return done(r)
      }
      return HttpResponse.json(r)
    }),
    http.get(/\/certificatemanager\/v1\/projects\/[^/]+\/locations$/, () =>
      HttpResponse.json({
        locations: [
          { name: `projects/${P}/locations/global`, locationId: 'global' },
          { name: `projects/${P}/locations/us-central1`, locationId: 'us-central1' },
        ],
      }),
    ),
    http.get(`*/certificatemanager/v1/${G}/trustConfigs`, () =>
      HttpResponse.json({
        trustConfigs: [{ name: TRUST }, { name: `${G}/trustConfigs/spare` }],
      }),
    ),
    http.get(`*/certificatemanager/v1/${R}/trustConfigs`, () => HttpResponse.json({})),
    http.get(`*/certificatemanager/v1/${TRUST}`, () =>
      HttpResponse.json({
        name: TRUST,
        description: 'Corporate CA',
        trustStores: [{ trustAnchors: [{ pemCertificate: 'PEM' }], intermediateCas: [] }],
      }),
    ),
    http.get(`*/certificatemanager/v1/projects/${P}/locations/-/certificates`, () =>
      HttpResponse.json({ certificates: certs }),
    ),
    http.get(`*/compute/v1/projects/${P}/aggregated/targetHttpsProxies`, () =>
      HttpResponse.json({
        items: {
          global: {
            targetHttpsProxies: [
              {
                name: 'partner-https',
                selfLink: `https://www.googleapis.com/compute/v1/projects/${P}/global/targetHttpsProxies/partner-https`,
                serverTlsPolicy: `//networksecurity.googleapis.com/${G}/serverTlsPolicies/mtls`,
              },
              {
                name: 'plain-https',
                selfLink: `https://www.googleapis.com/compute/v1/projects/${P}/global/targetHttpsProxies/plain-https`,
              },
            ],
          },
        },
      }),
    ),
  )
})

const sent = (method: string, path: RegExp) =>
  requests.filter((r) => r.method === method && path.test(r.path))

it('lists the Project’s server TLS policies of every location', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs?project=${P}`)
  await user.click(await screen.findByRole('link', { name: 'TLS policies' }))
  const table = await screen.findByRole('table', { name: 'Server TLS policies' })
  const rows = within(table).getAllByTestId('tls-resource')
  expect(rows).toHaveLength(2)
  expect(rows[0]).toHaveTextContent(/mtls.*global.*Partner mTLS.*REJECT_INVALID.*corp/)
  expect(rows[1]).toHaveTextContent(/open.*us-central1/)
  expect(within(rows[0]!).getByRole('link', { name: 'corp' })).toHaveAttribute(
    'href',
    `/certs/locations/global/trustConfigs/corp?project=${P}`,
  )
  await user.click(within(table).getByRole('link', { name: 'mtls' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/certs/locations/global/serverTlsPolicies/mtls'),
  )
})

it('switches between the kinds, each with the trust config it verifies with', async () => {
  const user = userEvent.setup()
  renderApp(`/certs/tls?project=${P}`)
  const kinds = await screen.findByRole('navigation', { name: 'TLS policy types' })
  await user.click(within(kinds).getByRole('link', { name: 'Backend authentication configs' }))
  const bacs = await screen.findByRole('table', { name: 'Backend authentication configs' })
  expect(within(bacs).getByTestId('tls-resource')).toHaveTextContent(/origin.*global.*corp/)
  await user.click(within(kinds).getByRole('link', { name: 'Client TLS policies' }))
  const clients = await screen.findByRole('table', { name: 'Client TLS policies' })
  expect(within(clients).getByTestId('tls-resource')).toHaveTextContent(
    /backend.*SNI backend\.example\.com/,
  )
})

it('shows a server TLS policy with its trust config and the HTTPS proxies that use it', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/locations/global/serverTlsPolicies/mtls?project=${P}`)
  expect(await screen.findByRole('heading', { name: 'mtls' })).toBeInTheDocument()
  const details = screen.getByLabelText('Server TLS policy details')
  expect(details).toHaveTextContent('Reject invalid client certificates')
  expect(details).toHaveTextContent('team=edge')
  expect(within(details).getByRole('link', { name: 'corp' })).toHaveAttribute(
    'href',
    `/certs/locations/global/trustConfigs/corp?project=${P}`,
  )
  const proxies = await screen.findByRole('table', { name: 'Target HTTPS proxies' })
  const rows = await within(proxies).findAllByTestId('tls-proxy')
  expect(rows).toHaveLength(1)
  await user.click(within(rows[0]!).getByRole('link', { name: 'partner-https' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/lb/global/targetHttpsProxies/partner-https'),
  )
})

it('says when no target HTTPS proxy uses a server TLS policy', async () => {
  renderApp(`/certs/locations/us-central1/serverTlsPolicies/open?project=${P}`)
  expect(
    await screen.findByText('No target HTTPS proxy uses this server TLS policy yet.'),
  ).toBeInTheDocument()
  expect(screen.getByLabelText('Server TLS policy details')).toHaveTextContent('Allow open')
})

it('shows a backend authentication config with its trust config and client certificate', async () => {
  renderApp(`/certs/locations/global/backendAuthenticationConfigs/origin?project=${P}`)
  const details = await screen.findByLabelText('Backend authentication config details')
  expect(within(details).getByRole('link', { name: 'corp' })).toBeInTheDocument()
  expect(within(details).getByRole('link', { name: 'client' })).toHaveAttribute(
    'href',
    `/certs/locations/global/certificates/client?project=${P}`,
  )
  expect(details).toHaveTextContent('NONE')
})

it('creates a server TLS policy that verifies clients with a trust config, as the API checks it', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/tls?project=${P}`)
  await user.click(await screen.findByRole('link', { name: 'Create server TLS policy' }))
  await user.type(await screen.findByLabelText('Name'), 'Bad_ID')
  await user.selectOptions(screen.getByLabelText('Client validation'), 'REJECT_INVALID')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(
    await screen.findByText(/Invalid serverTlsPolicyId "Bad_ID": must be 1-63 characters/),
  ).toBeInTheDocument()
  expect(
    screen.getByText(
      'mtls_policy.client_validation_trust_config is required when client_validation_mode is REJECT_INVALID.',
    ),
  ).toBeInTheDocument()

  await user.clear(screen.getByLabelText('Name'))
  await user.type(screen.getByLabelText('Name'), 'partners')
  await user.type(screen.getByLabelText('Description'), 'Partner mTLS')
  await user.selectOptions(await screen.findByLabelText('Trust config'), 'corp')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe(
      '/certs/locations/global/serverTlsPolicies/partners',
    ),
  )
  const [post] = sent('POST', /global\/serverTlsPolicies$/)
  expect(post!.search.get('serverTlsPolicyId')).toBe('partners')
  expect(post!.body).toEqual({
    description: 'Partner mTLS',
    mtlsPolicy: { clientValidationMode: 'REJECT_INVALID', clientValidationTrustConfig: TRUST },
  })
})

it('creates a backend authentication config with a client certificate, as the API checks it', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/tls/backendAuthenticationConfigs/create?project=${P}`)
  await user.type(await screen.findByLabelText('Name'), 'origin-auth')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(
    await screen.findByText(
      'Field trust_config is required unless well_known_roots is PUBLIC_ROOTS.',
    ),
  ).toBeInTheDocument()

  // Only CLIENT_AUTH certificates can be a client certificate.
  const cert = screen.getByLabelText('Client certificate')
  expect(
    within(cert)
      .getAllByRole('option')
      .map((o) => o.textContent),
  ).toEqual(['None', 'client'])
  await user.selectOptions(cert, 'client')
  await user.selectOptions(screen.getByLabelText('Well-known roots'), 'PUBLIC_ROOTS')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe(
      '/certs/locations/global/backendAuthenticationConfigs/origin-auth',
    ),
  )
  expect(sent('POST', /backendAuthenticationConfigs$/)[0]!.body).toEqual({
    wellKnownRoots: 'PUBLIC_ROOTS',
    clientCertificate: `${G}/certificates/client`,
  })
})

it('creates a regional client TLS policy with its SNI', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/tls/clientTlsPolicies/create?project=${P}`)
  await user.type(await screen.findByLabelText('Name'), 'mesh')
  await user.selectOptions(await screen.findByLabelText('Location'), 'us-central1')
  await user.type(screen.getByLabelText('SNI'), 'svc.internal')
  await user.type(screen.getByLabelText('Labels'), 'team=mesh')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe(
      '/certs/locations/us-central1/clientTlsPolicies/mesh',
    ),
  )
  const [post] = sent('POST', /us-central1\/clientTlsPolicies$/)
  expect(post!.search.get('clientTlsPolicyId')).toBe('mesh')
  expect(post!.body).toEqual({ sni: 'svc.internal', labels: { team: 'mesh' } })
})

it('edits a server TLS policy’s client validation and labels, with an updateMask', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/locations/global/serverTlsPolicies/mtls?project=${P}`)
  await user.click(await screen.findByRole('link', { name: 'Edit server TLS policy' }))
  expect(await screen.findByLabelText('Client validation')).toHaveValue('REJECT_INVALID')
  expect(await screen.findByLabelText('Trust config')).toHaveValue(TRUST)
  await user.selectOptions(
    screen.getByLabelText('Client validation'),
    'ALLOW_INVALID_OR_MISSING_CLIENT_CERT',
  )
  await user.clear(screen.getByLabelText('Labels'))
  await user.type(screen.getByLabelText('Labels'), 'team=partners')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/certs/locations/global/serverTlsPolicies/mtls'),
  )
  const [patch] = sent('PATCH', /serverTlsPolicies\/mtls$/)
  expect(patch!.search.get('updateMask')).toBe('labels,mtls_policy')
  expect(patch!.body).toEqual({
    labels: { team: 'partners' },
    mtlsPolicy: {
      clientValidationMode: 'ALLOW_INVALID_OR_MISSING_CLIENT_CERT',
      clientValidationTrustConfig: TRUST,
    },
  })
})

it('turns mutual TLS off by clearing the policy’s mtlsPolicy', async () => {
  const user = userEvent.setup()
  renderApp(`/certs/locations/global/serverTlsPolicies/mtls/edit?project=${P}`)
  await user.selectOptions(await screen.findByLabelText('Client validation'), '')
  expect(screen.queryByLabelText('Trust config')).not.toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(sent('PATCH', /serverTlsPolicies\/mtls$/)).toHaveLength(1))
  const [patch] = sent('PATCH', /serverTlsPolicies\/mtls$/)
  expect(patch!.search.get('updateMask')).toBe('mtls_policy')
  expect(patch!.body).toEqual({})
})

it('saves nothing when nothing changed', async () => {
  const user = userEvent.setup()
  renderApp(`/certs/locations/global/backendAuthenticationConfigs/origin/edit?project=${P}`)
  expect(await screen.findByLabelText('Client certificate')).toHaveValue(`${G}/certificates/client`)
  await user.click(screen.getByRole('button', { name: 'Save' }))
  expect(await screen.findByText('Nothing to change.')).toBeInTheDocument()
  expect(sent('PATCH', /./)).toHaveLength(0)
})

it('deletes a server TLS policy', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/locations/us-central1/serverTlsPolicies/open?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Delete server TLS policy' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', {
      name: 'Delete server TLS policy',
    }),
  )
  await waitFor(() => expect(router.state.location.pathname).toBe('/certs/tls/serverTlsPolicies'))
  expect(sent('DELETE', /us-central1\/serverTlsPolicies\/open$/)).toHaveLength(1)
})

it('copies a server TLS policy from its page', async () => {
  const user = userEvent.setup()
  renderApp(`/certs/locations/global/serverTlsPolicies/mtls?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Copy server TLS policy as' }))
  const dialog = await screen.findByRole('dialog', { name: 'Copy server TLS policy as' })
  expect(within(dialog).getByLabelText('gcloud for the server TLS policy')).toHaveTextContent(
    'gcloud network-security server-tls-policies import mtls',
  )
})

it('copies server TLS policies, client TLS policies and backend authentication configs', () => {
  const [mtls, open, client, origin] = resources
  const server = tlsRecipe(mtls!, P)
  expect(server.gcloud).toBe(
    [
      'gcloud network-security server-tls-policies import mtls \\',
      `    --project=${P} \\`,
      "    --location=global <<'EOF'",
      JSON.stringify(
        {
          description: 'Partner mTLS',
          labels: { team: 'edge' },
          mtlsPolicy: {
            clientValidationMode: 'REJECT_INVALID',
            clientValidationTrustConfig: TRUST,
          },
        },
        null,
        2,
      ),
      'EOF',
    ].join('\n'),
  )
  expect(server.rest).toEqual({
    method: 'POST',
    path: `/networksecurity/v1/${G}/serverTlsPolicies?serverTlsPolicyId=mtls`,
    body: {
      description: 'Partner mTLS',
      labels: { team: 'edge' },
      mtlsPolicy: { clientValidationMode: 'REJECT_INVALID', clientValidationTrustConfig: TRUST },
    },
  })
  expect(server.tofu).toBe(
    [
      'resource "google_network_security_server_tls_policy" "mtls" {',
      `  project     = "${P}"`,
      '  name        = "mtls"',
      '  location    = "global"',
      '  description = "Partner mTLS"',
      '  labels      = { team = "edge" }',
      '  mtls_policy {',
      '    client_validation_mode         = "REJECT_INVALID"',
      `    client_validation_trust_config = "${TRUST}"`,
      '  }',
      '}',
    ].join('\n'),
  )
  expect(tlsRecipe(open!, P).tofu).toContain('  location   = "us-central1"\n  allow_open = true')

  expect(tlsRecipe(client!, P).tofu).toBe(
    [
      'resource "google_network_security_client_tls_policy" "backend" {',
      `  project  = "${P}"`,
      '  name     = "backend"',
      '  location = "global"',
      '  sni      = "backend.example.com"',
      '}',
    ].join('\n'),
  )

  const bac = tlsRecipe(origin!, P)
  expect(bac.gcloud).toBe(
    [
      'gcloud network-security backend-authentication-configs create origin \\',
      `    --project=${P} \\`,
      '    --location=global \\',
      `    --trust-config=${TRUST} \\`,
      '    --well-known-roots=none \\',
      `    --client-certificate=${G}/certificates/client`,
    ].join('\n'),
  )
  expect(bac.tofu).toBe(
    [
      'resource "google_network_security_backend_authentication_config" "origin" {',
      `  project            = "${P}"`,
      '  name               = "origin"',
      '  location           = "global"',
      `  client_certificate = "${G}/certificates/client"`,
      `  trust_config       = "${TRUST}"`,
      '  well_known_roots   = "NONE"',
      '}',
    ].join('\n'),
  )
})

it('shows a trust config with the TLS policies that verify with it', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/locations/global/trustConfigs/corp?project=${P}`)
  expect(await screen.findByRole('heading', { name: 'corp' })).toBeInTheDocument()
  const details = await screen.findByLabelText('Trust config details')
  expect(details).toHaveTextContent('Corporate CA')
  expect(details).toHaveTextContent(/Trust anchors\s*1/)
  const users = await screen.findByRole('table', { name: 'Used by' })
  await waitFor(() => expect(within(users).getAllByTestId('trust-user')).toHaveLength(2))
  expect(within(users).getAllByTestId('trust-user')[1]).toHaveTextContent(
    /Backend authentication config.*origin/,
  )
  await user.click(within(users).getByRole('link', { name: 'mtls' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/certs/locations/global/serverTlsPolicies/mtls'),
  )
})

it('keeps a server TLS policy’s validation CAs when an edit leaves client validation off', async () => {
  resources[1]!.mtlsPolicy = { clientValidationCa: [{ grpcEndpoint: { targetUri: 'unix:ca' } }] }
  const user = userEvent.setup()
  renderApp(`/certs/locations/us-central1/serverTlsPolicies/open/edit?project=${P}`)
  await user.type(await screen.findByLabelText('Description'), 'Mesh')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(sent('PATCH', /serverTlsPolicies\/open$/)).toHaveLength(1))
  const [patch] = sent('PATCH', /serverTlsPolicies\/open$/)
  expect(patch!.search.get('updateMask')).toBe('description')
  expect(patch!.body).toEqual({ description: 'Mesh' })
})
