import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, describe, expect, it } from 'vitest'

import type { RequestEntry } from '@/api/admin'
import { appendRequest } from '@/api/events'
import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import type { Subnetwork } from '../vpc/api'
import { coverage, natRanges, type NatSink, type Router } from './api'
import { natRecipe, routerRecipe } from './copy'
import { natBody, natValues } from './NatForm'

const P = 'alpha-project'
const link = (p: string) => `https://www.googleapis.com/compute/v1/projects/${P}/${p}`
const ROUTER = `projects/${P}/regions/us-central1/routers/edge`

// A small in-memory compute API: routers with their NATs, subnetworks,
// networks and addresses, router status and NAT mappings, the offline
// sink and the Request log, and the writes the view sends.
let routers: Router[]
let subnets: Subnetwork[]
let sink: NatSink
let log: RequestEntry[]
let requests: { method: string; path: string; body: unknown }[]

const op = { name: 'op-1', status: 'DONE', selfLink: link('regions/us-central1/operations/op-1') }
const err = (code: number, message: string) =>
  HttpResponse.json({ error: { code, message } }, { status: code })

const subnet = (name: string, region: string, cidr: string, over: Partial<Subnetwork> = {}) => ({
  name,
  region: link(`regions/${region}`),
  network: link('global/networks/vpc'),
  ipCidrRange: cidr,
  selfLink: link(`regions/${region}/subnetworks/${name}`),
  ...over,
})

const egress = (
  code: string,
  over: Partial<RequestEntry['nat']> = {},
  resource = `${ROUTER}/nats/nat-a`,
): RequestEntry => ({
  time: '2026-10-12T10:00:00.250Z',
  service: 'nat',
  protocol: 'tcp',
  method: 'EGRESS',
  resource,
  status: code === 'DROPPED' ? 1 : 0,
  code,
  latencyMs: 0,
  nat: {
    network: `projects/${P}/global/networks/vpc`,
    subnetwork: `projects/${P}/regions/us-central1/subnetworks/nodes`,
    vm: `projects/${P}/zones/us-central1-a/instances/gke-node-1`,
    srcIp: '10.0.0.2',
    srcPort: 40000,
    destIp: '93.184.216.34',
    destPort: 443,
    ...(code === 'OK' ? { natIp: '203.0.113.5', natPort: 1030 } : {}),
    ...over,
  },
})

beforeEach(() => {
  requests = []
  fake.info.services = ['iam', 'compute', 'nat']
  subnets = [
    subnet('nodes', 'us-central1', '10.0.0.0/24', {
      secondaryIpRanges: [{ rangeName: 'pods', ipCidrRange: '10.4.0.0/14' }],
    }),
    subnet('db', 'us-central1', '10.0.1.0/24'),
    subnet('eu', 'europe-west1', '10.1.0.0/24'),
  ]
  routers = [
    {
      name: 'edge',
      selfLink: link('regions/us-central1/routers/edge'),
      region: link('regions/us-central1'),
      network: link('global/networks/vpc'),
      fingerprint: 'fp-edge',
      creationTimestamp: '2026-10-12T09:00:00Z',
      nats: [
        {
          name: 'nat-a',
          natIpAllocateOption: 'AUTO_ONLY',
          sourceSubnetworkIpRangesToNat: 'LIST_OF_SUBNETWORKS',
          subnetworks: [
            {
              name: link('regions/us-central1/subnetworks/nodes'),
              sourceIpRangesToNat: ['ALL_IP_RANGES'],
            },
          ],
          logConfig: { enable: true, filter: 'ERRORS_ONLY' },
          minPortsPerVm: 128,
        },
      ],
    },
    {
      name: 'quiet',
      selfLink: link('regions/europe-west1/routers/quiet'),
      region: link('regions/europe-west1'),
      network: link('global/networks/vpc'),
    },
  ]
  sink = { enabled: false, status: 503, body: 'gcpemu offline: egress blocked' }
  log = [
    egress('OK'),
    egress(
      'DROPPED',
      { srcIp: '10.0.1.9', srcPort: 5000, vm: undefined, subnetwork: undefined },
      `projects/${P}/global/networks/vpc`,
    ),
    egress(
      'OK',
      { destIp: '1.1.1.1', destPort: 80 },
      `projects/other/regions/us-central1/routers/x/nats/y`,
    ),
  ]

  const find = (region: string, name: string) =>
    routers.find((r) => r.name === name && r.region === link(`regions/${region}`))
  const record = async (request: Request) => {
    const text = await request.text()
    const body = text ? (JSON.parse(text) as Record<string, unknown>) : undefined
    requests.push({ method: request.method, path: new URL(request.url).pathname, body })
    return body ?? {}
  }
  const R = `*/compute/v1/projects/:p/regions/:region/routers`
  server.use(
    http.get('*/compute/v1/projects/:p/aggregated/routers', () =>
      HttpResponse.json({
        items: Object.fromEntries(
          ['us-central1', 'europe-west1'].map((reg) => [
            `regions/${reg}`,
            { routers: routers.filter((r) => r.region === link(`regions/${reg}`)) },
          ]),
        ),
      }),
    ),
    http.get('*/compute/v1/projects/:p/aggregated/subnetworks', () =>
      HttpResponse.json({ items: { 'regions/all': { subnetworks: subnets } } }),
    ),
    http.get('*/compute/v1/projects/:p/global/networks', () =>
      HttpResponse.json({ items: [{ name: 'vpc' }, { name: 'default' }] }),
    ),
    http.get('*/compute/v1/projects/:p/regions/:region/addresses', () =>
      HttpResponse.json({
        items: [
          { name: 'nat-ip-1', address: '34.1.1.1', addressType: 'EXTERNAL' },
          { name: 'internal', address: '10.0.0.9', addressType: 'INTERNAL' },
        ],
      }),
    ),
    http.post(R, async ({ request, params }) => {
      const body = (await record(request)) as Router
      routers.push({
        ...body,
        selfLink: link(`regions/${params.region as string}/routers/${body.name}`),
        region: link(`regions/${params.region as string}`),
        network: link(`global/networks/${body.network?.split('/').pop()}`),
      })
      return HttpResponse.json(op)
    }),
    http.get(`${R}/:name/getRouterStatus`, ({ params }) => {
      const r = find(params.region as string, params.name as string)
      return HttpResponse.json({
        result: {
          natStatus: (r?.nats ?? []).map((n) => ({
            name: n.name,
            autoAllocatedNatIps: ['203.0.113.5'],
            numVmEndpointsWithNatMappings: 2,
          })),
        },
      })
    }),
    http.get(`${R}/:name/getNatMappingInfo`, ({ request }) =>
      HttpResponse.json({
        result:
          new URL(request.url).searchParams.get('natName') === 'nat-a'
            ? [
                {
                  instanceName: `projects/${P}/zones/us-central1-a/instances/gke-node-1`,
                  interfaceNatMappings: [
                    {
                      sourceVirtualIp: '10.0.0.2',
                      natIpPortRanges: ['203.0.113.5:1024-1151'],
                      numTotalNatPorts: 128,
                    },
                  ],
                },
                {
                  instanceName: `projects/${P}/zones/us-central1-a/instances/gke-node-2`,
                  interfaceNatMappings: [
                    {
                      sourceVirtualIp: '10.0.0.3',
                      natIpPortRanges: ['203.0.113.5:1152-1279'],
                      numTotalNatPorts: 128,
                    },
                  ],
                },
              ]
            : [],
      }),
    ),
    http.get(`${R}/:name`, ({ params }) => {
      const r = find(params.region as string, params.name as string)
      return r ? HttpResponse.json(r) : err(404, `The resource '${ROUTER}' was not found`)
    }),
    http.patch(`${R}/:name`, async ({ request, params }) => {
      const body = (await record(request)) as Partial<Router>
      const r = find(params.region as string, params.name as string)!
      const bad = body.nats?.findIndex((n) => n.natIps?.includes('missing'))
      if (bad !== undefined && bad >= 0) {
        return err(
          400,
          `Invalid value for field 'resource.nats[${bad}].natIps[0]': 'missing'. The referenced address resource cannot be found.`,
        )
      }
      Object.assign(r, body)
      return HttpResponse.json(op)
    }),
    http.delete(`${R}/:name`, async ({ request, params }) => {
      await record(request)
      routers = routers.filter((r) => r !== find(params.region as string, params.name as string))
      return HttpResponse.json(op)
    }),
    http.get('*/_emu/v1/nat/sink', () => HttpResponse.json(sink)),
    http.put('*/_emu/v1/nat/sink', async ({ request }) => {
      const body = (await record(request)) as { enabled: boolean }
      sink = { ...sink, enabled: body.enabled }
      return HttpResponse.json(sink)
    }),
    http.get('*/_emu/v1/requests', () => HttpResponse.json({ requests: log })),
  )
})

const writes = () => requests.filter((r) => r.method !== 'GET')

describe('coverage', () => {
  const nodes = subnet('nodes', 'us-central1', '10.0.0.0/24', {
    secondaryIpRanges: [
      { rangeName: 'pods', ipCidrRange: '10.4.0.0/14' },
      { rangeName: 'svc', ipCidrRange: '10.8.0.0/20' },
    ],
  })
  it('follows sourceSubnetworkIpRangesToNat', () => {
    expect(
      natRanges(
        { name: 'n', sourceSubnetworkIpRangesToNat: 'ALL_SUBNETWORKS_ALL_IP_RANGES' },
        nodes,
      ).map((r) => r.cidr),
    ).toEqual(['10.0.0.0/24', '10.4.0.0/14', '10.8.0.0/20'])
    expect(
      natRanges(
        { name: 'n', sourceSubnetworkIpRangesToNat: 'ALL_SUBNETWORKS_ALL_PRIMARY_IP_RANGES' },
        nodes,
      ),
    ).toEqual([{ name: 'primary', cidr: '10.0.0.0/24' }])
    expect(
      natRanges(
        {
          name: 'n',
          sourceSubnetworkIpRangesToNat: 'LIST_OF_SUBNETWORKS',
          subnetworks: [
            {
              name: `projects/${P}/regions/us-central1/subnetworks/nodes`,
              sourceIpRangesToNat: ['PRIMARY_IP_RANGE', 'LIST_OF_SECONDARY_IP_RANGES'],
              secondaryIpRangeNames: ['svc'],
            },
          ],
        },
        nodes,
      ).map((r) => r.name),
    ).toEqual(['primary', 'svc'])
  })

  it('covers only the router’s region and network', () => {
    const r = routers[0]!
    const all = {
      name: 'n',
      sourceSubnetworkIpRangesToNat: 'ALL_SUBNETWORKS_ALL_IP_RANGES' as const,
    }
    const other = subnet('x', 'us-central1', '10.9.0.0/24', {
      network: link('global/networks/other'),
    })
    expect(coverage(r, all, [...subnets, other]).map((c) => c.subnetwork.name)).toEqual([
      'db',
      'nodes',
    ])
  })
})

describe('the NAT form’s body', () => {
  it('keeps what the form does not edit, and the API’s links', () => {
    const nat = routers[0]!.nats![0]!
    const base = { ...nat, udpIdleTimeoutSec: 60 }
    expect(natBody(natValues(base), base)).toEqual(base)
    const v = {
      ...natValues(base),
      logEnable: false,
      dynamic: true,
      minPorts: '64',
      maxPorts: '1024',
    }
    expect(natBody(v, base)).toEqual({
      ...base,
      logConfig: { enable: false, filter: 'ERRORS_ONLY' },
      minPortsPerVm: 64,
      enableDynamicPortAllocation: true,
      maxPortsPerVm: 1024,
    })
  })
})

it('asks for the Service and a Project', async () => {
  fake.info.services = ['iam', 'compute']
  const { unmount } = renderApp(`/nat?project=${P}`)
  expect(await screen.findByRole('heading', { name: /Service nat is not enabled/ })).toBeVisible()
  unmount()
  fake.info.services = ['iam', 'compute', 'nat']
  renderApp('/nat')
  expect(await screen.findByText(/Choose a Project with the Project switcher/)).toBeVisible()
})

it('lists routers with their NATs and covered subnetworks, filtered by name', async () => {
  const user = userEvent.setup()
  renderApp(`/nat?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Routers' })
  const rows = within(table).getAllByTestId('router')
  expect(rows).toHaveLength(2)
  expect(rows[0]).toHaveTextContent('edge')
  expect(rows[0]).toHaveTextContent('us-central1')
  expect(rows[0]).toHaveTextContent('vpc')
  expect(within(rows[0]!).getByRole('link', { name: 'nat-a' })).toHaveAttribute(
    'href',
    `/nat/regions/us-central1/routers/edge/nats/nat-a?project=${P}`,
  )
  await waitFor(() => expect(rows[0]).toHaveTextContent(/nodes$/))
  expect(rows[1]).toHaveTextContent('None')

  await user.type(screen.getByLabelText('Filter'), 'nat-a')
  expect(within(table).getAllByTestId('router')).toHaveLength(1)
})

it('toggles the offline sink', async () => {
  const user = userEvent.setup()
  renderApp(`/nat?project=${P}`)
  const sw = await screen.findByRole('switch', { name: 'Offline sink' })
  expect(sw).toHaveAttribute('aria-checked', 'false')
  expect(screen.getByText(/Egress a Cloud NAT allows reaches the internet/)).toBeVisible()
  await user.click(sw)
  await waitFor(() => expect(sw).toHaveAttribute('aria-checked', 'true'))
  expect(writes()).toEqual([{ method: 'PUT', path: '/_emu/v1/nat/sink', body: { enabled: true } }])
  expect(screen.getByText('503 gcpemu offline: egress blocked')).toBeVisible()
})

it('creates a router, showing the API’s messages', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/nat/create?project=${P}`)
  await user.type(await screen.findByLabelText('Name'), 'Edge_2')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(await screen.findByText(/Invalid value for field 'resource.name': 'Edge_2'/)).toBeVisible()
  expect(screen.getByText("Required field 'resource.network' not specified")).toBeVisible()
  await user.clear(screen.getByLabelText('Name'))
  await user.type(screen.getByLabelText('Name'), 'edge-2')
  await user.type(screen.getByLabelText('Network'), 'vpc')
  await user.type(screen.getByLabelText('BGP ASN'), '100')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(await screen.findByText(/Must be a private ASN/)).toBeVisible()
  await user.clear(screen.getByLabelText('BGP ASN'))
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/nat/regions/us-central1/routers/edge-2'),
  )
  expect(writes()).toEqual([
    {
      method: 'POST',
      path: `/compute/v1/projects/${P}/regions/us-central1/routers`,
      body: { name: 'edge-2', network: `projects/${P}/global/networks/vpc` },
    },
  ])
})

it('shows a router’s NAT gateways and deletes it', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/nat/regions/us-central1/routers/edge?project=${P}`)
  const nats = await screen.findByRole('table', { name: 'NAT gateways' })
  const row = within(nats).getByTestId('nat')
  await waitFor(() => expect(row).toHaveTextContent('203.0.113.5'))
  expect(row).toHaveTextContent('Automatic')
  expect(row).toHaveTextContent('LIST_OF_SUBNETWORKS')
  expect(within(row).getByRole('link', { name: 'nodes' })).toHaveAttribute(
    'href',
    `/compute/regions/us-central1/subnetworks/nodes?project=${P}`,
  )
  expect(row).toHaveTextContent('Errors only')
  expect(within(row).getAllByRole('cell')[4]).toHaveTextContent('2')
  expect(screen.getByLabelText('Router JSON')).toHaveTextContent('"fingerprint": "fp-edge"')

  await user.click(screen.getByRole('button', { name: 'Delete router' }))
  const dialog = await screen.findByRole('dialog')
  expect(dialog).toHaveTextContent('Its NAT gateways (nat-a) go with it')
  await user.click(within(dialog).getByRole('button', { name: 'Delete router' }))
  await waitFor(() => expect(router.state.location.pathname).toBe('/nat'))
  expect(writes()).toEqual([{ method: 'DELETE', path: `/compute/v1/${ROUTER}`, body: undefined }])
})

it('edits a router’s description and ASN as a merge patch', async () => {
  const user = userEvent.setup()
  routers[0]!.bgp = { asn: 64512, keepaliveInterval: 20 }
  renderApp(`/nat/regions/us-central1/routers/edge/edit?project=${P}`)
  await user.type(await screen.findByLabelText('Description'), 'egress')
  await user.clear(screen.getByLabelText('BGP ASN'))
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(writes()).toHaveLength(1))
  expect(writes()[0]).toEqual({
    method: 'PATCH',
    path: `/compute/v1/${ROUTER}`,
    body: { description: 'egress', bgp: { asn: null } },
  })
})

it('adds a NAT gateway with the router’s other NATs kept', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/nat/regions/us-central1/routers/edge/nats/add?project=${P}`)
  await user.type(await screen.findByLabelText('Name'), 'nat-a')
  await user.selectOptions(screen.getByLabelText('NAT IP allocation'), 'MANUAL_ONLY')
  await user.click(screen.getByRole('button', { name: 'Add' }))
  expect(
    await screen.findByText(
      "Invalid value for field 'resource.nats[1].name': 'nat-a'. NAT names must be unique within a router.",
    ),
  ).toBeVisible()
  expect(
    screen.getByText(/'resource.nats\[1\].natIps': ''. At least one NAT IP is required/),
  ).toBeVisible()

  // An error only the API knows is shown on the field it names.
  await user.clear(screen.getByLabelText('Name'))
  await user.type(screen.getByLabelText('Name'), 'nat-b')
  await user.type(screen.getByLabelText('NAT IPs'), 'missing')
  await user.click(screen.getByRole('button', { name: 'Add' }))
  expect(
    (await screen.findAllByText(/The referenced address resource cannot be found/))[0],
  ).toBeVisible()
  expect(screen.getByLabelText('NAT IPs')).toHaveAttribute('aria-invalid', 'true')

  await user.clear(screen.getByLabelText('NAT IPs'))
  await user.click(await screen.findByRole('button', { name: 'Use address nat-ip-1' }))
  expect(screen.queryByRole('button', { name: /Use address internal/ })).toBeNull()
  await user.selectOptions(screen.getByLabelText('Source ranges'), 'LIST_OF_SUBNETWORKS')
  await user.selectOptions(await screen.findByLabelText('Ranges of db'), 'PRIMARY_IP_RANGE')
  expect(screen.queryByLabelText('Ranges of eu')).toBeNull()
  await user.click(screen.getByLabelText('Logging'))
  await user.click(screen.getByRole('button', { name: 'Add' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/nat/regions/us-central1/routers/edge/nats/nat-b'),
  )
  const sent = writes().at(-1)!.body as { nats: unknown[]; fingerprint: string }
  expect(sent.fingerprint).toBe('fp-edge')
  expect(sent.nats).toEqual([
    routers[0]!.nats![0],
    {
      name: 'nat-b',
      natIpAllocateOption: 'MANUAL_ONLY',
      natIps: ['nat-ip-1'],
      sourceSubnetworkIpRangesToNat: 'LIST_OF_SUBNETWORKS',
      subnetworks: [{ name: 'db', sourceIpRangesToNat: ['PRIMARY_IP_RANGE'] }],
      logConfig: { enable: true, filter: 'ALL' },
    },
  ])
})

it('adds a NAT gateway from the JSON editor', async () => {
  const user = userEvent.setup()
  renderApp(`/nat/regions/europe-west1/routers/quiet/nats/add?project=${P}`)
  await user.type(await screen.findByLabelText('Name'), 'nat-eu')
  await user.click(screen.getByRole('tab', { name: 'JSON' }))
  const json = screen.getByLabelText('Request body')
  const body = JSON.parse((json as HTMLTextAreaElement).value) as Record<string, unknown>
  expect(body).toEqual({
    name: 'nat-eu',
    natIpAllocateOption: 'AUTO_ONLY',
    sourceSubnetworkIpRangesToNat: 'ALL_SUBNETWORKS_ALL_IP_RANGES',
  })
  await user.clear(json)
  await user.click(json)
  await user.paste(JSON.stringify({ ...body, udpIdleTimeoutSec: 45 }))
  await user.click(screen.getByRole('button', { name: 'Add' }))
  await waitFor(() => expect(writes()).toHaveLength(1))
  expect(writes()[0]!.body).toEqual({
    nats: [{ ...body, udpIdleTimeoutSec: 45 }],
  })
})

it('shows a NAT’s settings, coverage, port usage and translations', async () => {
  const user = userEvent.setup()
  const { queryClient } = renderApp(`/nat/regions/us-central1/routers/edge/nats/nat-a?project=${P}`)
  const settings = await screen.findByLabelText('NAT gateway settings')
  await waitFor(() => expect(settings).toHaveTextContent('203.0.113.5'))
  expect(settings).toHaveTextContent('Minimum ports per VM128')
  expect(settings).toHaveTextContent('Errors only')

  const covered = await screen.findByRole('table', { name: 'Covered subnetworks' })
  expect(within(covered).getAllByTestId('covered')).toHaveLength(1)
  expect(covered).toHaveTextContent('10.0.0.0/24 primary')
  expect(covered).toHaveTextContent('10.4.0.0/14 pods')
  expect(within(covered).getByRole('link', { name: 'nodes' })).toHaveAttribute(
    'href',
    `/compute/regions/us-central1/subnetworks/nodes?project=${P}`,
  )

  const ports = await screen.findByRole('table', { name: 'Port usage' })
  expect(within(ports).getAllByTestId('mapping')).toHaveLength(2)
  expect(ports).toHaveTextContent('gke-node-1')
  expect(ports).toHaveTextContent('203.0.113.5:1024-1151')
  expect(screen.getByText('2 endpoints, 256 ports allocated.')).toBeVisible()

  // Only this NAT's connections; new ones arrive live.
  const conns = await screen.findByRole('table', { name: 'Connections' })
  expect(within(conns).getAllByTestId('connection')).toHaveLength(1)
  expect(conns).toHaveTextContent('203.0.113.5:1030')
  act(() =>
    appendRequest(queryClient, egress('OFFLINE_SINK', { destPort: 80, request: 'GET / HTTP/1.1' })),
  )
  await waitFor(() => expect(within(conns).getAllByTestId('connection')).toHaveLength(2))
  const first = within(conns).getAllByTestId('connection')[0]!
  expect(first).toHaveTextContent('Offline sink')
  await user.click(within(first).getByRole('button', { name: /Details of/ }))
  expect(screen.getByLabelText(/^Connection 10.0.0.2:40000/)).toHaveTextContent('GET / HTTP/1.1')
})

it('edits and deletes a NAT gateway through its router', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/nat/regions/us-central1/routers/edge/nats/nat-a/edit?project=${P}`)
  expect(await screen.findByLabelText('Name')).toHaveAttribute('readonly')
  expect(await screen.findByLabelText('Ranges of nodes')).toHaveValue('ALL_IP_RANGES')
  await user.selectOptions(screen.getByLabelText('Log'), 'ALL')
  await user.click(screen.getByLabelText('Dynamic port allocation'))
  await user.click(screen.getByRole('button', { name: 'Save' }))
  // 128 is a power of two: dynamic port allocation accepts it.
  await waitFor(() => expect(writes()).toHaveLength(1))
  const nat = (writes()[0]!.body as { nats: Record<string, unknown>[] }).nats[0]!
  expect(nat).toMatchObject({
    name: 'nat-a',
    logConfig: { enable: true, filter: 'ALL' },
    enableDynamicPortAllocation: true,
    minPortsPerVm: 128,
    subnetworks: routers[0]!.nats![0]!.subnetworks,
  })
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/nat/regions/us-central1/routers/edge/nats/nat-a'),
  )

  await user.click(await screen.findByRole('button', { name: 'Delete NAT gateway' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete NAT gateway' }),
  )
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/nat/regions/us-central1/routers/edge'),
  )
  expect(writes().at(-1)).toEqual({
    method: 'PATCH',
    path: `/compute/v1/${ROUTER}`,
    body: { nats: [], fingerprint: 'fp-edge' },
  })
})

it('filters the translation log of the Project from the URL', async () => {
  const user = userEvent.setup()
  renderApp(`/nat/translations?project=${P}&outcome=DROPPED`)
  const conns = await screen.findByRole('table', { name: 'Connections' })
  // The other Project's connection is never shown.
  expect(within(conns).getAllByTestId('connection')).toHaveLength(1)
  expect(conns).toHaveTextContent('Dropped')
  expect(conns).toHaveTextContent('global/networks/vpc')
  await user.selectOptions(screen.getByLabelText('Outcome'), '')
  expect(within(conns).getAllByTestId('connection')).toHaveLength(2)
  await user.type(screen.getByLabelText('Source'), 'gke-node')
  expect(within(conns).getAllByTestId('connection')).toHaveLength(1)
  expect(within(conns).getByRole('link', { name: 'edge/nat-a' })).toBeVisible()
})

it('says when there is no egress yet', async () => {
  log = []
  renderApp(`/nat/translations?project=${P}`)
  expect(await screen.findByText(/No egress from this Project yet/)).toBeVisible()
})

it('copies a NAT gateway as gcloud, curl and OpenTofu', async () => {
  const user = userEvent.setup()
  renderApp(`/nat/regions/us-central1/routers/edge/nats/nat-a?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Copy NAT gateway as' }))
  const dialog = await screen.findByRole('dialog', { name: 'Copy NAT gateway as' })
  expect(within(dialog).getByLabelText('gcloud for the NAT gateway')).toHaveTextContent(
    [
      'gcloud compute routers nats create nat-a \\',
      `    --project=${P} \\`,
      '    --region=us-central1 \\',
      '    --router=edge \\',
      '    --auto-allocate-nat-external-ips \\',
      '    --nat-custom-subnet-ip-ranges=nodes:ALL \\',
      '    --min-ports-per-vm=128 \\',
      '    --enable-logging \\',
      '    --log-filter=ERRORS_ONLY',
    ].join('\n'),
    { normalizeWhitespace: false },
  )

  // A NAT is created by patching its router's nats[].
  await user.click(within(dialog).getByRole('tab', { name: 'curl' }))
  const curl = within(dialog).getByLabelText('curl for the NAT gateway')
  expect(curl).toHaveTextContent(`curl ${location.origin}/compute/v1/${ROUTER} \\ -X PATCH`)
  expect(curl).toHaveTextContent('"nats": [ { "name": "nat-a"')

  await user.click(within(dialog).getByRole('tab', { name: 'OpenTofu' }))
  const tofu = within(dialog).getByLabelText('OpenTofu for the NAT gateway')
  expect(tofu).toHaveTextContent('resource "google_compute_router_nat" "nat-a" {')
  expect(tofu).toHaveTextContent('source_subnetwork_ip_ranges_to_nat = "LIST_OF_SUBNETWORKS"')
  expect(tofu).toHaveTextContent(
    `subnetwork { name = "projects/${P}/regions/us-central1/subnetworks/nodes" source_ip_ranges_to_nat = ["ALL_IP_RANGES"] }`,
  )
  expect(tofu).toHaveTextContent('log_config { enable = true filter = "ERRORS_ONLY" }')
})

describe('Copy as', () => {
  const ref = { project: P, region: 'us-central1', router: 'edge' }

  it('copies a router with custom BGP advertisements, without its NATs', () => {
    const r = routerRecipe(
      {
        ...routers[0]!,
        description: 'Edge',
        bgp: {
          asn: 64514,
          advertiseMode: 'CUSTOM',
          advertisedGroups: ['ALL_SUBNETS'],
          advertisedIpRanges: [{ range: '10.9.0.0/16' }],
        },
      },
      ref,
    )
    expect(r.gcloud).toBe(
      [
        'gcloud compute routers create edge \\',
        `    --project=${P} \\`,
        '    --region=us-central1 \\',
        '    --network=vpc \\',
        '    --description=Edge \\',
        '    --asn=64514 \\',
        '    --advertisement-mode=CUSTOM \\',
        '    --set-advertisement-groups=ALL_SUBNETS \\',
        '    --set-advertisement-ranges=10.9.0.0/16',
      ].join('\n'),
    )
    expect(r.rest).toEqual({
      method: 'POST',
      path: `/compute/v1/projects/${P}/regions/us-central1/routers`,
      body: {
        name: 'edge',
        region: link('regions/us-central1'),
        network: link('global/networks/vpc'),
        description: 'Edge',
        bgp: {
          asn: 64514,
          advertiseMode: 'CUSTOM',
          advertisedGroups: ['ALL_SUBNETS'],
          advertisedIpRanges: [{ range: '10.9.0.0/16' }],
        },
      },
    })
    expect(r.tofu).toBe(
      [
        'resource "google_compute_router" "edge" {',
        `  project     = "${P}"`,
        '  name        = "edge"',
        '  region      = "us-central1"',
        '  network     = "vpc"',
        '  description = "Edge"',
        '  bgp {',
        '    asn               = 64514',
        '    advertise_mode    = "CUSTOM"',
        '    advertised_groups = ["ALL_SUBNETS"]',
        '    advertised_ip_ranges {',
        '      range = "10.9.0.0/16"',
        '    }',
        '  }',
        '}',
      ].join('\n'),
    )
  })

  it('copies a manual NAT with chosen ranges and timeouts', () => {
    const nat = {
      name: 'nat-m',
      natIpAllocateOption: 'MANUAL_ONLY' as const,
      natIps: [link('regions/us-central1/addresses/ip-1')],
      sourceSubnetworkIpRangesToNat: 'LIST_OF_SUBNETWORKS' as const,
      subnetworks: [
        {
          name: link('regions/us-central1/subnetworks/nodes'),
          sourceIpRangesToNat: [
            'PRIMARY_IP_RANGE' as const,
            'LIST_OF_SECONDARY_IP_RANGES' as const,
          ],
          secondaryIpRangeNames: ['pods'],
        },
      ],
      enableDynamicPortAllocation: true,
      udpIdleTimeoutSec: 60,
    }
    const r = natRecipe(routers[0]!, nat, ref)
    expect(r.gcloud).toBe(
      [
        'gcloud compute routers nats create nat-m \\',
        `    --project=${P} \\`,
        '    --region=us-central1 \\',
        '    --router=edge \\',
        '    --nat-external-ip-pool=ip-1 \\',
        '    --nat-custom-subnet-ip-ranges=nodes,nodes:pods \\',
        '    --enable-dynamic-port-allocation \\',
        '    --udp-idle-timeout=60s',
      ].join('\n'),
    )
    // A NAT the router lacks is added to the ones it has.
    expect(r.rest).toMatchObject({
      method: 'PATCH',
      body: { nats: [{ name: 'nat-a' }, { name: 'nat-m' }] },
    })
    expect(r.tofu).toContain(
      `  nat_ips                            = ["projects/${P}/regions/us-central1/addresses/ip-1"]`,
    )
    expect(r.tofu).toContain('  udp_idle_timeout_sec               = 60')
    expect(r.tofu).toContain(
      [
        '  subnetwork {',
        `    name                     = "projects/${P}/regions/us-central1/subnetworks/nodes"`,
        '    source_ip_ranges_to_nat  = ["PRIMARY_IP_RANGE", "LIST_OF_SECONDARY_IP_RANGES"]',
        '    secondary_ip_range_names = ["pods"]',
        '  }',
      ].join('\n'),
    )
    expect(r.tofu).not.toContain('log_config')
  })
})
