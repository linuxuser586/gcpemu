import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, describe, expect, it } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import type { Network, Subnetwork } from './api'
import { networkRecipe, subnetRecipe } from './copy'

const P = 'alpha-project'
const link = (p: string) => `https://www.googleapis.com/compute/v1/projects/${P}/${p}`

const vpc = (over: Partial<Network> = {}): Network => ({
  name: 'vpc',
  id: '101',
  kind: 'compute#network',
  selfLink: link('global/networks/vpc'),
  creationTimestamp: '2026-10-12T09:00:00Z',
  autoCreateSubnetworks: false,
  mtu: 1460,
  routingConfig: { routingMode: 'REGIONAL' },
  subnetworks: [link('regions/us-central1/subnetworks/nodes')],
  ...over,
})

const nodes = (over: Partial<Subnetwork> = {}): Subnetwork => ({
  name: 'nodes',
  id: '201',
  kind: 'compute#subnetwork',
  selfLink: link('regions/us-central1/subnetworks/nodes'),
  creationTimestamp: '2026-10-12T09:01:00Z',
  region: link('regions/us-central1'),
  network: link('global/networks/vpc'),
  ipCidrRange: '10.0.0.0/24',
  gatewayAddress: '10.0.0.1',
  fingerprint: 'fp-nodes',
  purpose: 'PRIVATE',
  stackType: 'IPV4_ONLY',
  state: 'READY',
  ...over,
})

describe('Copy as', () => {
  it('copies a custom mode network', () => {
    const r = networkRecipe(
      vpc({ description: 'Main', mtu: 1500, routingConfig: { routingMode: 'GLOBAL' } }),
      P,
    )
    expect(r.gcloud).toBe(
      [
        'gcloud compute networks create vpc \\',
        `    --project=${P} \\`,
        '    --subnet-mode=custom \\',
        '    --description=Main \\',
        '    --mtu=1500 \\',
        '    --bgp-routing-mode=global',
      ].join('\n'),
    )
    expect(r.rest).toEqual({
      method: 'POST',
      path: `/compute/v1/projects/${P}/global/networks`,
      body: {
        name: 'vpc',
        description: 'Main',
        autoCreateSubnetworks: false,
        mtu: 1500,
        routingConfig: { routingMode: 'GLOBAL' },
      },
    })
    expect(r.tofu).toBe(
      [
        'resource "google_compute_network" "vpc" {',
        `  project                 = "${P}"`,
        '  name                    = "vpc"',
        '  description             = "Main"',
        '  auto_create_subnetworks = false',
        '  mtu                     = 1500',
        '  routing_mode            = "GLOBAL"',
        '}',
      ].join('\n'),
    )
  })

  it('copies an auto mode network', () => {
    const r = networkRecipe(vpc({ autoCreateSubnetworks: true, mtu: undefined }), P)
    expect(r.gcloud).toContain('--subnet-mode=auto')
    expect(r.tofu).toContain('auto_create_subnetworks = true')
  })

  it('copies a subnetwork with secondary ranges and Private Google Access', () => {
    const r = subnetRecipe(
      nodes({
        description: 'GKE nodes',
        privateIpGoogleAccess: true,
        secondaryIpRanges: [
          { rangeName: 'pods', ipCidrRange: '10.4.0.0/14' },
          { rangeName: 'svc', ipCidrRange: '10.8.0.0/20' },
        ],
      }),
      P,
    )
    expect(r.gcloud).toBe(
      [
        'gcloud compute networks subnets create nodes \\',
        `    --project=${P} \\`,
        '    --region=us-central1 \\',
        '    --network=vpc \\',
        '    --range=10.0.0.0/24 \\',
        "    --description='GKE nodes' \\",
        '    --secondary-range=pods=10.4.0.0/14,svc=10.8.0.0/20 \\',
        '    --enable-private-ip-google-access',
      ].join('\n'),
    )
    expect(r.rest).toEqual({
      method: 'POST',
      path: `/compute/v1/projects/${P}/regions/us-central1/subnetworks`,
      body: {
        name: 'nodes',
        network: `projects/${P}/global/networks/vpc`,
        ipCidrRange: '10.0.0.0/24',
        description: 'GKE nodes',
        privateIpGoogleAccess: true,
        secondaryIpRanges: [
          { rangeName: 'pods', ipCidrRange: '10.4.0.0/14' },
          { rangeName: 'svc', ipCidrRange: '10.8.0.0/20' },
        ],
        purpose: 'PRIVATE',
        stackType: 'IPV4_ONLY',
      },
    })
    expect(r.tofu).toBe(
      [
        'resource "google_compute_subnetwork" "nodes" {',
        `  project                  = "${P}"`,
        '  name                     = "nodes"',
        '  region                   = "us-central1"',
        `  network                  = "projects/${P}/global/networks/vpc"`,
        '  ip_cidr_range            = "10.0.0.0/24"',
        '  description              = "GKE nodes"',
        '  private_ip_google_access = true',
        '  secondary_ip_range {',
        '    range_name    = "pods"',
        '    ip_cidr_range = "10.4.0.0/14"',
        '  }',
        '  secondary_ip_range {',
        '    range_name    = "svc"',
        '    ip_cidr_range = "10.8.0.0/20"',
        '  }',
        '}',
      ].join('\n'),
    )
  })
})

// A small in-memory compute API: networks and subnetworks, their custom
// methods, and the Offered ports of the admin API.
let networks: Network[]
let subnets: Subnetwork[]
let requests: { method: string; path: string; body: unknown }[]

const op = { name: 'op-1', status: 'DONE', selfLink: link('global/operations/op-1') }
const err = (code: number, message: string) =>
  HttpResponse.json({ error: { code, message } }, { status: code })
const lastOf = (s?: string) => s?.split('/').pop() ?? ''

beforeEach(() => {
  requests = []
  fake.info.services = ['iam', 'compute']
  networks = [
    vpc(),
    vpc({
      name: 'default',
      id: '102',
      selfLink: link('global/networks/default'),
      autoCreateSubnetworks: true,
      subnetworks: [link('regions/europe-west1/subnetworks/default')],
    }),
  ]
  subnets = [
    nodes({ secondaryIpRanges: [{ rangeName: 'pods', ipCidrRange: '10.4.0.0/14' }] }),
    nodes({
      name: 'default',
      selfLink: link('regions/europe-west1/subnetworks/default'),
      region: link('regions/europe-west1'),
      network: link('global/networks/default'),
      ipCidrRange: '10.132.0.0/20',
      privateIpGoogleAccess: true,
    }),
  ]
  const record = async (request: Request) => {
    const text = await request.text()
    const body = text ? (JSON.parse(text) as Record<string, unknown>) : undefined
    requests.push({ method: request.method, path: new URL(request.url).pathname, body })
    return body ?? {}
  }
  const N = '*/compute/v1/projects/:p/global/networks'
  const S = '*/compute/v1/projects/:p/regions/:region/subnetworks'
  const net = (name: unknown) => networks.find((n) => n.name === name)
  const sub = (region: unknown, name: unknown) =>
    subnets.find((s) => s.name === name && s.region === link(`regions/${region as string}`))
  server.use(
    http.get(N, () => HttpResponse.json({ items: networks })),
    http.post(N, async ({ request }) => {
      const body = (await record(request)) as Network
      networks.push({ ...body, selfLink: link(`global/networks/${body.name}`) })
      return HttpResponse.json(op)
    }),
    http.get(`${N}/:name`, ({ params }) => {
      const n = net(params.name)
      return n ? HttpResponse.json(n) : err(404, 'not found')
    }),
    http.patch(`${N}/:name`, async ({ request, params }) => {
      Object.assign(net(params.name)!, await record(request))
      return HttpResponse.json(op)
    }),
    http.post(`${N}/:name/switchToCustomMode`, async ({ request, params }) => {
      await record(request)
      net(params.name)!.autoCreateSubnetworks = false
      return HttpResponse.json(op)
    }),
    http.delete(`${N}/:name`, async ({ request, params }) => {
      await record(request)
      networks = networks.filter((n) => n.name !== params.name)
      return HttpResponse.json(op)
    }),
    http.get('*/compute/v1/projects/:p/aggregated/subnetworks', () =>
      HttpResponse.json({
        items: Object.fromEntries(
          ['us-central1', 'europe-west1'].map((reg) => [
            `regions/${reg}`,
            { subnetworks: subnets.filter((s) => s.region === link(`regions/${reg}`)) },
          ]),
        ),
      }),
    ),
    http.post(S, async ({ request, params }) => {
      const body = (await record(request)) as Subnetwork
      if (body.ipCidrRange === '10.0.0.0/25') {
        return err(
          400,
          "Invalid IPCidrRange: 10.0.0.0/25 conflicts with existing subnetwork 'nodes' in region 'us-central1'.",
        )
      }
      subnets.push(
        nodes({
          ...body,
          region: link(`regions/${params.region as string}`),
          selfLink: link(`regions/${params.region as string}/subnetworks/${body.name}`),
          network: link(`global/networks/${lastOf(body.network)}`),
        }),
      )
      return HttpResponse.json(op)
    }),
    http.get(`${S}/:name`, ({ params }) => {
      const s = sub(params.region, params.name)
      return s ? HttpResponse.json(s) : err(404, 'not found')
    }),
    http.patch(`${S}/:name`, async ({ request, params }) => {
      const body = await record(request)
      Object.assign(sub(params.region, params.name)!, body, { fingerprint: 'fp-2' })
      return HttpResponse.json(op)
    }),
    http.post(`${S}/:name/expandIpCidrRange`, async ({ request, params }) => {
      const body = (await record(request)) as { ipCidrRange: string }
      const s = sub(params.region, params.name)!
      if (body.ipCidrRange === '10.0.0.0/25') {
        return err(
          400,
          `Invalid value for field 'ipCidrRange': '10.0.0.0/25'. The new range must contain the existing range ${s.ipCidrRange} and be larger.`,
        )
      }
      s.ipCidrRange = body.ipCidrRange
      return HttpResponse.json(op)
    }),
    http.post(`${S}/:name/setPrivateIpGoogleAccess`, async ({ request, params }) => {
      const body = (await record(request)) as { privateIpGoogleAccess: boolean }
      sub(params.region, params.name)!.privateIpGoogleAccess = body.privateIpGoogleAccess
      return HttpResponse.json(op)
    }),
    http.delete(`${S}/:name`, async ({ request, params }) => {
      await record(request)
      subnets = subnets.filter((s) => s !== sub(params.region, params.name))
      return HttpResponse.json(op)
    }),
    http.get('*/_emu/v1/host/offers', () =>
      HttpResponse.json({
        subnetworks: [
          {
            subnetwork: `projects/${P}/regions/us-central1/subnetworks/nodes`,
            hostAddress: '10.0.0.254',
            offers: [{ port: 8080, protocol: 'tcp' }],
          },
        ],
      }),
    ),
  )
})

const writes = () => requests.filter((r) => r.method !== 'GET')

it('asks for the Service and a Project', async () => {
  fake.info.services = ['iam']
  const { unmount } = renderApp(`/compute?project=${P}`)
  expect(
    await screen.findByRole('heading', { name: /Service compute is not enabled/ }),
  ).toBeVisible()
  unmount()
  fake.info.services = ['iam', 'compute']
  renderApp('/compute')
  expect(await screen.findByText(/Choose a Project with the Project switcher/)).toBeVisible()
})

describe('networks', () => {
  it('lists networks with their mode and subnetworks', async () => {
    renderApp(`/compute?project=${P}`)
    const table = await screen.findByRole('table', { name: 'Networks' })
    const rows = within(table).getAllByTestId('network')
    expect(rows).toHaveLength(2)
    expect(rows[0]).toHaveTextContent('default')
    expect(rows[0]).toHaveTextContent('Auto')
    expect(rows[1]).toHaveTextContent('Custom')
    expect(rows[1]).toHaveTextContent('1460')
    expect(within(rows[1]!).getByRole('link', { name: 'vpc' })).toHaveAttribute(
      'href',
      `/compute/global/networks/vpc?project=${P}`,
    )
    await waitFor(() => expect(rows[1]).toHaveTextContent('nodes'))
  })

  it('creates a custom mode network, showing the API’s messages', async () => {
    const user = userEvent.setup()
    const { router } = renderApp(`/compute/create-network?project=${P}`)
    await user.type(await screen.findByLabelText('Name'), 'Main_1')
    await user.type(screen.getByLabelText('MTU'), '100')
    await user.click(screen.getByRole('button', { name: 'Create' }))
    expect(
      await screen.findByText(/Invalid value for field 'resource.name': 'Main_1'/),
    ).toBeVisible()
    expect(
      screen.getByText(
        "Invalid value for field 'resource.mtu': '100'. Must be between 1300 and 8896.",
      ),
    ).toBeVisible()
    await user.clear(screen.getByLabelText('Name'))
    await user.type(screen.getByLabelText('Name'), 'main')
    await user.clear(screen.getByLabelText('MTU'))
    await user.selectOptions(screen.getByLabelText('Subnet creation mode'), 'custom')
    await user.selectOptions(screen.getByLabelText('Dynamic routing mode'), 'GLOBAL')
    await user.click(screen.getByRole('button', { name: 'Create' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/compute/global/networks/main'),
    )
    expect(writes()).toEqual([
      {
        method: 'POST',
        path: `/compute/v1/projects/${P}/global/networks`,
        body: {
          name: 'main',
          autoCreateSubnetworks: false,
          routingConfig: { routingMode: 'GLOBAL' },
        },
      },
    ])
  })

  it('lists a network’s subnetworks with links to them', async () => {
    renderApp(`/compute/global/networks/vpc?project=${P}`)
    const table = await screen.findByRole('table', { name: 'Subnetworks' })
    const row = within(table).getByTestId('subnetwork')
    expect(row).toHaveTextContent('us-central1')
    expect(row).toHaveTextContent('10.0.0.0/24')
    expect(row).toHaveTextContent('pods 10.4.0.0/14')
    expect(within(row).getByRole('link', { name: 'nodes' })).toHaveAttribute(
      'href',
      `/compute/regions/us-central1/subnetworks/nodes?project=${P}`,
    )
    expect(screen.getByRole('link', { name: 'Add subnetwork' })).toHaveAttribute(
      'href',
      `/compute/create-subnetwork?network=vpc&project=${P}`,
    )
  })

  it('switches an auto mode network to custom mode and deletes it', async () => {
    const user = userEvent.setup()
    subnets = subnets.filter((s) => s.name !== 'default')
    const { router } = renderApp(`/compute/global/networks/default?project=${P}`)
    expect(await screen.findByLabelText('Network details')).toHaveTextContent('Auto')
    expect(screen.getByText(/No subnetworks in this network/)).toBeVisible()

    await user.click(screen.getByRole('button', { name: 'Switch to custom mode' }))
    const dialog = await screen.findByRole('dialog')
    expect(dialog).toHaveTextContent('cannot be switched back')
    await user.click(within(dialog).getByRole('button', { name: 'Switch to custom mode' }))
    await waitFor(() =>
      expect(screen.getByLabelText('Network details')).toHaveTextContent('Custom'),
    )
    expect(screen.queryByRole('button', { name: 'Switch to custom mode' })).toBeNull()

    await user.click(screen.getByRole('button', { name: 'Delete network' }))
    await user.click(
      within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete network' }),
    )
    await waitFor(() => expect(router.state.location.pathname).toBe('/compute'))
    expect(writes()).toEqual([
      {
        method: 'POST',
        path: `/compute/v1/projects/${P}/global/networks/default/switchToCustomMode`,
        body: undefined,
      },
      {
        method: 'DELETE',
        path: `/compute/v1/projects/${P}/global/networks/default`,
        body: undefined,
      },
    ])
  })

  it('edits a network’s description and MTU as a merge patch', async () => {
    const user = userEvent.setup()
    renderApp(`/compute/global/networks/vpc/edit?project=${P}`)
    await user.type(await screen.findByLabelText('Description'), 'Main')
    await user.clear(screen.getByLabelText('MTU'))
    await user.type(screen.getByLabelText('MTU'), '1500')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(writes()).toHaveLength(1))
    expect(writes()[0]).toEqual({
      method: 'PATCH',
      path: `/compute/v1/projects/${P}/global/networks/vpc`,
      body: { description: 'Main', mtu: 1500 },
    })
  })
})

describe('subnetworks', () => {
  it('lists subnetworks of every region', async () => {
    renderApp(`/compute/subnetworks?project=${P}`)
    const table = await screen.findByRole('table', { name: 'Subnetworks' })
    const rows = within(table).getAllByTestId('subnetwork')
    expect(rows).toHaveLength(2)
    expect(rows[0]).toHaveTextContent('europe-west1')
    expect(rows[0]).toHaveTextContent('10.132.0.0/20')
    expect(rows[0]).toHaveTextContent('On')
    expect(within(rows[0]!).getAllByRole('link', { name: 'default' })[1]).toHaveAttribute(
      'href',
      `/compute/global/networks/default?project=${P}`,
    )
    expect(rows[1]).toHaveTextContent('us-central1')
    expect(rows[1]).toHaveTextContent('Off')
    expect(screen.getByRole('link', { name: 'Subnetworks' })).toHaveAttribute(
      'aria-current',
      'page',
    )
  })

  it('creates a subnetwork with secondary ranges, showing the API’s messages', async () => {
    const user = userEvent.setup()
    const { router } = renderApp(`/compute/create-subnetwork?network=vpc&project=${P}`)
    expect(await screen.findByLabelText('Network')).toHaveValue('vpc')
    await user.type(screen.getByLabelText('Name'), 'db')
    await user.type(screen.getByLabelText('Primary range'), '10.0.1.1/24')
    await user.type(screen.getByLabelText('Secondary ranges'), 'Pods 10.8.0.0/14')
    await user.click(screen.getByRole('button', { name: 'Create' }))
    expect(
      await screen.findByText(
        "Invalid value for field 'resource.ipCidrRange': '10.0.1.1/24'. Must be a valid IPv4 CIDR network address range.",
      ),
    ).toBeVisible()
    expect(
      screen.getByText(/'resource.secondaryIpRanges.rangeName': 'Pods'. Must be a match of regex/),
    ).toBeVisible()
    await user.clear(screen.getByLabelText('Primary range'))
    await user.type(screen.getByLabelText('Primary range'), '10.0.0.0/30')
    await user.clear(screen.getByLabelText('Secondary ranges'))
    await user.type(screen.getByLabelText('Secondary ranges'), 'pods 10.8.0.0/14')
    await user.click(screen.getByRole('button', { name: 'Create' }))
    expect(await screen.findByText(/The prefix length must be between 8 and 29/)).toBeVisible()

    // A conflict only the API knows is shown above the buttons.
    await user.clear(screen.getByLabelText('Primary range'))
    await user.type(screen.getByLabelText('Primary range'), '10.0.0.0/25')
    await user.click(screen.getByRole('button', { name: 'Create' }))
    expect(await screen.findByText(/conflicts with existing subnetwork 'nodes'/)).toBeVisible()

    await user.clear(screen.getByLabelText('Primary range'))
    await user.type(screen.getByLabelText('Primary range'), '10.0.1.0/24')
    await user.click(screen.getByLabelText('Private Google Access'))
    await user.click(screen.getByRole('button', { name: 'Create' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/compute/regions/us-central1/subnetworks/db'),
    )
    expect(writes().at(-1)).toEqual({
      method: 'POST',
      path: `/compute/v1/projects/${P}/regions/us-central1/subnetworks`,
      body: {
        name: 'db',
        network: `projects/${P}/global/networks/vpc`,
        ipCidrRange: '10.0.1.0/24',
        privateIpGoogleAccess: true,
        secondaryIpRanges: [{ rangeName: 'pods', ipCidrRange: '10.8.0.0/14' }],
      },
    })
  })

  it('expands a subnetwork’s range and switches Private Google Access', async () => {
    const user = userEvent.setup()
    renderApp(`/compute/regions/us-central1/subnetworks/nodes?project=${P}`)
    const details = await screen.findByLabelText('Subnetwork details')
    expect(details).toHaveTextContent('10.0.0.0/24')
    expect(details).toHaveTextContent('pods 10.4.0.0/14')

    await user.click(screen.getByRole('button', { name: 'Expand range' }))
    const dialog = await screen.findByRole('dialog')
    const input = within(dialog).getByLabelText('New range')
    expect(input).toHaveValue('10.0.0.0/24')
    await user.clear(input)
    await user.type(input, '10.0.0.0/25')
    await user.click(within(dialog).getByRole('button', { name: 'Expand' }))
    expect(
      await within(dialog).findByText(/must contain the existing range 10.0.0.0\/24 and be larger/),
    ).toBeVisible()
    await user.clear(input)
    await user.type(input, '10.0.0.0/22')
    await user.click(within(dialog).getByRole('button', { name: 'Expand' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    await waitFor(() =>
      expect(screen.getByLabelText('Subnetwork details')).toHaveTextContent('10.0.0.0/22'),
    )

    const sw = screen.getByRole('switch', { name: 'Private Google Access' })
    expect(sw).toHaveAttribute('aria-checked', 'false')
    await user.click(sw)
    await waitFor(() =>
      expect(screen.getByRole('switch', { name: 'Private Google Access' })).toHaveAttribute(
        'aria-checked',
        'true',
      ),
    )
    expect(writes()).toEqual([
      {
        method: 'POST',
        path: `/compute/v1/projects/${P}/regions/us-central1/subnetworks/nodes/expandIpCidrRange`,
        body: { ipCidrRange: '10.0.0.0/25' },
      },
      {
        method: 'POST',
        path: `/compute/v1/projects/${P}/regions/us-central1/subnetworks/nodes/expandIpCidrRange`,
        body: { ipCidrRange: '10.0.0.0/22' },
      },
      {
        method: 'POST',
        path: `/compute/v1/projects/${P}/regions/us-central1/subnetworks/nodes/setPrivateIpGoogleAccess`,
        body: { privateIpGoogleAccess: true },
      },
    ])
  })

  it('edits a subnetwork’s description and secondary ranges with its fingerprint', async () => {
    const user = userEvent.setup()
    const { router } = renderApp(`/compute/regions/us-central1/subnetworks/nodes/edit?project=${P}`)
    const ranges = await screen.findByLabelText('Secondary ranges')
    expect(ranges).toHaveValue('pods 10.4.0.0/14')
    await user.type(screen.getByLabelText('Description'), 'GKE')
    await user.type(ranges, '\nsvc 10.8.0.0/20')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/compute/regions/us-central1/subnetworks/nodes'),
    )
    expect(writes()).toEqual([
      {
        method: 'PATCH',
        path: `/compute/v1/projects/${P}/regions/us-central1/subnetworks/nodes`,
        body: {
          fingerprint: 'fp-nodes',
          description: 'GKE',
          secondaryIpRanges: [
            { rangeName: 'pods', ipCidrRange: '10.4.0.0/14' },
            { rangeName: 'svc', ipCidrRange: '10.8.0.0/20' },
          ],
        },
      },
    ])
  })

  it('deletes a subnetwork and returns to its network', async () => {
    const user = userEvent.setup()
    const { router } = renderApp(`/compute/regions/us-central1/subnetworks/nodes?project=${P}`)
    await user.click(await screen.findByRole('button', { name: 'Delete subnetwork' }))
    await user.click(
      within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete subnetwork' }),
    )
    await waitFor(() => expect(router.state.location.pathname).toBe('/compute/global/networks/vpc'))
    expect(writes()).toEqual([
      {
        method: 'DELETE',
        path: `/compute/v1/projects/${P}/regions/us-central1/subnetworks/nodes`,
        body: undefined,
      },
    ])
  })
})

it('shows the Offered ports without a Project', async () => {
  renderApp('/compute/offers')
  const row = await screen.findByTestId('host-offers')
  expect(row).toHaveTextContent('10.0.0.254')
  expect(row).toHaveTextContent('8080/tcp')
})
