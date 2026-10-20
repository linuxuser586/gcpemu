import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, describe, expect, it } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import type { Address, Connection, ContainerNetwork, Network, NetworkPeering } from './api'
import { connectionRecipe, peeringRecipe } from './copy'

const P = 'alpha-project'
const link = (p: string) => `https://www.googleapis.com/compute/v1/projects/${P}/${p}`

const toOther = (over: Partial<NetworkPeering> = {}): NetworkPeering => ({
  name: 'main-to-other',
  network: link('global/networks/other'),
  state: 'ACTIVE',
  stateDetails: '[2026-10-12T09:05:00Z]: Connected.',
  autoCreateRoutes: true,
  exchangeSubnetRoutes: true,
  exportCustomRoutes: false,
  importCustomRoutes: true,
  exportSubnetRoutesWithPublicIp: true,
  importSubnetRoutesWithPublicIp: false,
  stackType: 'IPV4_ONLY',
  ...over,
})

describe('Copy as', () => {
  it('copies a peering', () => {
    const r = peeringRecipe(toOther(), 'main', P)
    expect(r.gcloud).toBe(
      [
        'gcloud compute networks peerings create main-to-other \\',
        `    --project=${P} \\`,
        '    --network=main \\',
        '    --peer-network=other \\',
        '    --import-custom-routes \\',
        '    --stack-type=IPV4_ONLY',
      ].join('\n'),
    )
    expect(r.rest).toEqual({
      method: 'POST',
      path: `/compute/v1/projects/${P}/global/networks/main/addPeering`,
      body: {
        networkPeering: {
          name: 'main-to-other',
          network: `projects/${P}/global/networks/other`,
          exchangeSubnetRoutes: true,
          exportCustomRoutes: false,
          importCustomRoutes: true,
          exportSubnetRoutesWithPublicIp: true,
          importSubnetRoutesWithPublicIp: false,
          stackType: 'IPV4_ONLY',
        },
      },
    })
    expect(r.tofu).toBe(
      [
        'resource "google_compute_network_peering" "main-to-other" {',
        '  name                                = "main-to-other"',
        `  network                             = "projects/${P}/global/networks/main"`,
        `  peer_network                        = "projects/${P}/global/networks/other"`,
        '  export_custom_routes                = false',
        '  import_custom_routes                = true',
        '  export_subnet_routes_with_public_ip = true',
        '  import_subnet_routes_with_public_ip = false',
        '  stack_type                          = "IPV4_ONLY"',
        '}',
      ].join('\n'),
    )
  })

  it('copies a peering to another Project’s network', () => {
    const r = peeringRecipe(
      toOther({
        network: 'https://www.googleapis.com/compute/v1/projects/beta/global/networks/hub',
      }),
      'main',
      P,
    )
    expect(r.gcloud).toContain('    --peer-network=hub \\\n    --peer-project=beta \\')
  })

  it('copies a peering that exchanges subnet routes with public IPs as gcloud writes it', () => {
    const r = peeringRecipe(
      toOther({ exportSubnetRoutesWithPublicIp: false, importSubnetRoutesWithPublicIp: true }),
      'main',
      P,
    )
    expect(r.gcloud).toContain('    --import-subnet-routes-with-public-ip \\\n')
    expect(r.gcloud).toContain('    --no-export-subnet-routes-with-public-ip \\\n')
  })

  it('copies a private services access connection', () => {
    const r = connectionRecipe(
      {
        network: 'projects/123456/global/networks/main',
        reservedPeeringRanges: ['psa-a', 'psa-b'],
      },
      'main',
      P,
    )
    expect(r.gcloud).toBe(
      [
        'gcloud services vpc-peerings connect \\',
        `    --project=${P} \\`,
        '    --service=servicenetworking.googleapis.com \\',
        '    --network=main \\',
        '    --ranges=psa-a,psa-b',
      ].join('\n'),
    )
    expect(r.rest).toEqual({
      method: 'POST',
      path: '/servicenetworking/v1/services/servicenetworking.googleapis.com/connections',
      body: {
        network: `projects/${P}/global/networks/main`,
        reservedPeeringRanges: ['psa-a', 'psa-b'],
      },
    })
    expect(r.tofu).toBe(
      [
        'resource "google_service_networking_connection" "main" {',
        `  network                 = "projects/${P}/global/networks/main"`,
        '  service                 = "servicenetworking.googleapis.com"',
        '  reserved_peering_ranges = ["psa-a", "psa-b"]',
        '}',
      ].join('\n'),
    )
  })
})

// A small in-memory compute and servicenetworking API: two networks, a
// peering, private services access allocations and a connection.
let networks: Network[]
let addresses: Address[]
let connection: Connection | undefined
let requests: { method: string; path: string; body: unknown }[]

const op = { name: 'op-1', status: 'DONE', selfLink: link('global/operations/op-1') }
const lro = { name: 'services/servicenetworking.googleapis.com/operations/op-2', done: true }
const err = (code: number, message: string) =>
  HttpResponse.json({ error: { code, message } }, { status: code })

/** psaRange is a VPC_PEERING allocation of network main. */
const psaRange = (name: string, address: string): Address => ({
  name,
  selfLink: link(`global/addresses/${name}`),
  address,
  prefixLength: 16,
  addressType: 'INTERNAL',
  purpose: 'VPC_PEERING',
  network: link('global/networks/main'),
  status: 'RESERVED',
})

const psaPeering: NetworkPeering = {
  name: 'servicenetworking-googleapis-com',
  network:
    'https://www.googleapis.com/compute/v1/projects/y123456-tp/global/networks/servicenetworking',
  state: 'ACTIVE',
  exportCustomRoutes: false,
  importCustomRoutes: false,
  stackType: 'IPV4_ONLY',
}

const containerNet: ContainerNetwork = {
  network: `projects/${P}/global/networks/main`,
  allocation: `projects/${P}/global/addresses/psa-a`,
  bridge: 'gcpemu-psa-main',
  declaredRange: '10.120.0.0/16',
  fallbackRange: '',
  translated: false,
  hostAddress: '',
  containers: [
    {
      name: 'sql-db',
      service: 'sql',
      role: 'postgres',
      declaredAddress: '10.120.0.3',
      address: '10.120.0.3',
    },
  ],
}

beforeEach(() => {
  requests = []
  fake.info.services = ['iam', 'compute']
  networks = [
    { name: 'main', selfLink: link('global/networks/main'), peerings: [toOther(), psaPeering] },
    { name: 'other', selfLink: link('global/networks/other') },
  ]
  addresses = [psaRange('psa-a', '10.120.0.0'), psaRange('psa-b', '10.121.0.0')]
  connection = {
    network: 'projects/123456/global/networks/main',
    peering: 'servicenetworking-googleapis-com',
    reservedPeeringRanges: ['psa-a'],
    service: 'services/servicenetworking.googleapis.com',
  }
  const record = async (request: Request) => {
    const text = await request.text()
    const body = text ? (JSON.parse(text) as Record<string, unknown>) : undefined
    const url = new URL(request.url)
    requests.push({ method: request.method, path: url.pathname + url.search, body })
    return body ?? {}
  }
  const N = '*/compute/v1/projects/:p/global/networks'
  const net = (name: unknown) => networks.find((n) => n.name === name)!
  const C = '*/servicenetworking/v1/services/servicenetworking.googleapis.com/connections'
  server.use(
    http.get(N, () => HttpResponse.json({ items: networks })),
    http.get(`${N}/:name`, ({ params }) => {
      const n = networks.find((x) => x.name === params.name)
      return n ? HttpResponse.json(n) : err(404, 'not found')
    }),
    http.post(`${N}/:name/addPeering`, async ({ request, params }) => {
      const { networkPeering } = (await record(request)) as { networkPeering: NetworkPeering }
      if (networkPeering.network.endsWith('/main')) {
        return err(
          400,
          `Invalid value for field 'networkPeering.network': '${networkPeering.network}'. A network cannot be peered with itself.`,
        )
      }
      const n = net(params.name)
      n.peerings = [...(n.peerings ?? []), { ...networkPeering, state: 'INACTIVE' }]
      return HttpResponse.json(op)
    }),
    http.patch(`${N}/:name/updatePeering`, async ({ request, params }) => {
      const { networkPeering } = (await record(request)) as { networkPeering: NetworkPeering }
      const n = net(params.name)
      n.peerings = n.peerings?.map((x) =>
        x.name === networkPeering.name ? { ...x, ...networkPeering } : x,
      )
      return HttpResponse.json(op)
    }),
    http.post(`${N}/:name/removePeering`, async ({ request, params }) => {
      const { name } = (await record(request)) as { name: string }
      const n = net(params.name)
      n.peerings = n.peerings?.filter((x) => x.name !== name)
      return HttpResponse.json(op)
    }),
    http.get('*/compute/v1/projects/:p/aggregated/subnetworks', () =>
      HttpResponse.json({ items: {} }),
    ),
    http.get('*/compute/v1/projects/:p/global/firewalls', () => HttpResponse.json({ items: [] })),
    http.get('*/compute/v1/projects/:p/global/routes', () => HttpResponse.json({ items: [] })),
    http.get('*/compute/v1/projects/:p/aggregated/addresses', () =>
      HttpResponse.json({ items: { global: { addresses } } }),
    ),
    http.get('*/compute/v1/projects/:p/global/addresses', () =>
      HttpResponse.json({ items: addresses }),
    ),
    http.get('*/_emu/v1/vpc/containerNetworks', () =>
      HttpResponse.json({ containerNetworks: [containerNet] }),
    ),
    http.get(C, () => HttpResponse.json({ connections: connection ? [connection] : [] })),
    http.post(C, async ({ request }) => {
      const body = (await record(request)) as unknown as Connection
      connection = { ...body, peering: 'servicenetworking-googleapis-com' }
      return HttpResponse.json(lro)
    }),
    http.patch(`${C}/:name`, async ({ request }) => {
      const body = (await record(request)) as unknown as Connection
      connection = { ...connection!, reservedPeeringRanges: body.reservedPeeringRanges }
      return HttpResponse.json(lro)
    }),
    http.post(`${C}/:verb`, async ({ request }) => {
      await record(request)
      connection = undefined
      return HttpResponse.json(lro)
    }),
  )
})

const writes = () => requests.filter((r) => r.method !== 'GET')

describe('peerings', () => {
  it('lists a network’s peerings, the private services access one read-only, and removes one', async () => {
    const user = userEvent.setup()
    renderApp(`/compute/global/networks/main?project=${P}`)
    const table = await screen.findByRole('table', { name: 'Peerings' })
    const rows = within(table).getAllByTestId('peering')
    expect(rows).toHaveLength(2)
    const [mine, psa] = rows as [HTMLElement, HTMLElement]
    expect(mine).toHaveTextContent('main-to-other')
    expect(mine).toHaveTextContent('ACTIVE')
    expect(mine).toHaveTextContent('Import')
    expect(within(mine).getByRole('link', { name: 'other' })).toHaveAttribute(
      'href',
      `/compute/global/networks/other?project=${P}`,
    )
    expect(within(mine).getByRole('link', { name: 'Edit peering main-to-other' })).toHaveAttribute(
      'href',
      `/compute/global/networks/main/peerings/main-to-other/edit?project=${P}`,
    )
    expect(psa).toHaveTextContent('Private services access')
    expect(psa).toHaveTextContent(`projects/y123456-tp/global/networks/servicenetworking`)
    expect(within(psa).queryByRole('button', { name: /Remove/ })).toBeNull()

    await user.click(within(mine).getByRole('button', { name: 'Copy peering main-to-other as' }))
    const copy = await screen.findByRole('dialog', { name: 'Copy peering as' })
    await user.click(within(copy).getByRole('tab', { name: 'gcloud' }))
    expect(copy).toHaveTextContent('gcloud compute networks peerings create main-to-other')
    await user.keyboard('{Escape}')

    await user.click(within(mine).getByRole('button', { name: 'Remove peering main-to-other' }))
    await user.click(
      within(await screen.findByRole('dialog')).getByRole('button', { name: 'Remove peering' }),
    )
    await waitFor(() => expect(within(table).getAllByTestId('peering')).toHaveLength(1))
    expect(writes()).toEqual([
      {
        method: 'POST',
        path: `/compute/v1/projects/${P}/global/networks/main/removePeering`,
        body: { name: 'main-to-other' },
      },
    ])
  })

  it('adds a peering, showing the API’s messages on its fields', async () => {
    const user = userEvent.setup()
    const { router } = renderApp(`/compute/global/networks/main/add-peering?project=${P}`)
    await user.type(await screen.findByLabelText('Name'), 'To_other')
    await user.click(screen.getByRole('button', { name: 'Add' }))
    expect(
      await screen.findByText(/Invalid value for field 'networkPeering.name': 'To_other'/),
    ).toBeVisible()
    expect(screen.getByText("Required field 'networkPeering.network' not specified")).toBeVisible()

    await user.clear(screen.getByLabelText('Name'))
    await user.type(screen.getByLabelText('Name'), 'to-other')
    await user.type(screen.getByLabelText('Peer network'), 'main')
    await user.click(screen.getByRole('button', { name: 'Add' }))
    expect(
      await screen.findByText(/A network cannot be peered with itself/, {
        selector: '[id$="-error"]',
      }),
    ).toBeVisible()
    await user.clear(screen.getByLabelText('Peer network'))
    await user.type(screen.getByLabelText('Peer network'), 'other')
    await user.click(screen.getByLabelText('Import custom routes'))
    await user.click(screen.getByRole('button', { name: 'Add' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/compute/global/networks/main'),
    )
    expect(writes().at(-1)).toEqual({
      method: 'POST',
      path: `/compute/v1/projects/${P}/global/networks/main/addPeering`,
      body: {
        networkPeering: {
          name: 'to-other',
          network: `projects/${P}/global/networks/other`,
          exchangeSubnetRoutes: true,
          importCustomRoutes: true,
          exportCustomRoutes: false,
          importSubnetRoutesWithPublicIp: false,
          exportSubnetRoutesWithPublicIp: true,
          stackType: 'IPV4_ONLY',
        },
      },
    })
    const rows = within(await screen.findByRole('table', { name: 'Peerings' })).getAllByTestId(
      'peering',
    )
    expect(rows.at(-1)).toHaveTextContent('INACTIVE')
  })

  it('edits a peering’s route exchange with updatePeering', async () => {
    const user = userEvent.setup()
    const { router } = renderApp(
      `/compute/global/networks/main/peerings/main-to-other/edit?project=${P}`,
    )
    expect(await screen.findByLabelText('Import custom routes')).toBeChecked()
    expect(screen.queryByLabelText('Name')).toBeNull()
    expect(screen.queryByLabelText('Peer network')).toBeNull()
    await user.click(screen.getByLabelText('Export custom routes'))
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/compute/global/networks/main'),
    )
    expect(writes()).toEqual([
      {
        method: 'PATCH',
        path: `/compute/v1/projects/${P}/global/networks/main/updatePeering`,
        body: {
          networkPeering: {
            name: 'main-to-other',
            importCustomRoutes: true,
            exportCustomRoutes: true,
            importSubnetRoutesWithPublicIp: false,
            exportSubnetRoutesWithPublicIp: true,
            stackType: 'IPV4_ONLY',
          },
        },
      },
    ])
  })
})

const CONN = `/servicenetworking/v1/services/servicenetworking.googleapis.com/connections`

describe('private services access', () => {
  it('shows a network’s allocations, their container networks and its connection', async () => {
    const user = userEvent.setup()
    renderApp(`/compute/global/networks/main?project=${P}`)
    const table = await screen.findByRole('table', { name: 'Allocated ranges' })
    const rows = within(table).getAllByTestId('allocation')
    expect(rows).toHaveLength(2)
    const [a, b] = rows as [HTMLElement, HTMLElement]
    expect(within(a).getByRole('link', { name: 'psa-a' })).toHaveAttribute(
      'href',
      `/compute/global/addresses/psa-a?project=${P}`,
    )
    expect(a).toHaveTextContent('10.120.0.0/16')
    expect(within(a).getByRole('link', { name: 'gcpemu-psa-main' })).toHaveAttribute(
      'href',
      '#container-network-gcpemu-psa-main',
    )
    expect(document.getElementById('container-network-gcpemu-psa-main')).toHaveTextContent('psa-a')
    expect(a).toHaveTextContent('1 Managed container')
    expect(a).toHaveTextContent('Connected')
    expect(b).toHaveTextContent('10.121.0.0/16')
    expect(b).toHaveTextContent('None yet')
    expect(b).toHaveTextContent('Not connected')
    expect(screen.getByRole('link', { name: 'Allocate range' })).toHaveAttribute(
      'href',
      `/compute/create-address?network=main&purpose=VPC_PEERING&project=${P}`,
    )
    const conn = screen.getByLabelText('Connection details')
    expect(conn).toHaveTextContent('servicenetworking.googleapis.com')
    expect(conn).toHaveTextContent('psa-a')

    // The container networks panel links the allocation too.
    const summary = screen.getByRole('table', { name: 'Container networks' })
    expect(within(summary).getByRole('link', { name: 'psa-a' })).toHaveAttribute(
      'href',
      `/compute/global/addresses/psa-a?project=${P}`,
    )

    await user.click(screen.getByRole('button', { name: 'Copy connection as' }))
    const copy = await screen.findByRole('dialog', { name: 'Copy connection as' })
    await user.click(within(copy).getByRole('tab', { name: 'gcloud' }))
    expect(copy).toHaveTextContent('gcloud services vpc-peerings connect')
    await user.keyboard('{Escape}')

    await user.click(screen.getByRole('button', { name: 'Delete connection' }))
    await user.click(
      within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete connection' }),
    )
    expect(await screen.findByText(/No private services access connection/)).toBeVisible()
    expect(writes()).toEqual([
      {
        method: 'POST',
        path: `${CONN}/servicenetworking-googleapis-com:deleteConnection`,
        body: { consumerNetwork: `projects/${P}/global/networks/main` },
      },
    ])
  })

  it('connects a network over its allocated ranges', async () => {
    connection = undefined
    const user = userEvent.setup()
    const { router } = renderApp(`/compute/global/networks/main/connection?project=${P}`)
    expect(await screen.findByRole('heading', { name: /Connect main/ })).toBeVisible()
    await user.click(screen.getByRole('button', { name: 'Connect' }))
    expect(
      await screen.findByText('At least one reserved peering range is required.'),
    ).toBeVisible()
    await user.click(screen.getByLabelText(/psa-b/))
    await user.click(screen.getByRole('button', { name: 'Connect' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/compute/global/networks/main'),
    )
    expect(writes()).toEqual([
      {
        method: 'POST',
        path: CONN,
        body: { network: `projects/${P}/global/networks/main`, reservedPeeringRanges: ['psa-b'] },
      },
    ])
  })

  it('changes a connection’s ranges with connections.patch', async () => {
    const user = userEvent.setup()
    const { router } = renderApp(`/compute/global/networks/main/connection?project=${P}`)
    expect(await screen.findByLabelText(/psa-a/)).toBeChecked()
    await user.click(screen.getByLabelText(/psa-b/))
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/compute/global/networks/main'),
    )
    expect(writes()).toEqual([
      {
        method: 'PATCH',
        path: `${CONN}/servicenetworking-googleapis-com?updateMask=reservedPeeringRanges&force=false`,
        body: {
          network: `projects/${P}/global/networks/main`,
          reservedPeeringRanges: ['psa-a', 'psa-b'],
        },
      },
    ])
  })
})
