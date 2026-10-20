import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, describe, expect, it } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import type { Address, Network, Subnetwork } from './api'
import { addressRecipe } from './copy'

const P = 'alpha-project'
const link = (p: string) => `https://www.googleapis.com/compute/v1/projects/${P}/${p}`

const webIp = (over: Partial<Address> = {}): Address => ({
  name: 'web-ip',
  id: '501',
  kind: 'compute#address',
  selfLink: link('regions/us-central1/addresses/web-ip'),
  creationTimestamp: '2026-10-12T09:04:00Z',
  region: link('regions/us-central1'),
  address: '34.16.0.7',
  addressType: 'EXTERNAL',
  networkTier: 'PREMIUM',
  status: 'RESERVED',
  labelFingerprint: '42WmSpB8rSM=',
  ...over,
})

describe('Copy as', () => {
  it('copies a regional external address, leaving its IP to the API', () => {
    const r = addressRecipe(webIp({ description: 'Web front', labels: { env: 'dev' } }), P)
    expect(r.gcloud).toEqual([
      [
        'gcloud compute addresses create web-ip \\',
        `    --project=${P} \\`,
        '    --region=us-central1 \\',
        "    --description='Web front' \\",
        '    --network-tier=PREMIUM',
      ].join('\n'),
      [
        'gcloud compute addresses update web-ip \\',
        `    --project=${P} \\`,
        '    --region=us-central1 \\',
        '    --update-labels=env=dev',
      ].join('\n'),
    ])
    expect(r.rest).toEqual({
      method: 'POST',
      path: `/compute/v1/projects/${P}/regions/us-central1/addresses`,
      body: {
        name: 'web-ip',
        description: 'Web front',
        addressType: 'EXTERNAL',
        networkTier: 'PREMIUM',
        labels: { env: 'dev' },
      },
    })
    expect(r.tofu).toBe(
      [
        'resource "google_compute_address" "web-ip" {',
        `  project      = "${P}"`,
        '  name         = "web-ip"',
        '  region       = "us-central1"',
        '  description  = "Web front"',
        '  address_type = "EXTERNAL"',
        '  network_tier = "PREMIUM"',
        '  labels       = { env = "dev" }',
        '}',
      ].join('\n'),
    )
  })

  it('copies a regional internal address with its subnetwork and IP', () => {
    const r = addressRecipe(
      webIp({
        name: 'db-ip',
        address: '10.40.0.9',
        addressType: 'INTERNAL',
        purpose: 'GCE_ENDPOINT',
        networkTier: undefined,
        subnetwork: link('regions/us-central1/subnetworks/apps'),
        users: [link('zones/us-central1-a/instances/db')],
        status: 'IN_USE',
      }),
      P,
    )
    expect(r.gcloud).toBe(
      [
        'gcloud compute addresses create db-ip \\',
        `    --project=${P} \\`,
        '    --region=us-central1 \\',
        '    --addresses=10.40.0.9 \\',
        '    --subnet=apps \\',
        '    --purpose=GCE_ENDPOINT',
      ].join('\n'),
    )
    expect(r.rest).toEqual({
      method: 'POST',
      path: `/compute/v1/projects/${P}/regions/us-central1/addresses`,
      body: {
        name: 'db-ip',
        address: '10.40.0.9',
        addressType: 'INTERNAL',
        purpose: 'GCE_ENDPOINT',
        subnetwork: `projects/${P}/regions/us-central1/subnetworks/apps`,
      },
    })
    expect(r.tofu).toContain(
      `  subnetwork   = "projects/${P}/regions/us-central1/subnetworks/apps"\n`,
    )
    expect(r.tofu).toContain('  address      = "10.40.0.9"\n')
  })

  it('copies a global private services access allocation', () => {
    const r = addressRecipe(
      webIp({
        name: 'psa-range',
        selfLink: link('global/addresses/psa-range'),
        region: undefined,
        address: '10.120.0.0',
        prefixLength: 16,
        addressType: 'INTERNAL',
        purpose: 'VPC_PEERING',
        networkTier: undefined,
        network: link('global/networks/main'),
      }),
      P,
    )
    expect(r.gcloud).toBe(
      [
        'gcloud compute addresses create psa-range \\',
        `    --project=${P} \\`,
        '    --global \\',
        '    --addresses=10.120.0.0 \\',
        '    --prefix-length=16 \\',
        '    --network=main \\',
        '    --purpose=VPC_PEERING',
      ].join('\n'),
    )
    expect(r.rest).toMatchObject({
      path: `/compute/v1/projects/${P}/global/addresses`,
      body: { network: `projects/${P}/global/networks/main`, prefixLength: 16 },
    })
    expect(r.tofu).toBe(
      [
        'resource "google_compute_global_address" "psa-range" {',
        `  project       = "${P}"`,
        '  name          = "psa-range"',
        '  address       = "10.120.0.0"',
        '  address_type  = "INTERNAL"',
        '  purpose       = "VPC_PEERING"',
        '  prefix_length = 16',
        `  network       = "projects/${P}/global/networks/main"`,
        '}',
      ].join('\n'),
    )
  })
})

// A small in-memory compute API: networks, subnetworks and addresses.
let addresses: Address[]
let requests: { method: string; path: string; body: unknown }[]

const op = { name: 'op-1', status: 'DONE', selfLink: link('global/operations/op-1') }
const err = (code: number, message: string) =>
  HttpResponse.json({ error: { code, message } }, { status: code })

/** scopeOf is "global" or a region of an address's selfLink. */
const scopeOf = (a: Address) => /\/regions\/([^/]+)\//.exec(a.selfLink ?? '')?.[1] ?? 'global'

beforeEach(() => {
  requests = []
  fake.info.services = ['iam', 'compute']
  const networks: Network[] = [{ name: 'main', selfLink: link('global/networks/main') }]
  const subnets: Subnetwork[] = [
    {
      name: 'apps',
      selfLink: link('regions/us-central1/subnetworks/apps'),
      region: link('regions/us-central1'),
      network: link('global/networks/main'),
      ipCidrRange: '10.40.0.0/24',
    },
  ]
  addresses = [
    webIp({
      status: 'IN_USE',
      users: [link('regions/us-central1/forwardingRules/web-fr')],
    }),
    webIp({
      name: 'nat-ip',
      selfLink: link('regions/us-central1/addresses/nat-ip'),
      address: '34.16.0.8',
      status: 'IN_USE',
      users: [link('regions/us-central1/routers/edge')],
    }),
    webIp({
      name: 'db-ip',
      selfLink: link('regions/us-central1/addresses/db-ip'),
      address: '10.40.0.9',
      addressType: 'INTERNAL',
      purpose: 'GCE_ENDPOINT',
      networkTier: undefined,
      subnetwork: link('regions/us-central1/subnetworks/apps'),
      status: 'IN_USE',
      users: [link('zones/us-central1-a/instances/db')],
    }),
    webIp({
      name: 'psa-range',
      selfLink: link('global/addresses/psa-range'),
      region: undefined,
      address: '10.120.0.0',
      prefixLength: 16,
      addressType: 'INTERNAL',
      purpose: 'VPC_PEERING',
      networkTier: undefined,
      network: link('global/networks/main'),
    }),
  ]
  const record = async (request: Request) => {
    const text = await request.text()
    const body = text ? (JSON.parse(text) as Record<string, unknown>) : undefined
    requests.push({ method: request.method, path: new URL(request.url).pathname, body })
    return body ?? {}
  }
  const find = (scope: string, name: unknown) =>
    addresses.find((a) => scopeOf(a) === scope && a.name === name)
  const insert = async (request: Request, scope: string) => {
    const body = (await record(request)) as Address
    if (body.addressType === 'INTERNAL' && body.address === '10.40.0.1') {
      return err(
        400,
        "Invalid value for field 'resource.address': '10.40.0.1'. The specified IP address is not within the subnetwork range or is reserved.",
      )
    }
    const where = scope === 'global' ? 'global' : `regions/${scope}`
    addresses.push({
      ...body,
      selfLink: link(`${where}/addresses/${body.name}`),
      address: body.address ?? '34.16.0.20',
      status: 'RESERVED',
      labelFingerprint: 'new=',
    })
    return HttpResponse.json(op)
  }
  const A = '*/compute/v1/projects/:p/regions/:region/addresses'
  const G = '*/compute/v1/projects/:p/global/addresses'
  server.use(
    http.get('*/compute/v1/projects/:p/global/networks', () =>
      HttpResponse.json({ items: networks }),
    ),
    http.get('*/compute/v1/projects/:p/aggregated/subnetworks', () =>
      HttpResponse.json({ items: { 'regions/us-central1': { subnetworks: subnets } } }),
    ),
    // As on GCP, the aggregated list has the global addresses too.
    http.get('*/compute/v1/projects/:p/aggregated/addresses', () => {
      const items: Record<string, { addresses: Address[] }> = {}
      for (const a of addresses) {
        const s = scopeOf(a)
        const key = s === 'global' ? 'global' : `regions/${s}`
        ;(items[key] ??= { addresses: [] }).addresses.push(a)
      }
      return HttpResponse.json({ items })
    }),
    http.get(G, () =>
      HttpResponse.json({ items: addresses.filter((a) => scopeOf(a) === 'global') }),
    ),
    http.get(A, ({ params }) =>
      HttpResponse.json({ items: addresses.filter((a) => scopeOf(a) === params.region) }),
    ),
    http.post(G, ({ request }) => insert(request, 'global')),
    http.post(A, ({ request, params }) => insert(request, params.region as string)),
    http.get(`${G}/:name`, ({ params }) => {
      const a = find('global', params.name)
      return a ? HttpResponse.json(a) : err(404, 'not found')
    }),
    http.get(`${A}/:name`, ({ params }) => {
      const a = find(params.region as string, params.name)
      return a ? HttpResponse.json(a) : err(404, 'not found')
    }),
    http.post(`${A}/:name/setLabels`, async ({ request, params }) => {
      const body = (await record(request)) as { labels: Record<string, string> }
      const a = find(params.region as string, params.name)!
      a.labels = body.labels
      a.labelFingerprint = 'next='
      return HttpResponse.json(op)
    }),
    http.delete(`${A}/:name`, async ({ request, params }) => {
      await record(request)
      const a = find(params.region as string, params.name)!
      if (a.users?.length) {
        return err(
          400,
          `The address resource '${a.selfLink}' is already being used by '${a.users[0]}'`,
        )
      }
      addresses = addresses.filter((x) => x !== a)
      return HttpResponse.json(op)
    }),
  )
})

const writes = () => requests.filter((r) => r.method !== 'GET')

describe('addresses', () => {
  it('lists regional and global addresses, linking their users to their views', async () => {
    renderApp(`/compute/addresses?project=${P}`)
    const table = await screen.findByRole('table', { name: 'IP addresses' })
    const rows = within(table).getAllByTestId('address')
    expect(rows.map((r) => within(r).getAllByRole('cell')[0]!.textContent)).toEqual([
      'db-ip',
      'nat-ip',
      'psa-range',
      'web-ip',
    ])
    const [db, nat, psa, web] = rows as [HTMLElement, HTMLElement, HTMLElement, HTMLElement]
    expect(web).toHaveTextContent('us-central1')
    expect(web).toHaveTextContent('34.16.0.7')
    expect(web).toHaveTextContent('EXTERNAL')
    expect(web).toHaveTextContent('IN_USE')
    expect(within(web).getByRole('link', { name: 'web-ip' })).toHaveAttribute(
      'href',
      `/compute/regions/us-central1/addresses/web-ip?project=${P}`,
    )
    expect(within(web).getByRole('link', { name: 'web-fr' })).toHaveAttribute(
      'href',
      `/lb/regions/us-central1/forwardingRules/web-fr?project=${P}`,
    )
    expect(within(nat).getByRole('link', { name: 'edge' })).toHaveAttribute(
      'href',
      `/nat/regions/us-central1/routers/edge?project=${P}`,
    )
    expect(db).toHaveTextContent('INTERNAL')
    expect(db).toHaveTextContent('GCE_ENDPOINT')
    expect(db).toHaveTextContent(`projects/${P}/zones/us-central1-a/instances/db`)
    expect(within(db).queryAllByRole('link')).toHaveLength(1)
    expect(psa).toHaveTextContent('Global')
    expect(psa).toHaveTextContent('10.120.0.0/16')
    expect(psa).toHaveTextContent('VPC_PEERING')
    expect(screen.getByRole('link', { name: 'IP addresses' })).toHaveAttribute(
      'aria-current',
      'page',
    )
  })

  it('shows an address in use, copies it, sets its labels and is refused its release', async () => {
    const user = userEvent.setup()
    renderApp(`/compute/regions/us-central1/addresses/web-ip?project=${P}`)
    const details = await screen.findByLabelText('Address details')
    expect(details).toHaveTextContent('34.16.0.7')
    expect(details).toHaveTextContent('IN_USE')
    expect(within(details).getByRole('link', { name: 'web-fr' })).toHaveAttribute(
      'href',
      `/lb/regions/us-central1/forwardingRules/web-fr?project=${P}`,
    )

    await user.click(screen.getByRole('button', { name: 'Copy address as' }))
    const copy = await screen.findByRole('dialog', { name: 'Copy address as' })
    await user.click(within(copy).getByRole('tab', { name: 'gcloud' }))
    expect(within(copy).getByLabelText('gcloud for the address')).toHaveTextContent(
      'gcloud compute addresses create web-ip',
    )
    await user.keyboard('{Escape}')

    await user.click(screen.getByRole('button', { name: 'Set labels' }))
    const dialog = await screen.findByRole('dialog', { name: 'Labels of web-ip' })
    await user.type(within(dialog).getByLabelText('Labels'), 'Env=dev')
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))
    expect(await within(dialog).findByText(/Invalid label key "Env"/)).toBeVisible()
    await user.clear(within(dialog).getByLabelText('Labels'))
    await user.type(within(dialog).getByLabelText('Labels'), 'env=dev')
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(details).toHaveTextContent('env=dev'))

    await user.click(screen.getByRole('button', { name: 'Release address' }))
    await user.click(
      within(await screen.findByRole('dialog')).getByRole('button', { name: 'Release address' }),
    )
    expect(await screen.findByText(/is already being used by/)).toBeVisible()
    expect(writes()).toEqual([
      {
        method: 'POST',
        path: `/compute/v1/projects/${P}/regions/us-central1/addresses/web-ip/setLabels`,
        body: { labels: { env: 'dev' }, labelFingerprint: '42WmSpB8rSM=' },
      },
      {
        method: 'DELETE',
        path: `/compute/v1/projects/${P}/regions/us-central1/addresses/web-ip`,
        body: undefined,
      },
    ])
  })

  it('shows a global allocation and releases it', async () => {
    const user = userEvent.setup()
    const { router } = renderApp(`/compute/global/addresses/psa-range?project=${P}`)
    const details = await screen.findByLabelText('Address details')
    expect(details).toHaveTextContent('10.120.0.0/16')
    expect(details).toHaveTextContent('Global')
    expect(within(details).getByRole('link', { name: 'main' })).toHaveAttribute(
      'href',
      `/compute/global/networks/main?project=${P}`,
    )
    server.use(
      http.delete('*/compute/v1/projects/:p/global/addresses/:name', ({ request }) => {
        requests.push({ method: 'DELETE', path: new URL(request.url).pathname, body: undefined })
        addresses = addresses.filter((a) => a.name !== 'psa-range')
        return HttpResponse.json(op)
      }),
    )
    await user.click(screen.getByRole('button', { name: 'Release address' }))
    await user.click(
      within(await screen.findByRole('dialog')).getByRole('button', { name: 'Release address' }),
    )
    await waitFor(() => expect(router.state.location.pathname).toBe('/compute/addresses'))
    expect(writes()).toEqual([
      {
        method: 'DELETE',
        path: `/compute/v1/projects/${P}/global/addresses/psa-range`,
        body: undefined,
      },
    ])
  })

  it('reserves a regional external address', async () => {
    const user = userEvent.setup()
    const { router } = renderApp(`/compute/create-address?project=${P}`)
    await user.type(await screen.findByLabelText('Name'), 'Edge_ip')
    await user.click(screen.getByRole('button', { name: 'Reserve' }))
    expect(
      await screen.findByText(/Invalid value for field 'resource.name': 'Edge_ip'/),
    ).toBeVisible()
    await user.clear(screen.getByLabelText('Name'))
    await user.type(screen.getByLabelText('Name'), 'edge-ip')
    await user.selectOptions(screen.getByLabelText('Network tier'), 'STANDARD')
    await user.type(screen.getByLabelText('Labels'), 'team=web')
    await user.click(screen.getByRole('button', { name: 'Reserve' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/compute/regions/us-central1/addresses/edge-ip'),
    )
    expect(writes()).toEqual([
      {
        method: 'POST',
        path: `/compute/v1/projects/${P}/regions/us-central1/addresses`,
        body: {
          name: 'edge-ip',
          addressType: 'EXTERNAL',
          networkTier: 'STANDARD',
          labels: { team: 'web' },
        },
      },
    ])
  })

  it('reserves an internal address in a subnetwork, showing the API’s message', async () => {
    const user = userEvent.setup()
    const { router } = renderApp(`/compute/create-address?project=${P}`)
    await user.type(await screen.findByLabelText('Name'), 'cache-ip')
    await user.selectOptions(screen.getByLabelText('Type'), 'INTERNAL')
    expect(screen.queryByLabelText('Network tier')).toBeNull()
    await user.type(screen.getByLabelText('Subnetwork'), 'apps')
    await user.type(screen.getByLabelText('IP address'), '10.40.0.300')
    await user.click(screen.getByRole('button', { name: 'Reserve' }))
    expect(
      await screen.findByText(
        "Invalid value for field 'resource.address': '10.40.0.300'. Must be a valid IPv4 address.",
      ),
    ).toBeVisible()
    await user.clear(screen.getByLabelText('IP address'))
    await user.type(screen.getByLabelText('IP address'), '10.40.0.1')
    await user.click(screen.getByRole('button', { name: 'Reserve' }))
    expect(
      await screen.findByText(/The specified IP address is not within the subnetwork range/, {
        selector: '[id$="-error"]',
      }),
    ).toBeVisible()
    await user.clear(screen.getByLabelText('IP address'))
    await user.type(screen.getByLabelText('IP address'), '10.40.0.10')
    await user.click(screen.getByRole('button', { name: 'Reserve' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe(
        '/compute/regions/us-central1/addresses/cache-ip',
      ),
    )
    expect(writes().at(-1)!.body).toEqual({
      name: 'cache-ip',
      addressType: 'INTERNAL',
      address: '10.40.0.10',
      subnetwork: `projects/${P}/regions/us-central1/subnetworks/apps`,
    })
  })

  it('reserves a global range for private services access', async () => {
    const user = userEvent.setup()
    const { router } = renderApp(`/compute/create-address?project=${P}`)
    await user.type(await screen.findByLabelText('Name'), 'psa-two')
    await user.selectOptions(screen.getByLabelText('Scope'), 'global')
    expect(screen.queryByLabelText('Region')).toBeNull()
    await user.selectOptions(screen.getByLabelText('Type'), 'INTERNAL')
    await user.type(screen.getByLabelText('Prefix length'), '30')
    await user.click(screen.getByRole('button', { name: 'Reserve' }))
    expect(await screen.findByText("Required field 'resource.network' not specified")).toBeVisible()
    expect(
      screen.getByText(
        "Invalid value for field 'resource.prefixLength': '30'. Must be between 8 and 29.",
      ),
    ).toBeVisible()
    await user.type(screen.getByLabelText('Network'), 'main')
    await user.clear(screen.getByLabelText('Prefix length'))
    await user.type(screen.getByLabelText('Prefix length'), '20')
    await user.click(screen.getByRole('button', { name: 'Reserve' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/compute/global/addresses/psa-two'),
    )
    expect(writes().at(-1)).toEqual({
      method: 'POST',
      path: `/compute/v1/projects/${P}/global/addresses`,
      body: {
        name: 'psa-two',
        addressType: 'INTERNAL',
        purpose: 'VPC_PEERING',
        network: `projects/${P}/global/networks/main`,
        prefixLength: 20,
      },
    })
  })
})
