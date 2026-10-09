import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, expect, it } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import type { LbListener, LbRoute, Resource } from './api'

const P = 'alpha-project'
const API = `*/compute/v1/projects/${P}`
const link = (p: string) => `https://www.googleapis.com/compute/v1/projects/${P}/${p}`

/** state is the fake compute API's load-balancing resources, by collection. */
let state: Record<string, Resource[]>
let listeners: LbListener[]
let routeResult: LbRoute
/** requests records the writes and route tests the fakes received. */
let requests: { method: string; path: string; body: unknown }[] = []

async function record(request: Request) {
  const text = await request.text()
  const url = new URL(request.url)
  requests.push({
    method: request.method,
    path: url.pathname,
    body: text ? (JSON.parse(text) as unknown) : undefined,
  })
  return requests.at(-1)!.body as Record<string, unknown>
}

const res = (coll: string, name: string, over: Partial<Resource> = {}): Resource => ({
  name,
  selfLink: link(`global/${coll}/${name}`),
  fingerprint: `fp-${name}`,
  creationTimestamp: '2026-10-09T10:00:00Z',
  ...over,
})

const op = { name: 'op-1', status: 'DONE', selfLink: link('global/operations/op-1') }
const notFound = (path: string) =>
  HttpResponse.json(
    { error: { code: 404, message: `The resource '${path}' was not found` } },
    { status: 404 },
  )

beforeEach(() => {
  requests = []
  state = {
    forwardingRules: [
      res('forwardingRules', 'web', {
        IPAddress: '34.120.0.1',
        portRange: '80-80',
        loadBalancingScheme: 'EXTERNAL_MANAGED',
        target: link('global/targetHttpProxies/web-proxy'),
      }),
      res('forwardingRules', 'internal', {
        selfLink: link('regions/us-east1/forwardingRules/internal'),
        region: link('regions/us-east1'),
        IPAddress: '10.0.0.9',
        portRange: '80-80',
        loadBalancingScheme: 'INTERNAL_MANAGED',
        target: link('regions/us-east1/targetHttpProxies/int-proxy'),
      }),
    ],
    targetHttpProxies: [
      res('targetHttpProxies', 'web-proxy', { urlMap: link('global/urlMaps/web-map') }),
    ],
    targetHttpsProxies: [],
    urlMaps: [
      res('urlMaps', 'web-map', {
        defaultService: link('global/backendBuckets/static'),
        hostRules: [{ hosts: ['app.example.test'], pathMatcher: 'app' }],
        pathMatchers: [
          {
            name: 'app',
            defaultService: link('global/backendBuckets/static'),
            pathRules: [{ paths: ['/api/*'], service: link('global/backendServices/api') }],
            routeRules: [],
          },
        ],
      }),
    ],
    backendServices: [
      res('backendServices', 'api', {
        protocol: 'HTTP',
        loadBalancingScheme: 'EXTERNAL_MANAGED',
        healthChecks: [link('global/healthChecks/hc')],
        backends: [
          { group: link('zones/us-central1-a/networkEndpointGroups/neg'), balancingMode: 'RATE' },
        ],
      }),
    ],
    backendBuckets: [res('backendBuckets', 'static', { bucketName: 'assets', enableCdn: true })],
    healthChecks: [
      res('healthChecks', 'hc', {
        type: 'HTTP',
        checkIntervalSec: 5,
        httpHealthCheck: { port: 80 },
      }),
    ],
    sslCertificates: [
      res('sslCertificates', 'managed', {
        type: 'MANAGED',
        managed: {
          domains: ['app.example.test'],
          status: 'PROVISIONING',
          domainStatus: { 'app.example.test': 'FAILED_NOT_VISIBLE' },
        },
      }),
    ],
    sslPolicies: [],
  }
  listeners = [
    {
      forwardingRule: `projects/${P}/global/forwardingRules/web`,
      ipAddress: '34.120.0.1',
      port: 80,
      listener: '127.0.0.7:80',
      mode: 'loopback',
      https: false,
    },
  ]
  routeResult = {
    match: 'pathRule',
    pathMatcher: 'app',
    pathRule: { paths: ['/api/*'] },
    service: link('global/backendServices/api'),
    host: 'app.example.test',
    path: '/api/users',
  }
  fake.info.services = ['iam', 'compute', 'lb']
  const find = (coll: string, name: string) => state[coll]?.find((r) => r.name === name)
  server.use(
    http.get(`${API}/aggregated/:coll`, ({ params }) => {
      const coll = params.coll as string
      if (coll === 'networkEndpointGroups') {
        return HttpResponse.json({
          items: {
            'zones/us-central1-a': {
              networkEndpointGroups: [
                {
                  name: 'neg',
                  zone: link('zones/us-central1-a'),
                  selfLink: link('zones/us-central1-a/networkEndpointGroups/neg'),
                },
                {
                  name: 'neg2',
                  zone: link('zones/us-central1-a'),
                  selfLink: link('zones/us-central1-a/networkEndpointGroups/neg2'),
                },
              ],
            },
          },
        })
      }
      return HttpResponse.json({ items: { global: { [coll]: state[coll] ?? [] } } })
    }),
    http.get(`${API}/global/backendBuckets`, () =>
      HttpResponse.json({ items: state.backendBuckets }),
    ),
    http.get(`${API}/:scope/:coll/:name`, ({ params }) => {
      const r = find(params.coll as string, params.name as string)
      return r
        ? HttpResponse.json(r)
        : notFound(`${params.coll as string}/${params.name as string}`)
    }),
    http.post(`${API}/global/:coll`, async ({ request }) => {
      await record(request)
      return HttpResponse.json(op)
    }),
    http.patch(`${API}/global/:coll/:name`, async ({ request }) => {
      await record(request)
      return HttpResponse.json(op)
    }),
    http.delete(`${API}/global/:coll/:name`, async ({ request, params }) => {
      await record(request)
      if (params.name === 'web-map') {
        return HttpResponse.json(
          {
            error: {
              code: 400,
              message: `The url_map resource 'projects/${P}/global/urlMaps/web-map' is already being used by 'projects/${P}/global/targetHttpProxies/web-proxy'`,
            },
          },
          { status: 400 },
        )
      }
      return HttpResponse.json(op)
    }),
    http.post(`${API}/global/backendServices/api/getHealth`, async ({ request }) => {
      const { group } = (await request.json()) as { group: string }
      return HttpResponse.json({
        healthStatus: group.endsWith('/neg')
          ? [
              { ipAddress: '10.0.0.5', port: 8000, healthState: 'HEALTHY' },
              { ipAddress: '10.0.0.6', port: 8000, healthState: 'UNHEALTHY' },
            ]
          : [],
      })
    }),
    http.get('*/_emu/v1/lb/listeners', () => HttpResponse.json({ listeners })),
    http.post('*/_emu/v1/lb/route', async ({ request }) => {
      await record(request)
      return HttpResponse.json(routeResult)
    }),
  )
})

it('says how to enable the Service when it is off', async () => {
  fake.info.services = ['iam']
  renderApp(`/lb?project=${P}`)
  expect(await screen.findByRole('heading', { name: /Service lb is not enabled/ })).toBeVisible()
})

it('asks for a Project first', async () => {
  renderApp('/lb')
  expect(await screen.findByText(/Choose a Project/)).toBeInTheDocument()
})

it('lists forwarding rules with their local listeners, filtering by scope', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/lb?project=${P}`)
  await screen.findByRole('table', { name: 'Forwarding rules' })
  const rows = () => screen.getAllByTestId('resource')
  expect(rows()).toHaveLength(2)
  const web = rows().find((r) => within(r).queryByText('web'))!
  expect(web).toHaveTextContent('34.120.0.1:80')
  expect(web).toHaveTextContent('127.0.0.7:80')
  expect(within(web).getByRole('link', { name: 'web-proxy' })).toHaveAttribute(
    'href',
    `/lb/global/targetHttpProxies/web-proxy?project=${P}`,
  )
  await user.type(screen.getByLabelText('Scope'), 'us-east1')
  expect(rows()).toHaveLength(1)
  // The link carries the view state once the URL has caught up.
  await waitFor(() =>
    expect(screen.getByRole('link', { name: 'internal' })).toHaveAttribute(
      'href',
      `/lb/regions/us-east1/forwardingRules/internal?project=${P}&location=us-east1`,
    ),
  )
  expect(router.state.location.search).toContain('location=us-east1')
  await user.clear(screen.getByLabelText('Scope'))
  await user.click(screen.getByRole('link', { name: 'Certificates' }))
  expect(await screen.findByRole('table', { name: 'Certificates' })).toHaveTextContent(
    'PROVISIONING',
  )
})

it('shows a forwarding rule’s listener and the chain it serves', async () => {
  renderApp(`/lb/global/forwardingRules/web?project=${P}`)
  const card = await screen.findByRole('region', { name: 'Local listener' })
  expect(await within(card).findByText('127.0.0.7:80')).toBeVisible()
  expect(within(card).getByText('curl http://127.0.0.7:80/')).toBeVisible()
  expect(await within(card).findByRole('link', { name: 'web-map' })).toBeVisible()
})

it('shows each endpoint’s health', async () => {
  renderApp(`/lb/global/backendServices/api?project=${P}`)
  const endpoints = await screen.findAllByTestId('endpoint')
  expect(endpoints).toHaveLength(2)
  expect(endpoints[0]).toHaveTextContent('10.0.0.5:8000')
  expect(endpoints[0]).toHaveTextContent('HEALTHY')
  expect(endpoints[1]).toHaveTextContent('UNHEALTHY')
})

it('shows a managed certificate’s status per domain', async () => {
  renderApp(`/lb/global/sslCertificates/managed?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Domain status' })
  expect(table).toHaveTextContent('app.example.test')
  expect(table).toHaveTextContent('FAILED_NOT_VISIBLE')
  expect(screen.queryByRole('link', { name: 'Edit' })).not.toBeInTheDocument()
})

it('renders a URL map as a route tree and tests URLs against it', async () => {
  const user = userEvent.setup()
  renderApp(`/lb/global/urlMaps/web-map?project=${P}`)
  const tree = await screen.findByRole('list', { name: 'Route tree' })
  expect(within(tree).getByTestId('host-rule')).toHaveTextContent('app.example.test')
  expect(within(tree).getByTestId('path-rule')).toHaveTextContent('/api/*')
  expect(within(tree).getByTestId('map-default')).toHaveTextContent('static')

  const url = screen.getByLabelText('URL')
  expect(url).toHaveValue('http://app.example.test/')
  await user.clear(url)
  await user.type(url, 'https://app.example.test/api/users?x=1')
  await user.type(screen.getByLabelText('Headers'), 'X-Canary: 1')
  await user.click(screen.getByRole('button', { name: 'Test' }))
  const result = await screen.findByRole('status', { name: 'Route' })
  expect(result).toHaveTextContent('Path rule of path matcher app (/api/*)')
  expect(within(result).getByRole('link', { name: 'api' })).toBeVisible()
  expect(await within(result).findByText('10.0.0.5:8000')).toBeVisible()
  expect(within(tree).getByTestId('path-rule')).toHaveTextContent('Matched')
  expect(requests).toEqual([
    {
      method: 'POST',
      path: '/_emu/v1/lb/route',
      body: {
        urlMap: `projects/${P}/global/urlMaps/web-map`,
        scheme: 'https',
        method: 'GET',
        host: 'app.example.test',
        path: '/api/users?x=1',
        headers: { 'X-Canary': '1' },
      },
    },
  ])

  await user.clear(screen.getByLabelText('Headers'))
  await user.type(screen.getByLabelText('Headers'), 'no colon')
  await user.click(screen.getByRole('button', { name: 'Test' }))
  expect(await screen.findByText(/Write headers as Name: value/)).toBeVisible()
})

it('edits a URL map with a merge patch of what changed', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/lb/global/urlMaps/web-map/edit?project=${P}`)
  const rules = await screen.findByLabelText('Path rules')
  expect(rules).toHaveValue('/api/* = global/backendServices/api')
  await user.type(rules, '\n/img/* = global/backendBuckets/static')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(router.state.location.pathname).toBe('/lb/global/urlMaps/web-map'))
  expect(requests).toEqual([
    {
      method: 'PATCH',
      path: `/compute/v1/projects/${P}/global/urlMaps/web-map`,
      body: {
        pathMatchers: [
          {
            name: 'app',
            defaultService: link('global/backendBuckets/static'),
            pathRules: [
              { paths: ['/api/*'], service: link('global/backendServices/api') },
              { paths: ['/img/*'], service: 'global/backendBuckets/static' },
            ],
            routeRules: [],
          },
        ],
        fingerprint: 'fp-web-map',
      },
    },
  ])
})

it('creates a health check, showing the API’s validation messages', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/lb/create/healthChecks?project=${P}`)
  const name = await screen.findByLabelText('Name')
  await user.type(name, 'Bad_Name')
  await user.clear(screen.getByLabelText('Healthy threshold'))
  await user.type(screen.getByLabelText('Healthy threshold'), '11')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(
    await screen.findByText(/Invalid value for field 'resource.name': 'Bad_Name'/),
  ).toBeVisible()
  expect(
    screen.getByText(
      "Invalid value for field 'resource.healthyThreshold': '11'. Thresholds must be between 1 and 10.",
    ),
  ).toBeVisible()
  expect(requests).toHaveLength(0)

  await user.clear(name)
  await user.type(name, 'tcp-hc')
  await user.clear(screen.getByLabelText('Healthy threshold'))
  await user.type(screen.getByLabelText('Healthy threshold'), '3')
  await user.selectOptions(screen.getByLabelText('Protocol'), 'TCP')
  expect(screen.queryByLabelText('Request path')).not.toBeInTheDocument()
  await user.clear(screen.getByLabelText('Port'))
  await user.type(screen.getByLabelText('Port'), '5432')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() => expect(router.state.location.pathname).toBe('/lb/global/healthChecks/tcp-hc'))
  expect(requests).toEqual([
    {
      method: 'POST',
      path: `/compute/v1/projects/${P}/global/healthChecks`,
      body: {
        name: 'tcp-hc',
        type: 'TCP',
        tcpHealthCheck: { port: 5432 },
        checkIntervalSec: 5,
        timeoutSec: 5,
        healthyThreshold: 3,
        unhealthyThreshold: 2,
      },
    },
  ])
})

it('creates a backend service across NEGs', async () => {
  const user = userEvent.setup()
  renderApp(`/lb/create/backendServices?project=${P}`)
  await user.type(await screen.findByLabelText('Name'), 'web')
  const backends = screen.getByLabelText('Backends')
  await waitFor(() => expect(within(backends).getAllByRole('option')).toHaveLength(2))
  await user.selectOptions(backends, ['neg2 (us-central1-a)'])
  await user.selectOptions(screen.getByLabelText('Health check'), 'hc')
  await user.click(screen.getByLabelText('Cloud CDN'))
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() => expect(requests).toHaveLength(1))
  expect(requests[0]!.body).toEqual({
    name: 'web',
    loadBalancingScheme: 'EXTERNAL_MANAGED',
    protocol: 'HTTP',
    backends: [
      {
        group: `projects/${P}/zones/us-central1-a/networkEndpointGroups/neg2`,
        balancingMode: 'RATE',
        maxRatePerEndpoint: 100,
      },
    ],
    healthChecks: [`projects/${P}/global/healthChecks/hc`],
    enableCdn: true,
  })
})

it('shows why the API refuses a delete', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/lb/global/urlMaps/web-map?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Delete' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete URL map' }),
  )
  expect(await screen.findByText(/is already being used by/)).toBeInTheDocument()
  expect(router.state.location.pathname).toBe('/lb/global/urlMaps/web-map')
})

it('deletes a resource and returns to its list', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/lb/global/backendBuckets/static?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Delete' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', {
      name: 'Delete backend bucket',
    }),
  )
  await waitFor(() =>
    expect(requests.at(-1)).toEqual({
      method: 'DELETE',
      path: `/compute/v1/projects/${P}/global/backendBuckets/static`,
      body: undefined,
    }),
  )
  await waitFor(() => expect(router.state.location.pathname).toBe('/lb/backendBuckets'))
})
