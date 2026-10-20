import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, expect, it } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import type { DnsAuthorization } from './api'
import { dnsAuthorizationRecipe } from './copy'
import { fakeCertManager, type Res, type Sent } from './fakeCertManager'

const P = 'alpha-project'
const G = `projects/${P}/locations/global`
const R = `projects/${P}/locations/us-central1`

const auths: DnsAuthorization[] = [
  {
    name: `${G}/dnsAuthorizations/www`,
    domain: 'www.example.com',
    type: 'FIXED_RECORD',
    description: 'Web front end',
    labels: { team: 'web' },
    dnsResourceRecord: {
      name: '_acme-challenge.www.example.com.',
      type: 'CNAME',
      data: '0b5e.4.authorize.certificatemanager.goog.',
    },
  },
  {
    name: `${R}/dnsAuthorizations/api`,
    domain: 'api.example.com',
    type: 'PER_PROJECT_RECORD',
  },
]

let resources: Res[] = []
let requests: Sent[] = []
// The CNAME the Instance's DNS answers for, by name.
let resolved: Record<string, string> = {}

beforeEach(() => {
  requests = []
  resolved = {}
  fake.info.services = ['iam', 'certs', 'dns']
  resources = auths.map((a) => ({ ...a, createTime: '2026-10-01T10:00:00Z' }))
  server.use(
    fakeCertManager(resources, requests, {
      inUse: [`${G}/dnsAuthorizations/www`],
      computed: (r) => ({
        ...r,
        type: r.type ?? 'FIXED_RECORD',
        dnsResourceRecord: {
          name: `_acme-challenge.${String(r.domain)}.`,
          type: 'CNAME',
          data: 'abcd.4.authorize.certificatemanager.goog.',
        },
      }),
    }),
    http.get(/\/certificatemanager\/v1\/projects\/[^/]+\/locations$/, () =>
      HttpResponse.json({
        locations: [
          { name: G, locationId: 'global' },
          { name: R, locationId: 'us-central1' },
        ],
      }),
    ),
    http.get(/\/dns\/v1\/projects\/[^/]+\/managedZones$/, () =>
      HttpResponse.json({
        managedZones: [{ name: 'example', dnsName: 'example.com.', visibility: 'public' }],
      }),
    ),
    http.get('*/_emu/v1/dns/query', ({ request }) => {
      const name = new URL(request.url).searchParams.get('name') ?? ''
      const data = resolved[name]
      return HttpResponse.json({
        server: '127.0.0.1:53',
        name,
        type: 'CNAME',
        rcode: data ? 'NOERROR' : 'NXDOMAIN',
        authoritative: true,
        recursionAvailable: false,
        answer: data ? [{ name, type: 'CNAME', ttl: 300, data }] : [],
        authority: [],
        additional: [],
        latencyMs: 0.2,
      })
    }),
  )
})

const sent = (method: string, path: RegExp) =>
  requests.filter((r) => r.method === method && path.test(r.path))

it('lists the Project’s DNS authorizations of every location', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs?project=${P}`)
  await user.click(await screen.findByRole('link', { name: 'DNS authorizations' }))
  const table = await screen.findByRole('table', { name: 'DNS authorizations' })
  const rows = within(table).getAllByTestId('dns-authorization')
  expect(rows).toHaveLength(2)
  expect(rows[0]).toHaveTextContent(/www.*global.*www\.example\.com.*Fixed record.*Web front end/)
  expect(rows[1]).toHaveTextContent(/api.*us-central1.*api\.example\.com.*Per-project record/)
  await user.click(within(table).getByRole('link', { name: 'api' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe(
      '/certs/locations/us-central1/dnsAuthorizations/api',
    ),
  )
})

it('shows the record a DNS authorization asks for, with a link that creates it', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/locations/global/dnsAuthorizations/www?project=${P}`)
  expect(await screen.findByRole('heading', { name: 'www' })).toBeInTheDocument()
  const record = await screen.findByLabelText('DNS record')
  expect(record).toHaveTextContent('_acme-challenge.www.example.com.')
  expect(record).toHaveTextContent('CNAME')
  expect(record).toHaveTextContent('0b5e.4.authorize.certificatemanager.goog.')
  expect(await within(record).findByText('Does not resolve (NXDOMAIN)')).toBeInTheDocument()
  expect(screen.getByLabelText('DNS authorization details')).toHaveTextContent('team=web')
  await user.click(within(record).getByRole('link', { name: 'Create the CNAME record' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/dns/zones/example/rrsets/create'),
  )
  expect(new URLSearchParams(router.state.location.search).get('data')).toBe(
    '0b5e.4.authorize.certificatemanager.goog.',
  )
})

it('says when the record resolves, and when it resolves to something else', async () => {
  resolved['_acme-challenge.www.example.com.'] = '0b5e.4.authorize.certificatemanager.goog.'
  const { unmount } = renderApp(`/certs/locations/global/dnsAuthorizations/www?project=${P}`)
  const record = await screen.findByLabelText('DNS record')
  expect(await within(record).findByText('Resolves')).toBeInTheDocument()
  expect(within(record).queryByRole('link', { name: 'Create the CNAME record' })).toBeNull()
  unmount()

  resolved['_acme-challenge.www.example.com.'] = 'stale.authorize.certificatemanager.goog.'
  renderApp(`/certs/locations/global/dnsAuthorizations/www?project=${P}`)
  expect(
    await screen.findByText('Resolves to stale.authorize.certificatemanager.goog. instead'),
  ).toBeInTheDocument()
})

it('creates a DNS authorization, as the API checks it', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/dnsAuthorizations?project=${P}`)
  await user.click(await screen.findByRole('link', { name: 'Create DNS authorization' }))
  await user.type(await screen.findByLabelText('Name'), 'Shop')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(
    await screen.findByText(/Invalid dnsAuthorizationId "Shop": must be 1-63 characters/),
  ).toBeInTheDocument()
  expect(screen.getByText('Field domain is required.')).toBeInTheDocument()

  await user.type(screen.getByLabelText('Domain'), '*.shop.example.com')
  await user.selectOptions(await screen.findByLabelText('Location'), 'us-central1')
  await user.selectOptions(screen.getByLabelText('Type'), 'FIXED_RECORD')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(
    await screen.findByText(
      'Invalid domain "*.shop.example.com": must be a fully qualified domain name without a wildcard.',
    ),
  ).toBeInTheDocument()
  expect(
    screen.getByText(
      'DNS authorizations of type FIXED_RECORD are only supported in location "global".',
    ),
  ).toBeInTheDocument()

  await user.clear(screen.getByLabelText('Name'))
  await user.type(screen.getByLabelText('Name'), 'shop')
  await user.clear(screen.getByLabelText('Domain'))
  await user.type(screen.getByLabelText('Domain'), 'Shop.Example.com.')
  await user.selectOptions(screen.getByLabelText('Type'), 'PER_PROJECT_RECORD')
  await user.type(screen.getByLabelText('Labels'), 'team=shop')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe(
      '/certs/locations/us-central1/dnsAuthorizations/shop',
    ),
  )
  const [post] = sent('POST', /us-central1\/dnsAuthorizations$/)
  expect(post!.search.get('dnsAuthorizationId')).toBe('shop')
  expect(post!.body).toEqual({
    domain: 'shop.example.com',
    type: 'PER_PROJECT_RECORD',
    labels: { team: 'shop' },
  })
  // The new authorization's page shows the record to add.
  expect(await screen.findByLabelText('DNS record')).toHaveTextContent(
    '_acme-challenge.shop.example.com.',
  )
})

it('edits a DNS authorization’s description and labels; its domain cannot change', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/locations/global/dnsAuthorizations/www?project=${P}`)
  await user.click(await screen.findByRole('link', { name: 'Edit DNS authorization' }))
  expect(await screen.findByLabelText('Description')).toHaveValue('Web front end')
  expect(screen.queryByLabelText('Domain')).toBeNull()
  expect(screen.getByText('www.example.com')).toBeInTheDocument()
  await user.clear(screen.getByLabelText('Description'))
  await user.type(screen.getByLabelText('Description'), 'Storefront')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/certs/locations/global/dnsAuthorizations/www'),
  )
  const [patch] = sent('PATCH', /dnsAuthorizations\/www$/)
  expect(patch!.search.get('updateMask')).toBe('description')
  expect(patch!.body).toEqual({ description: 'Storefront' })
  expect(await screen.findByLabelText('DNS authorization details')).toHaveTextContent('Storefront')
})

it('deletes a DNS authorization once no certificate uses it', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/locations/global/dnsAuthorizations/www?project=${P}`)
  const del = async () => {
    await user.click(await screen.findByRole('button', { name: 'Delete DNS authorization' }))
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Delete DNS authorization' }))
  }
  await del()
  expect(await screen.findByText(/is in use by/)).toBeInTheDocument()
  await user.keyboard('{Escape}')

  await user.click(screen.getByRole('link', { name: 'DNS authorizations' }))
  await user.click(await screen.findByRole('link', { name: 'api' }))
  await del()
  await waitFor(() => expect(router.state.location.pathname).toBe('/certs/dnsAuthorizations'))
  expect(sent('DELETE', /us-central1\/dnsAuthorizations\/api$/)).toHaveLength(1)
  const table = await screen.findByRole('table', { name: 'DNS authorizations' })
  await waitFor(() => expect(within(table).getAllByTestId('dns-authorization')).toHaveLength(1))
})

it('copies a DNS authorization as gcloud, REST and OpenTofu, leaving out its record', () => {
  const r = dnsAuthorizationRecipe(auths[0]!, P)
  expect(r.gcloud).toBe(
    [
      'gcloud certificate-manager dns-authorizations create www \\',
      `    --project=${P} \\`,
      '    --domain=www.example.com \\',
      "    --description='Web front end' \\",
      '    --labels=team=web',
    ].join('\n'),
  )
  expect(r.rest).toEqual({
    method: 'POST',
    path: `/certificatemanager/v1/${G}/dnsAuthorizations?dnsAuthorizationId=www`,
    body: { domain: 'www.example.com', description: 'Web front end', labels: { team: 'web' } },
  })
  expect(r.tofu).toBe(
    [
      'resource "google_certificate_manager_dns_authorization" "www" {',
      `  project     = "${P}"`,
      '  name        = "www"',
      '  domain      = "www.example.com"',
      '  description = "Web front end"',
      '  labels      = { team = "web" }',
      '}',
    ].join('\n'),
  )
})

it('copies a regional DNS authorization with its location and per-project record type', () => {
  const r = dnsAuthorizationRecipe(auths[1]!, P)
  expect(r.gcloud).toBe(
    [
      'gcloud certificate-manager dns-authorizations create api \\',
      `    --project=${P} \\`,
      '    --location=us-central1 \\',
      '    --domain=api.example.com \\',
      '    --type=PER_PROJECT_RECORD',
    ].join('\n'),
  )
  expect(r.rest).toEqual({
    method: 'POST',
    path: `/certificatemanager/v1/${R}/dnsAuthorizations?dnsAuthorizationId=api`,
    body: { domain: 'api.example.com', type: 'PER_PROJECT_RECORD' },
  })
  expect(r.tofu).toContain('  location = "us-central1"\n')
  expect(r.tofu).toContain('  type     = "PER_PROJECT_RECORD"\n')
})
