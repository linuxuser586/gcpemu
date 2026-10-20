import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, expect, it } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import type { Certificate, DnsAuthorization } from './api'
import { certificateRecipe } from './copy'

const P = 'alpha-project'
const G = `projects/${P}/locations/global`

// A small in-memory Certificate Manager REST API: certificates, DNS
// authorizations and issuance configs in two locations, operations that
// are done at once, and the requests the view sends. Cloud DNS answers
// with one public zone.
let certs: Certificate[] = []
let auths: DnsAuthorization[] = []
let requests: { method: string; path: string; search: URLSearchParams; body: unknown }[] = []

const err = (code: number, status: string, message: string) =>
  HttpResponse.json({ error: { code, status, message } }, { status: code })

const done = (response: unknown) =>
  HttpResponse.json({ name: `${G}/operations/op-1`, done: true, response })

const PEM = '-----BEGIN CERTIFICATE-----\nMIIBself\n-----END CERTIFICATE-----\n'
const RECORD = {
  name: '_acme-challenge.example.com.',
  type: 'CNAME',
  data: '1234.0.authorize.certificatemanager.goog.',
}

beforeEach(() => {
  requests = []
  fake.info.services = ['iam', 'certs', 'dns', 'lb']
  auths = [
    {
      name: `${G}/dnsAuthorizations/example`,
      domain: 'example.com',
      type: 'FIXED_RECORD',
      dnsResourceRecord: RECORD,
    },
  ]
  certs = [
    {
      name: `${G}/certificates/self`,
      description: 'Uploaded',
      labels: { team: 'web' },
      pemCertificate: PEM,
      sanDnsnames: ['self.example.com'],
      expireTime: '2099-06-01T00:00:00Z',
      createTime: '2026-10-01T10:00:00Z',
      usedBy: [{ name: `${G}/certificateMaps/main/certificateMapEntries/www` }],
    },
    {
      name: `${G}/certificates/managed`,
      scope: 'EDGE_CACHE',
      sanDnsnames: ['example.com', 'www.example.com'],
      managed: {
        domains: ['example.com', 'www.example.com'],
        dnsAuthorizations: [`${G}/dnsAuthorizations/example`],
        state: 'PROVISIONING',
        provisioningIssue: {
          reason: 'AUTHORIZATION_ISSUE',
          details: 'Authorization attempt failed for one or more domains.',
        },
        authorizationAttemptInfo: [
          {
            domain: 'example.com',
            state: 'FAILED',
            failureReason: 'CONFIG',
            details: 'The CNAME record _acme-challenge.example.com. does not resolve.',
            troubleshooting: {
              cname: { name: RECORD.name, expectedData: RECORD.data, resolvedData: ['wrong.'] },
              issues: ['CNAME_MISMATCH'],
            },
          },
          { domain: 'www.example.com', state: 'AUTHORIZED' },
        ],
      },
    },
    {
      name: `projects/${P}/locations/us-central1/certificates/lb-auth`,
      managed: {
        domains: ['shop.example.org'],
        state: 'PROVISIONING',
        authorizationAttemptInfo: [
          {
            domain: 'shop.example.org',
            state: 'AUTHORIZING',
            details: 'shop.example.org does not resolve to a forwarding rule.',
            troubleshooting: {
              ips: { resolved: ['198.51.100.9'], serving: ['203.0.113.5'] },
              issues: ['WRONG_IP_ADDRESS'],
            },
          },
        ],
      },
    },
  ]
  server.use(
    http.all(/\/certificatemanager\/v1\/(.*)$/, async ({ request }) => {
      const url = new URL(request.url)
      const text = request.method === 'GET' ? '' : await request.text()
      const body: unknown = text ? JSON.parse(text) : undefined
      const path = decodeURIComponent(url.pathname.replace(/^.*\/certificatemanager\/v1\//, ''))
      requests.push({ method: request.method, path, search: url.searchParams, body })
      if (path === `projects/${P}/locations`) {
        return HttpResponse.json({
          locations: [
            { name: `projects/${P}/locations/global`, locationId: 'global' },
            { name: `projects/${P}/locations/us-central1`, locationId: 'us-central1' },
          ],
        })
      }
      const m =
        /^projects\/[^/]+\/locations\/([^/]+)\/(certificates|dnsAuthorizations|certificateIssuanceConfigs)(?:\/([^/]+))?$/.exec(
          path,
        )
      if (!m) return err(404, 'NOT_FOUND', `bad path ${path}`)
      const [, loc, coll, id] = m
      const inLoc = (n: string) => loc === '-' || n.includes(`/locations/${loc}/`)
      if (coll === 'dnsAuthorizations') {
        return HttpResponse.json({ dnsAuthorizations: auths.filter((a) => inLoc(a.name)) })
      }
      if (coll === 'certificateIssuanceConfigs') {
        return HttpResponse.json({
          certificateIssuanceConfigs: [{ name: `${G}/certificateIssuanceConfigs/private` }].filter(
            (c) => inLoc(c.name),
          ),
        })
      }
      if (!id) {
        if (request.method === 'GET') {
          const list = certs.filter((c) => inLoc(c.name))
          return HttpResponse.json(list.length ? { certificates: list } : {})
        }
        const cid = url.searchParams.get('certificateId') ?? ''
        const name = `projects/${P}/locations/${loc}/certificates/${cid}`
        if (certs.some((c) => c.name === name)) {
          return err(409, 'ALREADY_EXISTS', `Certificate ${name} already exists.`)
        }
        const c = { ...(body as Certificate), name }
        certs.push(c)
        return done(c)
      }
      const c = certs.find((x) => x.name === `projects/${P}/locations/${loc}/certificates/${id}`)
      if (!c) return err(404, 'NOT_FOUND', `Certificate ${id} not found.`)
      if (request.method === 'DELETE') {
        certs = certs.filter((x) => x !== c)
        return done({})
      }
      if (request.method === 'PATCH') {
        Object.assign(c, body)
        return done(c)
      }
      return HttpResponse.json(c)
    }),
    http.get(/\/dns\/v1\/projects\/[^/]+\/managedZones$/, () =>
      HttpResponse.json({
        managedZones: [
          { name: 'other', dnsName: 'example.org.', visibility: 'public' },
          { name: 'example', dnsName: 'example.com.', visibility: 'public' },
          { name: 'inside', dnsName: 'example.com.', visibility: 'private' },
        ],
      }),
    ),
    http.get(/\/dns\/v1\/projects\/[^/]+\/managedZones\/example$/, () =>
      HttpResponse.json({ name: 'example', dnsName: 'example.com.', visibility: 'public' }),
    ),
    http.get(/\/dns\/v1\/projects\/[^/]+\/managedZones\/example\/rrsets$/, () =>
      HttpResponse.json({ rrsets: [] }),
    ),
  )
})

const sent = (method: string, path: RegExp) =>
  requests.filter((r) => r.method === method && path.test(r.path))

it('says how to enable Certificate Manager when it is off', async () => {
  fake.info.services = ['iam']
  renderApp(`/certs?project=${P}`)
  expect(
    await screen.findByRole('heading', { name: /Service certs is not enabled/ }),
  ).toBeInTheDocument()
})

it('lists every location’s certificates with scope, type, domains, state and expiry', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Certificates' })
  const rows = within(table).getAllByTestId('certificate')
  expect(rows).toHaveLength(3)
  expect(rows[0]).toHaveTextContent(
    /self.*global.*Default.*Self-managed.*self\.example\.com.*ACTIVE.*2099/,
  )
  expect(rows[1]).toHaveTextContent(
    /managed.*global.*Edge cache.*Google-managed \(DNS authorization\).*example\.com, www\.example\.com.*PROVISIONING/,
  )
  expect(rows[2]).toHaveTextContent(
    /lb-auth.*us-central1.*Google-managed \(load balancer authorization\).*PROVISIONING/,
  )
  expect(sent('GET', /locations\/-\/certificates$/)).toHaveLength(1)

  await user.type(screen.getByLabelText('Filter'), 'lb')
  expect(router.state.location.search).toContain('q=lb')
  expect(within(table).getAllByTestId('certificate')).toHaveLength(1)
  await user.click(within(table).getByRole('link', { name: 'lb-auth' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe(
      '/certs/locations/us-central1/certificates/lb-auth',
    ),
  )
})

it('shows a managed certificate’s issue and each domain’s authorization, linking to the fix', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/locations/global/certificates/managed?project=${P}`)
  expect(await screen.findByRole('heading', { name: 'managed' })).toBeInTheDocument()
  expect(screen.getByRole('alert')).toHaveTextContent(
    'AUTHORIZATION_ISSUE: Authorization attempt failed for one or more domains.',
  )
  const table = screen.getByRole('table', { name: 'Domain authorization' })
  const [failed, ok] = within(table).getAllByTestId('authorization')
  expect(failed).toHaveTextContent(
    /example\.com.*FAILED.*CONFIG.*does not resolve.*CNAME _acme-challenge\.example\.com\..*1234\.0\.authorize\.certificatemanager\.goog\..*wrong\./,
  )
  expect(ok).toHaveTextContent(/www\.example\.com.*AUTHORIZED/)
  expect(within(ok!).queryByRole('link')).not.toBeInTheDocument()

  // The fix is the DNS authorization's record in the public zone that holds it.
  await user.click(await within(failed!).findByRole('link', { name: 'Create the CNAME record' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/dns/zones/example/rrsets/create'),
  )
  expect(await screen.findByLabelText('DNS name')).toHaveValue('_acme-challenge')
  expect(screen.getByLabelText('Type')).toHaveValue('CNAME')
  expect(screen.getByLabelText('Data')).toHaveValue(RECORD.data)
})

it('links a load balancer authorization’s wrong IP to the forwarding rules', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/locations/us-central1/certificates/lb-auth?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Domain authorization' })
  const [row] = within(table).getAllByTestId('authorization')
  expect(row).toHaveTextContent(
    /shop\.example\.org.*AUTHORIZING.*IP.*203\.0\.113\.5.*198\.51\.100\.9/,
  )
  await user.click(within(row!).getByRole('link', { name: 'See the forwarding rules' }))
  await waitFor(() => expect(router.state.location.pathname).toBe('/lb'))
  expect(router.state.location.search).toBe(`?project=${P}`)
})

it('shows a self-managed certificate, where it is used, and never a private key', async () => {
  renderApp(`/certs/locations/global/certificates/self?project=${P}`)
  const details = await screen.findByLabelText('Certificate details')
  expect(details).toHaveTextContent('Self-managed')
  expect(details).toHaveTextContent('team=web')
  expect(details).toHaveTextContent('self.example.com')
  expect(screen.getByLabelText('Used by')).toHaveTextContent(
    `${G}/certificateMaps/main/certificateMapEntries/www`,
  )
  expect(screen.getByLabelText('PEM certificate')).toHaveTextContent('MIIBself')
  expect(screen.queryByRole('table', { name: 'Domain authorization' })).not.toBeInTheDocument()
  expect(document.body).not.toHaveTextContent(/PRIVATE KEY/i)
})

it('follows a provisioning certificate until it is ACTIVE', async () => {
  renderApp(`/certs/locations/global/certificates/managed?project=${P}`)
  expect(await screen.findByText('PROVISIONING')).toBeInTheDocument()
  const c = certs.find((x) => x.name.endsWith('/managed'))!
  c.managed = { ...c.managed, state: 'ACTIVE', provisioningIssue: undefined }
  expect(await screen.findByText('ACTIVE', {}, { timeout: 3000 })).toBeInTheDocument()
  expect(screen.queryByRole('alert')).not.toBeInTheDocument()
})

it('deletes a certificate', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/locations/global/certificates/self?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Delete certificate' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete certificate' }),
  )
  await waitFor(() => expect(router.state.location.pathname).toBe('/certs'))
  expect(sent('DELETE', /certificates\/self$/)).toHaveLength(1)
})

it('copies a self-managed certificate with the private key left for the reader', async () => {
  const user = userEvent.setup()
  renderApp(`/certs/locations/global/certificates/self?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Copy certificate as' }))
  const dialog = await screen.findByRole('dialog', { name: 'Copy certificate as' })
  const gc = within(dialog).getByLabelText('gcloud for the certificate')
  expect(gc).toHaveTextContent(
    `cat > certificate.pem <<'EOF' ${PEM.trim().replaceAll('\n', ' ')} EOF`,
  )
  expect(gc).toHaveTextContent(
    [
      '# The API does not return the private key: save it as private-key.pem.',
      'gcloud certificate-manager certificates create self \\',
      `    --project=${P} \\`,
      '    --description=Uploaded \\',
      '    --labels=team=web \\',
      '    --certificate-file=certificate.pem \\',
      '    --private-key-file=private-key.pem',
    ].join('\n'),
    { normalizeWhitespace: false },
  )

  await user.click(within(dialog).getByRole('tab', { name: 'curl' }))
  const curl = within(dialog).getByLabelText('curl for the certificate')
  expect(curl).toHaveTextContent(
    `curl '${location.origin}/certificatemanager/v1/projects/${P}/locations/global/certificates?certificateId=self' \\ -X POST`,
  )
  expect(curl).toHaveTextContent(
    '"pemPrivateKey": "PRIVATE_KEY_PEM (the API does not return the private key)"',
  )
  expect(curl).not.toHaveTextContent('usedBy')
  expect(curl).not.toHaveTextContent('sanDnsnames')

  await user.click(within(dialog).getByRole('tab', { name: 'OpenTofu' }))
  const tofu = within(dialog).getByLabelText('OpenTofu for the certificate')
  expect(tofu).toHaveTextContent('resource "google_certificate_manager_certificate" "self" {')
  expect(tofu).toHaveTextContent('pem_private_key = file("private-key.pem")')
})

it('copies Google-managed certificates of each kind', () => {
  const managed = certificateRecipe(certs[1]!, P)
  expect(managed.gcloud).toBe(
    [
      'gcloud certificate-manager certificates create managed \\',
      `    --project=${P} \\`,
      '    --scope=edge-cache \\',
      '    --domains=example.com,www.example.com \\',
      `    --dns-authorizations=${G}/dnsAuthorizations/example`,
    ].join('\n'),
  )
  expect(managed.rest).toEqual({
    method: 'POST',
    path: `/certificatemanager/v1/${G}/certificates?certificateId=managed`,
    body: {
      scope: 'EDGE_CACHE',
      managed: {
        domains: ['example.com', 'www.example.com'],
        dnsAuthorizations: [`${G}/dnsAuthorizations/example`],
      },
    },
  })
  expect(managed.tofu).toBe(
    [
      'resource "google_certificate_manager_certificate" "managed" {',
      `  project = "${P}"`,
      '  name    = "managed"',
      '  scope   = "EDGE_CACHE"',
      '  managed {',
      '    domains            = ["example.com", "www.example.com"]',
      `    dns_authorizations = ["${G}/dnsAuthorizations/example"]`,
      '  }',
      '}',
    ].join('\n'),
  )

  const lb = certificateRecipe(certs[2]!, P)
  expect(lb.gcloud).toBe(
    [
      'gcloud certificate-manager certificates create lb-auth \\',
      `    --project=${P} \\`,
      '    --location=us-central1 \\',
      '    --domains=shop.example.org',
    ].join('\n'),
  )
  expect(lb.tofu).toContain('location = "us-central1"')

  const issued = certificateRecipe(
    {
      name: `${G}/certificates/private`,
      managed: {
        domains: ['internal.example.com'],
        issuanceConfig: `${G}/certificateIssuanceConfigs/ca`,
        state: 'ACTIVE',
      },
    },
    P,
  )
  expect(issued.gcloud).toContain(`--issuance-config=${G}/certificateIssuanceConfigs/ca`)
  expect(issued.tofu).toContain(`issuance_config = "${G}/certificateIssuanceConfigs/ca"`)
})

const KEY = '-----BEGIN PRIVATE KEY-----\nMIIkey\n-----END PRIVATE KEY-----\n'

const jsonBody = async (user: ReturnType<typeof userEvent.setup>) => {
  await user.click(screen.getByRole('tab', { name: 'JSON' }))
  const body: unknown = JSON.parse(screen.getByLabelText<HTMLTextAreaElement>('Request body').value)
  await user.click(screen.getByRole('tab', { name: 'Form' }))
  return body
}

it('creates a self-managed certificate from a pasted certificate and a key file', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/create?project=${P}`)
  await user.type(await screen.findByLabelText('Name'), 'Bad_ID')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(
    await screen.findByText(
      'Invalid certificateId "Bad_ID": must be 1-63 characters long, start with a lowercase letter and contain only lowercase letters, digits and hyphens.',
    ),
  ).toBeInTheDocument()
  expect(screen.getByText('Field self_managed.pem_certificate is required.')).toBeInTheDocument()
  expect(screen.getByText('Field self_managed.pem_private_key is required.')).toBeInTheDocument()

  await user.clear(screen.getByLabelText('Name'))
  await user.type(screen.getByLabelText('Name'), 'uploaded')
  await user.type(screen.getByLabelText('Description'), 'From a file')
  await user.type(screen.getByLabelText('Labels'), 'env=dev')
  await user.click(screen.getByLabelText('PEM certificate'))
  await user.paste(PEM)
  await user.upload(
    screen.getByLabelText('Private key file'),
    new File([KEY], 'key.pem', { type: 'application/x-pem-file' }),
  )
  await waitFor(() => expect(screen.getByLabelText('PEM private key')).toHaveValue(KEY))
  await user.click(screen.getByRole('button', { name: 'Create' }))

  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/certs/locations/global/certificates/uploaded'),
  )
  const [create] = sent('POST', /global\/certificates$/)
  expect(create!.search.get('certificateId')).toBe('uploaded')
  expect(create!.body).toEqual({
    description: 'From a file',
    labels: { env: 'dev' },
    selfManaged: { pemCertificate: PEM, pemPrivateKey: KEY },
  })
})

it('creates a Google-managed certificate with DNS authorizations, as the API checks it', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/create?project=${P}`)
  await user.type(await screen.findByLabelText('Name'), 'dns-cert')
  await user.selectOptions(screen.getByLabelText('Type'), 'dns')
  await user.type(screen.getByLabelText('Domains'), 'example.com\nother.org')
  await user.click(await screen.findByRole('checkbox', { name: /example/ }))
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(
    await screen.findByText(
      'Domain "other.org" is not covered by any of the provided DNS authorizations.',
    ),
  ).toBeInTheDocument()

  await user.clear(screen.getByLabelText('Domains'))
  await user.type(screen.getByLabelText('Domains'), 'example.com\n*.example.com')
  await user.selectOptions(screen.getByLabelText('Scope'), 'EDGE_CACHE')
  expect(await jsonBody(user)).toEqual({
    scope: 'EDGE_CACHE',
    managed: {
      domains: ['example.com', '*.example.com'],
      dnsAuthorizations: [`${G}/dnsAuthorizations/example`],
    },
  })
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/certs/locations/global/certificates/dns-cert'),
  )
})

it('creates a load balancer authorized certificate, which takes no wildcards', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/create?project=${P}`)
  await user.type(await screen.findByLabelText('Name'), 'lb-cert')
  await user.selectOptions(screen.getByLabelText('Location'), 'us-central1')
  await user.selectOptions(screen.getByLabelText('Type'), 'lb')
  await user.type(screen.getByLabelText('Domains'), '*.shop.example.org')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(
    await screen.findByText('Wildcard domain "*.shop.example.org" requires DNS authorization.'),
  ).toBeInTheDocument()
  await user.clear(screen.getByLabelText('Domains'))
  await user.type(screen.getByLabelText('Domains'), 'shop.example.org')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe(
      '/certs/locations/us-central1/certificates/lb-cert',
    ),
  )
  const [create] = sent('POST', /us-central1\/certificates$/)
  expect(create!.body).toEqual({ managed: { domains: ['shop.example.org'] } })
})

it('creates a certificate through a certificate issuance config', async () => {
  const user = userEvent.setup()
  renderApp(`/certs/create?project=${P}`)
  await user.type(await screen.findByLabelText('Name'), 'private')
  await user.selectOptions(screen.getByLabelText('Type'), 'issuance')
  await user.type(screen.getByLabelText('Domains'), 'internal.example.com')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(await screen.findByText('Choose a certificate issuance config.')).toBeInTheDocument()
  await user.selectOptions(
    await screen.findByLabelText('Certificate issuance config'),
    `${G}/certificateIssuanceConfigs/private`,
  )
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() => expect(sent('POST', /global\/certificates$/)).toHaveLength(1))
  expect(sent('POST', /global\/certificates$/)[0]!.body).toEqual({
    managed: {
      domains: ['internal.example.com'],
      issuanceConfig: `${G}/certificateIssuanceConfigs/private`,
    },
  })
})

it('edits labels and description only, with an updateMask', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/locations/global/certificates/self/edit?project=${P}`)
  const labels = await screen.findByLabelText('Labels')
  expect(labels).toHaveValue('team=web')
  await user.clear(labels)
  await user.type(labels, 'team=platform')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/certs/locations/global/certificates/self'),
  )
  const [patch] = sent('PATCH', /certificates\/self$/)
  expect(patch!.search.get('updateMask')).toBe('labels')
  expect(patch!.body).toEqual({ labels: { team: 'platform' } })

  await user.click(await screen.findByRole('link', { name: 'Edit certificate' }))
  await user.clear(await screen.findByLabelText('Description'))
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(sent('PATCH', /certificates\/self$/)).toHaveLength(2))
  const second = sent('PATCH', /certificates\/self$/)[1]!
  expect(second.search.get('updateMask')).toBe('description')
  expect(second.body).toEqual({ description: '' })
})
