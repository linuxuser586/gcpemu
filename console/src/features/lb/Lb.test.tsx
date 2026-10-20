import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, describe, expect, it } from 'vitest'

import type { RequestEntry } from '@/api/admin'
import { appendRequest } from '@/api/events'
import type { RestCall } from '@/lib/copyAs'
import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import type { Coll, LbListener, LbRoute, LbTrace, Ref, Resource } from './api'
import { recipeFor } from './copy'

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
    targetSslProxies: [],
    targetTcpProxies: [],
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
    http.get(`${API}/global/:coll`, ({ params }) =>
      HttpResponse.json({ items: state[params.coll as string] ?? [] }),
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

const CM = `//certificatemanager.googleapis.com/projects/${P}/locations/global/certificateMaps`

describe('certificate maps', () => {
  beforeEach(() => {
    fake.info.services = ['iam', 'compute', 'lb', 'certs']
    server.use(
      http.get(`*/certificatemanager/v1/projects/${P}/locations/global/certificateMaps`, () =>
        HttpResponse.json({
          certificateMaps: [
            { name: `projects/${P}/locations/global/certificateMaps/main` },
            { name: `projects/${P}/locations/global/certificateMaps/spare` },
          ],
        }),
      ),
    )
  })

  it('picks a global target HTTPS proxy’s certificate map from the Project’s maps', async () => {
    const user = userEvent.setup()
    const { router } = renderApp(`/lb/create/targetHttpsProxies?project=${P}`)
    await user.type(await screen.findByLabelText('Name'), 'web-https')
    await user.selectOptions(
      screen.getByLabelText('URL map'),
      `projects/${P}/global/urlMaps/web-map`,
    )
    const picker = screen.getByLabelText('Certificate map')
    await waitFor(() => expect(within(picker).getAllByRole('option')).toHaveLength(3))
    await user.selectOptions(picker, 'main')
    await user.click(screen.getByRole('button', { name: 'Create' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/lb/global/targetHttpsProxies/web-https'),
    )
    expect(requests[0]!.body).toEqual({
      name: 'web-https',
      urlMap: `projects/${P}/global/urlMaps/web-map`,
      certificateMap: `${CM}/main`,
    })

    // Certificate maps serve global target proxies only.
    await act(() => router.navigate(`/lb/create/targetHttpsProxies?project=${P}`))
    await user.type(await screen.findByLabelText('Region'), 'us-east1')
    expect(screen.queryByLabelText('Certificate map')).not.toBeInTheDocument()
  })

  it('links a target HTTPS proxy to its certificate map, and keeps one from another Project', async () => {
    state.targetHttpsProxies = [
      res('targetHttpsProxies', 'web-https', {
        urlMap: link('global/urlMaps/web-map'),
        certificateMap: `${CM}/main`,
      }),
      res('targetHttpsProxies', 'shared', {
        urlMap: link('global/urlMaps/web-map'),
        certificateMap:
          '//certificatemanager.googleapis.com/projects/other/locations/global/certificateMaps/shared',
      }),
    ]
    const user = userEvent.setup()
    const { router } = renderApp(`/lb/global/targetHttpsProxies/web-https?project=${P}`)
    const details = await screen.findByLabelText('target HTTPS proxy details')
    await user.click(within(details).getByRole('link', { name: 'main' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/certs/locations/global/certificateMaps/main'),
    )

    await act(() => router.navigate(`/lb/global/targetHttpsProxies/shared/edit?project=${P}`))
    const picker = await screen.findByLabelText('Certificate map')
    await waitFor(() =>
      expect(picker).toHaveValue(
        '//certificatemanager.googleapis.com/projects/other/locations/global/certificateMaps/shared',
      ),
    )
    expect(within(picker).getByRole('option', { name: 'other/shared' })).toBeInTheDocument()
  })
})

describe('target SSL proxies', () => {
  const RECORDED =
    'Recorded: stored and returned. A forwarding rule that targets this proxy carries no Data plane traffic; the emulator’s load balancer proxies HTTP and HTTPS only.'
  const proxy = (over: Partial<Resource> = {}) =>
    res('targetSslProxies', 'ssl', {
      service: link('global/backendServices/tcp'),
      sslCertificates: [link('global/sslCertificates/managed')],
      proxyHeader: 'NONE',
      ...over,
    })
  /** failing is the set* method the fake refuses. */
  let failing: string

  beforeEach(() => {
    failing = ''
    fake.info.services = ['iam', 'compute', 'lb', 'certs']
    state.backendServices!.push(
      res('backendServices', 'tcp', { protocol: 'TCP' }),
      res('backendServices', 'tls', { protocol: 'SSL' }),
    )
    server.use(
      http.get(`*/certificatemanager/v1/projects/${P}/locations/global/certificateMaps`, () =>
        HttpResponse.json({
          certificateMaps: [{ name: `projects/${P}/locations/global/certificateMaps/main` }],
        }),
      ),
      http.post(`${API}/global/targetSslProxies/:name/:method`, async ({ request, params }) => {
        const body = await record(request)
        if (params.method === failing) {
          return HttpResponse.json(
            { error: { code: 400, message: `${failing} refused` } },
            { status: 400 },
          )
        }
        // The fake applies the call, so that a reload shows it.
        state.targetSslProxies = state.targetSslProxies!.map((p) =>
          p.name === params.name ? { ...p, ...body } : p,
        )
        return HttpResponse.json(op)
      }),
    )
  })

  it('lists the Project’s global target SSL proxies, with the Recorded note', async () => {
    state.targetSslProxies = [proxy()]
    renderApp(`/lb/targetSslProxies?project=${P}`)
    const table = await screen.findByRole('table', { name: 'SSL proxies' })
    const [row] = within(table).getAllByTestId('resource')
    expect(row).toHaveTextContent(/ssl.*global.*tcp.*managed.*NONE/)
    expect(screen.getByRole('note')).toHaveTextContent(RECORDED)
    expect(screen.queryByLabelText('Scope')).not.toBeInTheDocument()
  })

  it('creates one with a certificate map, offering TCP and SSL backend services only', async () => {
    const user = userEvent.setup()
    const { router } = renderApp(`/lb/create/targetSslProxies?project=${P}`)
    await user.type(await screen.findByLabelText('Name'), 'ssl')
    expect(screen.getByRole('note')).toHaveTextContent(RECORDED)
    expect(screen.queryByLabelText('Region')).not.toBeInTheDocument()
    const service = screen.getByLabelText('Backend service')
    await waitFor(() => expect(within(service).getAllByRole('option')).toHaveLength(3))
    expect(within(service).queryByRole('option', { name: 'api' })).not.toBeInTheDocument()
    await user.selectOptions(service, 'tcp')

    await user.click(screen.getByRole('button', { name: 'Create' }))
    expect(
      await screen.findByText(/At least one SSL certificate or a certificate map/),
    ).toBeVisible()
    expect(requests).toHaveLength(0)

    const picker = screen.getByLabelText('Certificate map')
    await waitFor(() => expect(within(picker).getAllByRole('option')).toHaveLength(2))
    await user.selectOptions(picker, 'main')
    await user.click(screen.getByRole('button', { name: 'Create' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/lb/global/targetSslProxies/ssl'),
    )
    expect(requests).toEqual([
      {
        method: 'POST',
        path: `/compute/v1/projects/${P}/global/targetSslProxies`,
        body: {
          name: 'ssl',
          service: `projects/${P}/global/backendServices/tcp`,
          certificateMap: `${CM}/main`,
          proxyHeader: 'NONE',
        },
      },
    ])
  })

  it('links its backend service, certificates, SSL policy and certificate map', async () => {
    state.targetSslProxies = [
      proxy({ sslPolicy: link('global/sslPolicies/modern'), certificateMap: `${CM}/main` }),
    ]
    renderApp(`/lb/global/targetSslProxies/ssl?project=${P}`)
    const details = await screen.findByLabelText('target SSL proxy details')
    expect(screen.getByRole('note')).toHaveTextContent(RECORDED)
    for (const [name, href] of [
      ['tcp', `/lb/global/backendServices/tcp?project=${P}`],
      ['managed', `/lb/global/sslCertificates/managed?project=${P}`],
      ['modern', `/lb/global/sslPolicies/modern?project=${P}`],
      ['main', `/certs/locations/global/certificateMaps/main?project=${P}`],
    ]) {
      expect(within(details).getByRole('link', { name })).toHaveAttribute('href', href)
    }
  })

  it('edits through set* methods, setting the map before clearing the certificates', async () => {
    state.targetSslProxies = [proxy({ description: 'kept' })]
    const user = userEvent.setup()
    const { router } = renderApp(`/lb/global/targetSslProxies/ssl/edit?project=${P}`)
    const picker = await screen.findByLabelText('Certificate map')
    expect(screen.getByRole('note')).toHaveTextContent(RECORDED)
    expect(screen.queryByLabelText('Name')).not.toBeInTheDocument()
    expect(screen.queryByLabelText('Description')).not.toBeInTheDocument()
    await waitFor(() => expect(within(picker).getAllByRole('option')).toHaveLength(2))
    await user.selectOptions(picker, 'main')
    await user.deselectOptions(screen.getByLabelText('SSL certificates'), 'managed')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/lb/global/targetSslProxies/ssl'),
    )
    const base = `/compute/v1/projects/${P}/global/targetSslProxies/ssl`
    expect(requests).toEqual([
      { method: 'POST', path: `${base}/setCertificateMap`, body: { certificateMap: `${CM}/main` } },
      { method: 'POST', path: `${base}/setSslCertificates`, body: { sslCertificates: [] } },
    ])
  })

  it('stops at a failed set* call, shows its error and reloads the proxy', async () => {
    state.targetSslProxies = [proxy()]
    failing = 'setCertificateMap'
    const user = userEvent.setup()
    const { router } = renderApp(`/lb/global/targetSslProxies/ssl/edit?project=${P}`)
    const picker = await screen.findByLabelText('Certificate map')
    await waitFor(() => expect(within(picker).getAllByRole('option')).toHaveLength(2))
    await user.selectOptions(screen.getByLabelText('Proxy header'), 'PROXY_V1')
    await user.selectOptions(picker, 'main')
    await user.deselectOptions(screen.getByLabelText('SSL certificates'), 'managed')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    expect(await screen.findByText('setCertificateMap refused')).toBeVisible()
    expect(requests.map((r) => r.path.split('/').pop())).toEqual([
      'setProxyHeader',
      'setCertificateMap',
    ])
    // The form shows what was applied: the proxy header, not the map.
    await waitFor(() => expect(screen.getByLabelText('Certificate map')).toHaveValue(''))
    expect(screen.getByLabelText('Proxy header')).toHaveValue('PROXY_V1')
    expect(router.state.location.pathname).toBe('/lb/global/targetSslProxies/ssl/edit')
  })

  it('is a target of global forwarding rules only, whose page shows the Recorded note', async () => {
    state.targetSslProxies = [proxy()]
    state.forwardingRules!.push(
      res('forwardingRules', 'tls', {
        IPAddress: '34.120.0.2',
        portRange: '443-443',
        target: link('global/targetSslProxies/ssl'),
      }),
    )
    const user = userEvent.setup()
    const { router } = renderApp(`/lb/create/forwardingRules?project=${P}`)
    const target = await screen.findByLabelText('Target proxy')
    expect(
      await within(target).findByRole('option', { name: 'ssl (target SSL proxy)' }),
    ).toBeInTheDocument()
    await user.type(screen.getByLabelText('Region'), 'us-east1')
    expect(
      within(screen.getByLabelText('Target proxy')).queryByRole('option', {
        name: 'ssl (target SSL proxy)',
      }),
    ).not.toBeInTheDocument()

    await act(() => router.navigate(`/lb/global/forwardingRules/tls?project=${P}`))
    expect(await screen.findByRole('note')).toHaveTextContent(RECORDED)
    await act(() => router.navigate(`/lb/global/forwardingRules/web?project=${P}`))
    await screen.findByRole('region', { name: 'Local listener' })
    expect(screen.queryByRole('note')).not.toBeInTheDocument()
  })
})

describe('target TCP proxies', () => {
  const RECORDED =
    'Recorded: stored and returned. A forwarding rule that targets this proxy carries no Data plane traffic; the emulator’s load balancer proxies HTTP and HTTPS only.'
  /** regionalRes is a resource in region, or a global one for "". */
  const regionalRes = (coll: string, name: string, region: string, over: Partial<Resource>) =>
    res(coll, name, {
      ...(region && {
        selfLink: link(`regions/${region}/${coll}/${name}`),
        region: link(`regions/${region}`),
      }),
      ...over,
    })
  const tcp = (name: string, region = '', over: Partial<Resource> = {}) =>
    regionalRes('targetTcpProxies', name, region, {
      service: link(`${region ? `regions/${region}` : 'global'}/backendServices/tcp`),
      proxyHeader: 'NONE',
      ...over,
    })

  beforeEach(() => {
    state.backendServices!.push(
      res('backendServices', 'tcp', { protocol: 'TCP' }),
      res('backendServices', 'tls', { protocol: 'SSL' }),
      regionalRes('backendServices', 'tcp', 'us-east1', { protocol: 'TCP' }),
      regionalRes('backendServices', 'east-http', 'us-east1', { protocol: 'HTTP' }),
      regionalRes('backendServices', 'west-tcp', 'us-west1', { protocol: 'TCP' }),
    )
    server.use(
      http.get(`${API}/regions/:region/:coll/:name`, ({ params }) => {
        const r = state[params.coll as string]?.find((x) => x.name === params.name)
        return r ? HttpResponse.json(r) : notFound(params.name as string)
      }),
      http.post(`${API}/regions/:region/:coll`, async ({ request }) => {
        await record(request)
        return HttpResponse.json(op)
      }),
      http.post(`${API}/global/targetTcpProxies/:name/:method`, async ({ request }) => {
        await record(request)
        return HttpResponse.json(op)
      }),
    )
  })

  it('lists global and regional target TCP proxies, with the Recorded note', async () => {
    state.targetTcpProxies = [tcp('tcp'), tcp('east', 'us-east1', { proxyHeader: 'PROXY_V1' })]
    renderApp(`/lb/targetTcpProxies?project=${P}`)
    const table = await screen.findByRole('table', { name: 'TCP proxies' })
    const rows = within(table).getAllByTestId('resource')
    expect(rows).toHaveLength(2)
    expect(rows.find((r) => r.textContent?.includes('east'))).toHaveTextContent(
      /east.*us-east1.*tcp.*PROXY_V1/,
    )
    expect(screen.getByRole('note')).toHaveTextContent(RECORDED)
  })

  it('creates a global one, offering global TCP and SSL backend services only', async () => {
    const user = userEvent.setup()
    const { router } = renderApp(`/lb/create/targetTcpProxies?project=${P}`)
    await user.type(await screen.findByLabelText('Name'), 'tcp')
    expect(screen.getByRole('note')).toHaveTextContent(RECORDED)
    const service = screen.getByLabelText('Backend service')
    await waitFor(() => expect(within(service).getAllByRole('option')).toHaveLength(3))
    expect(within(service).getByRole('option', { name: 'tls' })).toBeInTheDocument()
    await user.selectOptions(service, 'tcp')
    await user.type(screen.getByLabelText('Description'), 'raw')
    await user.click(screen.getByRole('button', { name: 'Create' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/lb/global/targetTcpProxies/tcp'),
    )
    expect(requests).toEqual([
      {
        method: 'POST',
        path: `/compute/v1/projects/${P}/global/targetTcpProxies`,
        body: {
          name: 'tcp',
          description: 'raw',
          service: `projects/${P}/global/backendServices/tcp`,
          proxyHeader: 'NONE',
        },
      },
    ])
  })

  it('creates a regional one, offering TCP and SSL backend services of its region only', async () => {
    const user = userEvent.setup()
    const { router } = renderApp(`/lb/create/targetTcpProxies?project=${P}`)
    await user.type(await screen.findByLabelText('Name'), 'east')
    await user.type(screen.getByLabelText('Region'), 'us-east1')
    const service = screen.getByLabelText('Backend service')
    await waitFor(() =>
      expect(
        within(service)
          .getAllByRole('option')
          .map((o) => o.textContent),
      ).toEqual([expect.anything(), 'tcp']),
    )
    await user.selectOptions(service, 'tcp')
    await user.selectOptions(screen.getByLabelText('Proxy header'), 'PROXY_V1')
    await user.click(screen.getByRole('button', { name: 'Create' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/lb/regions/us-east1/targetTcpProxies/east'),
    )
    expect(requests).toEqual([
      {
        method: 'POST',
        path: `/compute/v1/projects/${P}/regions/us-east1/targetTcpProxies`,
        body: {
          name: 'east',
          service: `projects/${P}/regions/us-east1/backendServices/tcp`,
          proxyHeader: 'PROXY_V1',
        },
      },
    ])
  })

  it('edits a global one through setBackendService and setProxyHeader', async () => {
    state.targetTcpProxies = [tcp('tcp', '', { description: 'kept' })]
    const user = userEvent.setup()
    const { router } = renderApp(`/lb/global/targetTcpProxies/tcp?project=${P}`)
    const details = await screen.findByLabelText('target TCP proxy details')
    expect(screen.getByRole('note')).toHaveTextContent(RECORDED)
    expect(within(details).getByRole('link', { name: 'tcp' })).toHaveAttribute(
      'href',
      `/lb/global/backendServices/tcp?project=${P}`,
    )
    await user.click(screen.getByRole('link', { name: 'Edit' }))
    const service = await screen.findByLabelText('Backend service')
    expect(screen.getByRole('note')).toHaveTextContent(RECORDED)
    expect(screen.queryByLabelText('Description')).not.toBeInTheDocument()
    await waitFor(() => expect(within(service).getAllByRole('option')).toHaveLength(3))

    // An unchanged form sends nothing.
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/lb/global/targetTcpProxies/tcp'),
    )
    expect(requests).toEqual([])

    await user.click(await screen.findByRole('link', { name: 'Edit' }))
    await user.selectOptions(await screen.findByLabelText('Proxy header'), 'PROXY_V1')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/lb/global/targetTcpProxies/tcp'),
    )
    const base = `/compute/v1/projects/${P}/global/targetTcpProxies/tcp`
    expect(requests).toEqual([
      { method: 'POST', path: `${base}/setProxyHeader`, body: { proxyHeader: 'PROXY_V1' } },
    ])

    requests = []
    await user.click(await screen.findByRole('link', { name: 'Edit' }))
    const picker = await screen.findByLabelText('Backend service')
    await waitFor(() => expect(within(picker).getAllByRole('option')).toHaveLength(3))
    await user.selectOptions(picker, 'tls')
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/lb/global/targetTcpProxies/tcp'),
    )
    expect(requests).toEqual([
      {
        method: 'POST',
        path: `${base}/setBackendService`,
        body: { service: `projects/${P}/global/backendServices/tls` },
      },
    ])
  })

  it('has no Edit for a regional one, whose API has no set* methods', async () => {
    state.targetTcpProxies = [tcp('east', 'us-east1')]
    const { router } = renderApp(`/lb/regions/us-east1/targetTcpProxies/east?project=${P}`)
    const details = await screen.findByLabelText('target TCP proxy details')
    expect(screen.getByRole('note')).toHaveTextContent(RECORDED)
    expect(within(details).getByRole('link', { name: 'tcp' })).toHaveAttribute(
      'href',
      `/lb/regions/us-east1/backendServices/tcp?project=${P}`,
    )
    expect(screen.getByRole('button', { name: 'Delete' })).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'Edit' })).not.toBeInTheDocument()

    await act(() => router.navigate(`/lb/regions/us-east1/targetTcpProxies/east/edit?project=${P}`))
    expect(
      await screen.findByText(/A regional target TCP proxy cannot be changed/),
    ).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Save' })).not.toBeInTheDocument()
  })

  it('is a target of forwarding rules in its scope, whose page shows the Recorded note', async () => {
    state.targetTcpProxies = [tcp('tcp'), tcp('east', 'us-east1')]
    state.forwardingRules!.push(
      regionalRes('forwardingRules', 'east-tcp', 'us-east1', {
        IPAddress: '10.0.0.10',
        portRange: '5432-5432',
        loadBalancingScheme: 'INTERNAL_MANAGED',
        target: link('regions/us-east1/targetTcpProxies/east'),
      }),
    )
    const user = userEvent.setup()
    const { router } = renderApp(`/lb/create/forwardingRules?project=${P}`)
    const target = await screen.findByLabelText('Target proxy')
    expect(
      await within(target).findByRole('option', { name: 'tcp (target TCP proxy)' }),
    ).toBeInTheDocument()
    expect(
      within(target).queryByRole('option', { name: 'east (target TCP proxy)' }),
    ).not.toBeInTheDocument()
    await user.type(screen.getByLabelText('Region'), 'us-east1')
    const regionalTarget = screen.getByLabelText('Target proxy')
    expect(
      await within(regionalTarget).findByRole('option', { name: 'east (target TCP proxy)' }),
    ).toBeInTheDocument()
    expect(
      within(regionalTarget).queryByRole('option', { name: 'tcp (target TCP proxy)' }),
    ).not.toBeInTheDocument()

    await act(() => router.navigate(`/lb/regions/us-east1/forwardingRules/east-tcp?project=${P}`))
    expect(await screen.findByRole('note')).toHaveTextContent(RECORDED)
  })
})

describe('server TLS policies', () => {
  const NS = `projects/${P}/locations/global/serverTlsPolicies`
  beforeEach(() => {
    fake.info.services = ['iam', 'compute', 'lb', 'certs']
    server.use(
      http.get(`*/networksecurity/v1/projects/${P}/locations/-/serverTlsPolicies`, () =>
        HttpResponse.json({
          serverTlsPolicies: [
            { name: `${NS}/mtls` },
            { name: `projects/${P}/locations/us-east1/serverTlsPolicies/regional` },
          ],
        }),
      ),
    )
  })

  it('picks a target HTTPS proxy’s server TLS policy from those of its location', async () => {
    const user = userEvent.setup()
    const { router } = renderApp(`/lb/create/targetHttpsProxies?project=${P}`)
    await user.type(await screen.findByLabelText('Name'), 'web-https')
    await user.selectOptions(
      screen.getByLabelText('URL map'),
      `projects/${P}/global/urlMaps/web-map`,
    )
    const picker = screen.getByLabelText('Server TLS policy')
    await waitFor(() => expect(within(picker).getAllByRole('option')).toHaveLength(2))
    await user.selectOptions(picker, 'mtls')
    await user.click(screen.getByRole('button', { name: 'Create' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/lb/global/targetHttpsProxies/web-https'),
    )
    expect(requests[0]!.body).toEqual({
      name: 'web-https',
      urlMap: `projects/${P}/global/urlMaps/web-map`,
      serverTlsPolicy: `${NS}/mtls`,
    })
  })

  it('links a target HTTPS proxy to its server TLS policy', async () => {
    state.targetHttpsProxies = [
      res('targetHttpsProxies', 'web-https', {
        urlMap: link('global/urlMaps/web-map'),
        serverTlsPolicy: `//networksecurity.googleapis.com/${NS}/mtls`,
      }),
    ]
    const user = userEvent.setup()
    const { router } = renderApp(`/lb/global/targetHttpsProxies/web-https?project=${P}`)
    const details = await screen.findByLabelText('target HTTPS proxy details')
    await user.click(within(details).getByRole('link', { name: 'mtls' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/certs/locations/global/serverTlsPolicies/mtls'),
    )
  })
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

const traced = (path: string, over: Partial<RequestEntry> = {}, lb: Partial<LbTrace> = {}) => ({
  time: '2026-10-11T12:00:00.123Z',
  service: 'lb',
  protocol: 'http',
  method: `GET app.example.test${path}`,
  resource: `projects/${P}/global/forwardingRules/web`,
  status: 200,
  latencyMs: 2.5,
  ...over,
  lb: {
    host: 'app.example.test',
    path,
    targetProxy: `projects/${P}/global/targetHttpProxies/web-proxy`,
    urlMap: `projects/${P}/global/urlMaps/web-map`,
    match: 'pathRule' as const,
    pathMatcher: 'app',
    paths: ['/api/*'],
    backend: `projects/${P}/global/backendServices/api`,
    endpoint: '10.0.0.5:8000',
    cacheStatus: 'disabled' as const,
    statusDetails: 'response_sent_by_backend',
    backendLatencyMs: 1.5,
    ...lb,
  },
})

it('traces each request live, newest first, filtered by host and path', async () => {
  const user = userEvent.setup()
  server.use(
    http.get('*/_emu/v1/requests', ({ request }) => {
      expect(new URL(request.url).searchParams.get('service')).toBe('lb')
      return HttpResponse.json({
        requests: [
          traced('/api/users'),
          traced(
            '/app.css',
            {},
            {
              match: 'pathMatcherDefault',
              paths: undefined,
              backend: `projects/${P}/global/backendBuckets/static`,
              endpoint: 'storage.googleapis.com/assets',
              cacheStatus: 'hit',
            },
          ),
          // Another Project's load balancer.
          traced('/other', { resource: 'projects/other/global/forwardingRules/fr' }),
        ],
      })
    }),
  )
  const { router, queryClient } = renderApp(`/lb/traffic?project=${P}`)
  await screen.findByRole('table', { name: 'Requests' })
  expect(screen.getByRole('link', { name: 'Traffic' })).toHaveAttribute('aria-current', 'page')
  const rows = () => screen.getAllByTestId('request')
  expect(rows()).toHaveLength(2)
  const [css, api] = rows()
  expect(css).toHaveTextContent('GET app.example.test/app.css')
  expect(css).toHaveTextContent('Default of app')
  expect(css).toHaveTextContent('hit')
  expect(within(css!).getByRole('link', { name: 'static' })).toHaveAttribute(
    'href',
    `/lb/global/backendBuckets/static?project=${P}`,
  )
  expect(api).toHaveTextContent('Path rule /api/*')
  expect(api).toHaveTextContent('10.0.0.5:8000')
  expect(api).toHaveTextContent('2.5 ms')
  expect(within(api!).getByRole('link', { name: 'web' })).toHaveAttribute(
    'href',
    `/lb/global/forwardingRules/web?project=${P}`,
  )

  // A new entry arrives on the events stream.
  act(() =>
    appendRequest(
      queryClient,
      traced(
        '/old',
        { status: 302 },
        {
          backend: undefined,
          endpoint: undefined,
          cacheStatus: undefined,
          redirect: 'https://app.example.test/new',
          paths: ['/old'],
        },
      ),
    ),
  )
  await waitFor(() => expect(rows()).toHaveLength(3))
  expect(rows()[0]).toHaveTextContent('→ https://app.example.test/new')
  expect(rows()[0]).toHaveTextContent('302')

  // Details show the whole trace.
  await user.click(within(rows()[2]!).getByRole('button', { name: /Details of GET/ }))
  const details = screen.getByLabelText('Trace of GET app.example.test/api/users')
  expect(details).toHaveTextContent('response_sent_by_backend')
  expect(within(details).getByRole('link', { name: 'web-map' })).toBeVisible()
  expect(details).toHaveTextContent('1.5 ms')

  // The filters are in the URL.
  await user.type(screen.getByLabelText('Path'), '/api')
  expect(rows()).toHaveLength(1)
  await waitFor(() => expect(router.state.location.search).toContain('path=%2Fapi'))
  await user.type(screen.getByLabelText('Host'), 'nope.test')
  expect(screen.getByText('No requests match the filters.')).toBeVisible()

  // Paused, new entries wait.
  await user.clear(screen.getByLabelText('Host'))
  await user.clear(screen.getByLabelText('Path'))
  await user.click(screen.getByRole('button', { name: 'Pause' }))
  act(() => appendRequest(queryClient, traced('/api/late')))
  expect(rows()).toHaveLength(3)
  await user.click(screen.getByRole('button', { name: 'Resume' }))
  expect(rows()).toHaveLength(4)
})

it('opens the traffic view from a filter in the URL', async () => {
  server.use(
    http.get('*/_emu/v1/requests', () =>
      HttpResponse.json({ requests: [traced('/api/users'), traced('/app.css')] }),
    ),
  )
  renderApp(`/lb/traffic?project=${P}&path=.css`)
  await screen.findByRole('table', { name: 'Requests' })
  expect(screen.getAllByTestId('request')).toHaveLength(1)
  expect(screen.getByLabelText('Path')).toHaveValue('.css')
})

it('says when a Project has no load-balanced traffic yet', async () => {
  server.use(http.get('*/_emu/v1/requests', () => HttpResponse.json({ requests: [] })))
  renderApp(`/lb/traffic?project=${P}`)
  expect(await screen.findByText(/No requests to this Project/)).toBeVisible()
})

it('copies a backend service as gcloud commands, a request and an OpenTofu block', async () => {
  const user = userEvent.setup()
  renderApp(`/lb/global/backendServices/api?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Copy backend service as' }))
  const dialog = await screen.findByRole('dialog', { name: 'Copy backend service as' })
  const gcloud = [
    'gcloud compute backend-services create api \\',
    `    --project=${P} \\`,
    '    --global \\',
    '    --load-balancing-scheme=EXTERNAL_MANAGED \\',
    '    --protocol=HTTP \\',
    '    --health-checks=hc',
    '',
    'gcloud compute backend-services add-backend api \\',
    `    --project=${P} \\`,
    '    --global \\',
    '    --network-endpoint-group=neg \\',
    '    --network-endpoint-group-zone=us-central1-a \\',
    '    --balancing-mode=RATE',
  ].join('\n')
  expect(within(dialog).getByLabelText('gcloud for the backend service')).toHaveTextContent(
    gcloud,
    { normalizeWhitespace: false },
  )

  await user.click(within(dialog).getByRole('tab', { name: 'curl' }))
  const curl = within(dialog).getByLabelText('curl for the backend service')
  expect(curl).toHaveTextContent(
    `curl ${location.origin}/compute/v1/projects/${P}/global/backendServices \\ -X POST`,
  )
  expect(curl).toHaveTextContent(`"healthChecks": [ "projects/${P}/global/healthChecks/hc" ]`)
  expect(curl).not.toHaveTextContent('selfLink')
  expect(curl).not.toHaveTextContent('fingerprint')

  await user.click(within(dialog).getByRole('tab', { name: 'OpenTofu' }))
  const tofu = within(dialog).getByLabelText('OpenTofu for the backend service')
  expect(tofu).toHaveTextContent('resource "google_compute_backend_service" "api" {')
  expect(tofu).toHaveTextContent(`health_checks = ["projects/${P}/global/healthChecks/hc"]`)
  expect(tofu).toHaveTextContent(
    `backend { group = "projects/${P}/zones/us-central1-a/networkEndpointGroups/neg" balancing_mode = "RATE" }`,
  )
})

describe('Copy as', () => {
  const ref = (coll: Coll, region = ''): Ref => ({ project: P, region, coll, name: 'x' })
  const regional = (p: string) => link(`regions/us-east1/${p}`)

  it('copies a target HTTPS proxy with its server TLS policy', () => {
    const policy = `projects/${P}/locations/global/serverTlsPolicies/mtls`
    const r = recipeFor(
      ref('targetHttpsProxies'),
      res('targetHttpsProxies', 'web-https', {
        urlMap: link('global/urlMaps/web-map'),
        serverTlsPolicy: `//networksecurity.googleapis.com/${policy}`,
      }),
    )
    expect(r.gcloud).toEqual([expect.stringContaining(`--server-tls-policy=${policy}`)])
    expect(r.tofu).toContain(`server_tls_policy = "//networksecurity.googleapis.com/${policy}"`)
  })

  it('copies a target SSL proxy with its backend service, certificates and proxy header', () => {
    const r = recipeFor(
      ref('targetSslProxies'),
      res('targetSslProxies', 'ssl', {
        kind: 'compute#targetSslProxy',
        description: 'TLS to TCP',
        service: link('global/backendServices/tcp'),
        sslCertificates: [link('global/sslCertificates/a'), link('global/sslCertificates/b')],
        proxyHeader: 'PROXY_V1',
        sslPolicy: link('global/sslPolicies/modern'),
      }),
    )
    expect(r.gcloud).toEqual([
      [
        'gcloud compute target-ssl-proxies create ssl \\',
        `    --project=${P} \\`,
        '    --backend-service=tcp \\',
        '    --ssl-certificates=a,b \\',
        '    --ssl-policy=modern \\',
        '    --proxy-header=PROXY_V1 \\',
        "    --description='TLS to TCP'",
      ].join('\n'),
    ])
    expect(r.rest).toEqual({
      method: 'POST',
      path: `/compute/v1/projects/${P}/global/targetSslProxies`,
      body: {
        name: 'ssl',
        description: 'TLS to TCP',
        service: `projects/${P}/global/backendServices/tcp`,
        sslCertificates: [
          `projects/${P}/global/sslCertificates/a`,
          `projects/${P}/global/sslCertificates/b`,
        ],
        proxyHeader: 'PROXY_V1',
        sslPolicy: `projects/${P}/global/sslPolicies/modern`,
      },
    })
    expect(r.tofu).toContain('resource "google_compute_target_ssl_proxy" "ssl"')
    expect(r.tofu).toContain(`backend_service = "projects/${P}/global/backendServices/tcp"`)
    expect(r.tofu).toContain('proxy_header    = "PROXY_V1"')
  })

  it('copies global and regional target TCP proxies', () => {
    const global = recipeFor(
      ref('targetTcpProxies'),
      res('targetTcpProxies', 'tcp', {
        kind: 'compute#targetTcpProxy',
        description: 'raw TCP',
        service: link('global/backendServices/tcp'),
        proxyHeader: 'PROXY_V1',
      }),
    )
    expect(global.gcloud).toEqual([
      [
        'gcloud compute target-tcp-proxies create tcp \\',
        `    --project=${P} \\`,
        '    --global \\',
        '    --backend-service=tcp \\',
        '    --global-backend-service \\',
        '    --proxy-header=PROXY_V1 \\',
        "    --description='raw TCP'",
      ].join('\n'),
    ])
    expect(global.rest).toEqual({
      method: 'POST',
      path: `/compute/v1/projects/${P}/global/targetTcpProxies`,
      body: {
        name: 'tcp',
        description: 'raw TCP',
        service: `projects/${P}/global/backendServices/tcp`,
        proxyHeader: 'PROXY_V1',
      },
    })
    expect(global.tofu).toContain('resource "google_compute_target_tcp_proxy" "tcp" {')
    expect(global.tofu).toContain(`backend_service = "projects/${P}/global/backendServices/tcp"`)

    const east = recipeFor(
      ref('targetTcpProxies', 'us-east1'),
      res('targetTcpProxies', 'east', {
        selfLink: regional('targetTcpProxies/east'),
        region: link('regions/us-east1'),
        service: regional('backendServices/tcp'),
        proxyHeader: 'NONE',
      }),
    )
    expect(east.gcloud).toEqual([
      [
        'gcloud compute target-tcp-proxies create east \\',
        `    --project=${P} \\`,
        '    --region=us-east1 \\',
        '    --backend-service=tcp \\',
        '    --backend-service-region=us-east1',
      ].join('\n'),
    ])
    expect(east.rest).toMatchObject({
      path: `/compute/v1/projects/${P}/regions/us-east1/targetTcpProxies`,
    })
    expect(east.tofu).toContain('resource "google_compute_region_target_tcp_proxy" "east" {')
    expect(east.tofu).toContain('region          = "us-east1"')
  })

  it('copies a target SSL proxy that uses a certificate map', () => {
    const map = `projects/${P}/locations/global/certificateMaps/main`
    const r = recipeFor(
      ref('targetSslProxies'),
      res('targetSslProxies', 'ssl', {
        service: link('global/backendServices/tcp'),
        certificateMap: `//certificatemanager.googleapis.com/${map}`,
        proxyHeader: 'NONE',
      }),
    )
    expect(r.gcloud).toEqual([
      [
        'gcloud compute target-ssl-proxies create ssl \\',
        `    --project=${P} \\`,
        '    --backend-service=tcp \\',
        `    --certificate-map=${map}`,
      ].join('\n'),
    ])
  })

  it('copies a global forwarding rule with a reserved address and labels', () => {
    const r = recipeFor(
      ref('forwardingRules'),
      res('forwardingRules', 'web', {
        IPAddress: link('global/addresses/web-ip'),
        IPProtocol: 'TCP',
        portRange: '443-443',
        loadBalancingScheme: 'EXTERNAL_MANAGED',
        target: link('global/targetHttpsProxies/web-https'),
        labels: { env: 'dev' },
        labelFingerprint: 'lf',
      }),
    )
    expect(r.gcloud).toEqual([
      [
        'gcloud compute forwarding-rules create web \\',
        `    --project=${P} \\`,
        '    --global \\',
        '    --load-balancing-scheme=EXTERNAL_MANAGED \\',
        '    --address=web-ip \\',
        '    --ip-protocol=TCP \\',
        '    --ports=443 \\',
        '    --target-https-proxy=web-https',
      ].join('\n'),
      [
        'gcloud compute forwarding-rules update web \\',
        `    --project=${P} \\`,
        '    --global \\',
        '    --update-labels=env=dev',
      ].join('\n'),
    ])
    expect(r.rest).toEqual({
      method: 'POST',
      path: `/compute/v1/projects/${P}/global/forwardingRules`,
      body: {
        name: 'web',
        IPAddress: `projects/${P}/global/addresses/web-ip`,
        IPProtocol: 'TCP',
        portRange: '443-443',
        loadBalancingScheme: 'EXTERNAL_MANAGED',
        target: `projects/${P}/global/targetHttpsProxies/web-https`,
        labels: { env: 'dev' },
      },
    })
    expect(r.tofu).toBe(
      [
        'resource "google_compute_global_forwarding_rule" "web" {',
        `  project               = "${P}"`,
        '  name                  = "web"',
        `  ip_address            = "projects/${P}/global/addresses/web-ip"`,
        '  ip_protocol           = "TCP"',
        '  port_range            = "443"',
        '  load_balancing_scheme = "EXTERNAL_MANAGED"',
        `  target                = "projects/${P}/global/targetHttpsProxies/web-https"`,
        '  labels                = { env = "dev" }',
        '}',
      ].join('\n'),
    )
  })

  it('copies a regional internal forwarding rule', () => {
    const r = recipeFor(
      ref('forwardingRules', 'us-east1'),
      res('forwardingRules', 'internal', {
        selfLink: regional('forwardingRules/internal'),
        region: link('regions/us-east1'),
        IPAddress: '10.0.0.9',
        portRange: '80-80',
        loadBalancingScheme: 'INTERNAL_MANAGED',
        network: link('global/networks/vpc'),
        subnetwork: regional('subnetworks/sub'),
        target: regional('targetHttpProxies/int-proxy'),
      }),
    )
    expect(r.gcloud).toEqual([
      [
        'gcloud compute forwarding-rules create internal \\',
        `    --project=${P} \\`,
        '    --region=us-east1 \\',
        '    --load-balancing-scheme=INTERNAL_MANAGED \\',
        '    --address=10.0.0.9 \\',
        '    --ports=80 \\',
        '    --network=vpc \\',
        '    --subnet=sub \\',
        '    --target-http-proxy=int-proxy \\',
        '    --target-http-proxy-region=us-east1',
      ].join('\n'),
    ])
    expect((r.rest as RestCall).path).toBe(
      `/compute/v1/projects/${P}/regions/us-east1/forwardingRules`,
    )
    expect((r.rest as RestCall).body).not.toHaveProperty('region')
    expect(r.tofu).toContain('resource "google_compute_forwarding_rule" "internal" {')
    expect(r.tofu).toContain('  region                = "us-east1"')
    expect(r.tofu).toContain(
      `  subnetwork            = "projects/${P}/regions/us-east1/subnetworks/sub"`,
    )
  })

  it('copies target proxies with regional certificates and policy', () => {
    const http = recipeFor(
      ref('targetHttpProxies'),
      res('targetHttpProxies', 'web-proxy', { urlMap: link('global/urlMaps/web-map') }),
    )
    expect(http.gcloud).toEqual([
      [
        'gcloud compute target-http-proxies create web-proxy \\',
        `    --project=${P} \\`,
        '    --global \\',
        '    --url-map=web-map',
      ].join('\n'),
    ])
    expect(http.tofu).toContain('resource "google_compute_target_http_proxy" "web-proxy" {')
    expect(http.tofu).toContain(`  url_map = "projects/${P}/global/urlMaps/web-map"`)

    const https = recipeFor(
      ref('targetHttpsProxies', 'us-east1'),
      res('targetHttpsProxies', 'rhttps', {
        region: link('regions/us-east1'),
        urlMap: regional('urlMaps/rmap'),
        sslCertificates: [regional('sslCertificates/a'), regional('sslCertificates/b')],
        sslPolicy: regional('sslPolicies/strict'),
      }),
    )
    expect(https.gcloud).toEqual([
      [
        'gcloud compute target-https-proxies create rhttps \\',
        `    --project=${P} \\`,
        '    --region=us-east1 \\',
        '    --url-map=rmap \\',
        '    --url-map-region=us-east1 \\',
        '    --ssl-certificates=a,b \\',
        '    --ssl-certificates-region=us-east1 \\',
        '    --ssl-policy=strict \\',
        '    --ssl-policy-region=us-east1',
      ].join('\n'),
    ])
    expect(https.tofu).toContain('resource "google_compute_region_target_https_proxy" "rhttps" {')
    expect(https.tofu).toContain(`ssl_policy = "projects/${P}/regions/us-east1/sslPolicies/strict"`)
  })

  it('creates a plain URL map and imports one with routing', () => {
    const plain = recipeFor(
      ref('urlMaps'),
      res('urlMaps', 'grpc', { defaultService: link('global/backendBuckets/static') }),
    )
    expect(plain.gcloud).toEqual([
      [
        'gcloud compute url-maps create grpc \\',
        `    --project=${P} \\`,
        '    --global \\',
        '    --default-backend-bucket=static',
      ].join('\n'),
    ])

    const r = recipeFor(ref('urlMaps'), state.urlMaps![0]!)
    const body = {
      name: 'web-map',
      defaultService: `projects/${P}/global/backendBuckets/static`,
      hostRules: [{ hosts: ['app.example.test'], pathMatcher: 'app' }],
      pathMatchers: [
        {
          name: 'app',
          defaultService: `projects/${P}/global/backendBuckets/static`,
          pathRules: [{ paths: ['/api/*'], service: `projects/${P}/global/backendServices/api` }],
          routeRules: [],
        },
      ],
    }
    expect(r.gcloud).toEqual([
      [
        'gcloud compute url-maps import web-map \\',
        `    --project=${P} \\`,
        `    --global <<'EOF'`,
        JSON.stringify(body, null, 2),
        'EOF',
      ].join('\n'),
    ])
    expect((r.rest as RestCall).body).toEqual(body)
    expect(r.tofu).toBe(
      [
        'resource "google_compute_url_map" "web-map" {',
        `  project         = "${P}"`,
        '  name            = "web-map"',
        `  default_service = "projects/${P}/global/backendBuckets/static"`,
        '  host_rule {',
        '    hosts        = ["app.example.test"]',
        '    path_matcher = "app"',
        '  }',
        '  path_matcher {',
        '    name            = "app"',
        `    default_service = "projects/${P}/global/backendBuckets/static"`,
        '    path_rule {',
        '      paths   = ["/api/*"]',
        `      service = "projects/${P}/global/backendServices/api"`,
        '    }',
        '  }',
        '}',
      ].join('\n'),
    )
  })

  it('copies a regional backend service with CDN, headers, logging and a security policy', () => {
    const r = recipeFor(
      ref('backendServices', 'us-east1'),
      res('backendServices', 'rapi', {
        region: link('regions/us-east1'),
        loadBalancingScheme: 'INTERNAL_MANAGED',
        protocol: 'HTTP',
        timeoutSec: 30,
        healthChecks: [regional('healthChecks/rhc')],
        connectionDraining: { drainingTimeoutSec: 10 },
        enableCdn: true,
        cdnPolicy: {
          cacheMode: 'USE_ORIGIN_HEADERS',
          cacheKeyPolicy: { includeProtocol: true, includeHost: false, includeQueryString: true },
          signedUrlKeyNames: ['k'],
        },
        customResponseHeaders: ['X-Cache: {cdn_cache_status}'],
        logConfig: { enable: true, sampleRate: 0.5 },
        securityPolicy: link('global/securityPolicies/armor'),
        backends: [
          {
            group: regional('networkEndpointGroups/rneg'),
            balancingMode: 'RATE',
            maxRatePerEndpoint: 50,
            capacityScaler: 1,
          },
        ],
      }),
    )
    expect(r.gcloud).toEqual([
      [
        'gcloud compute backend-services create rapi \\',
        `    --project=${P} \\`,
        '    --region=us-east1 \\',
        '    --load-balancing-scheme=INTERNAL_MANAGED \\',
        '    --protocol=HTTP \\',
        '    --timeout=30 \\',
        '    --health-checks=rhc \\',
        '    --health-checks-region=us-east1 \\',
        '    --connection-draining-timeout=10 \\',
        '    --enable-cdn \\',
        '    --cache-mode=USE_ORIGIN_HEADERS \\',
        '    --cache-key-include-protocol \\',
        '    --no-cache-key-include-host \\',
        '    --cache-key-include-query-string \\',
        '    --enable-logging \\',
        '    --logging-sample-rate=0.5 \\',
        "    --custom-response-header='X-Cache: {cdn_cache_status}'",
      ].join('\n'),
      [
        'gcloud compute backend-services add-backend rapi \\',
        `    --project=${P} \\`,
        '    --region=us-east1 \\',
        '    --network-endpoint-group=rneg \\',
        '    --network-endpoint-group-region=us-east1 \\',
        '    --balancing-mode=RATE \\',
        '    --max-rate-per-endpoint=50 \\',
        '    --capacity-scaler=1',
      ].join('\n'),
      [
        'gcloud compute backend-services update rapi \\',
        `    --project=${P} \\`,
        '    --region=us-east1 \\',
        '    --security-policy=armor',
      ].join('\n'),
    ])
    const body = (r.rest as RestCall).body as Record<string, unknown>
    expect(body).not.toHaveProperty('securityPolicy')
    expect(body.cdnPolicy).not.toHaveProperty('signedUrlKeyNames')
    expect(r.tofu).toContain('resource "google_compute_region_backend_service" "rapi" {')
    expect(r.tofu).toContain(
      [
        '  cdn_policy {',
        '    cache_mode = "USE_ORIGIN_HEADERS"',
        '    cache_key_policy {',
        '      include_protocol     = true',
        '      include_host         = false',
        '      include_query_string = true',
        '    }',
        '  }',
      ].join('\n'),
    )
    expect(r.tofu).toContain('  log_config {\n    enable      = true\n    sample_rate = 0.5\n  }')
    expect(r.tofu).toContain('  backend {\n    group                 = ')
    expect(r.tofu).toContain('connection_draining_timeout_sec = 10')
    expect(r.tofu).toContain(
      `security_policy                 = "projects/${P}/global/securityPolicies/armor"`,
    )
    expect(r.tofu).not.toContain('connection_draining {')
  })

  it('copies a backend bucket with negative caching', () => {
    const r = recipeFor(
      ref('backendBuckets'),
      res('backendBuckets', 'static', {
        bucketName: 'assets',
        enableCdn: true,
        cdnPolicy: {
          cacheMode: 'CACHE_ALL_STATIC',
          defaultTtl: 3600,
          negativeCaching: true,
          negativeCachingPolicy: [{ code: 404, ttl: 60 }],
          bypassCacheOnRequestHeaders: [{ headerName: 'X-Debug' }],
        },
      }),
    )
    expect(r.gcloud).toEqual([
      [
        'gcloud compute backend-buckets create static \\',
        `    --project=${P} \\`,
        '    --gcs-bucket-name=assets \\',
        '    --enable-cdn \\',
        '    --cache-mode=CACHE_ALL_STATIC \\',
        '    --default-ttl=3600 \\',
        '    --negative-caching \\',
        '    --negative-caching-policy=404=60 \\',
        '    --bypass-cache-on-request-headers=X-Debug',
      ].join('\n'),
    ])
    expect(r.tofu).toContain('resource "google_compute_backend_bucket" "static" {')
    expect(r.tofu).toContain(
      '    negative_caching_policy {\n      code = 404\n      ttl  = 60\n    }',
    )
    expect(r.tofu).toContain(
      '    bypass_cache_on_request_headers {\n      header_name = "X-Debug"\n    }',
    )
  })

  it('copies health checks by protocol', () => {
    const http = recipeFor(ref('healthChecks'), {
      ...state.healthChecks![0]!,
      httpHealthCheck: { port: 8080, requestPath: '/healthz' },
      timeoutSec: 5,
      healthyThreshold: 2,
    })
    expect(http.gcloud).toEqual([
      [
        'gcloud compute health-checks create http hc \\',
        `    --project=${P} \\`,
        '    --global \\',
        '    --port=8080 \\',
        '    --request-path=/healthz \\',
        '    --check-interval=5 \\',
        '    --timeout=5 \\',
        '    --healthy-threshold=2',
      ].join('\n'),
    ])
    expect(http.tofu).toBe(
      [
        'resource "google_compute_health_check" "hc" {',
        `  project            = "${P}"`,
        '  name               = "hc"',
        '  check_interval_sec = 5',
        '  timeout_sec        = 5',
        '  healthy_threshold  = 2',
        '  http_health_check {',
        '    port         = 8080',
        '    request_path = "/healthz"',
        '  }',
        '}',
      ].join('\n'),
    )

    const tcp = recipeFor(
      ref('healthChecks', 'us-east1'),
      res('healthChecks', 'rtcp', {
        region: link('regions/us-east1'),
        type: 'TCP',
        tcpHealthCheck: { portSpecification: 'USE_SERVING_PORT' },
      }),
    )
    expect(tcp.gcloud).toEqual([
      [
        'gcloud compute health-checks create tcp rtcp \\',
        `    --project=${P} \\`,
        '    --region=us-east1 \\',
        '    --use-serving-port',
      ].join('\n'),
    ])
    expect(tcp.tofu).toContain('resource "google_compute_region_health_check" "rtcp" {')
    expect(tcp.tofu).toContain(
      '  tcp_health_check {\n    port_specification = "USE_SERVING_PORT"\n  }',
    )
    expect(tcp.tofu).not.toContain('type')
  })

  it('copies certificates, leaving out the private key the API does not return', () => {
    const pem = '-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n'
    const self = recipeFor(
      ref('sslCertificates', 'us-east1'),
      res('sslCertificates', 'rcert', {
        region: link('regions/us-east1'),
        type: 'SELF_MANAGED',
        certificate: pem,
        expireTime: '2027-01-01T00:00:00Z',
        subjectAlternativeNames: ['a.example.test'],
      }),
    )
    expect(self.gcloud).toEqual([
      `cat > certificate.pem <<'EOF'\n${pem}EOF`,
      [
        '# The API does not return the private key: save it as private-key.pem.',
        'gcloud compute ssl-certificates create rcert \\',
        `    --project=${P} \\`,
        '    --region=us-east1 \\',
        '    --certificate=certificate.pem \\',
        '    --private-key=private-key.pem',
      ].join('\n'),
    ])
    expect((self.rest as RestCall).body).toEqual({
      name: 'rcert',
      type: 'SELF_MANAGED',
      certificate: pem,
      privateKey: 'PRIVATE_KEY_PEM (the API does not return the private key)',
    })
    expect(self.tofu).toContain('resource "google_compute_region_ssl_certificate" "rcert" {')
    expect(self.tofu).toContain('  private_key = file("private-key.pem")')

    const managed = recipeFor(ref('sslCertificates'), state.sslCertificates![0]!)
    expect(managed.gcloud).toEqual([
      [
        'gcloud compute ssl-certificates create managed \\',
        `    --project=${P} \\`,
        '    --global \\',
        '    --domains=app.example.test',
      ].join('\n'),
    ])
    expect((managed.rest as RestCall).body).toEqual({
      name: 'managed',
      type: 'MANAGED',
      managed: { domains: ['app.example.test'] },
    })
    expect(managed.tofu).toBe(
      [
        'resource "google_compute_managed_ssl_certificate" "managed" {',
        `  project = "${P}"`,
        '  name    = "managed"',
        '  managed {',
        '    domains = ["app.example.test"]',
        '  }',
        '}',
      ].join('\n'),
    )
  })

  it('copies a regional custom SSL policy', () => {
    const r = recipeFor(
      ref('sslPolicies', 'us-east1'),
      res('sslPolicies', 'compat', {
        region: link('regions/us-east1'),
        profile: 'CUSTOM',
        minTlsVersion: 'TLS_1_2',
        customFeatures: ['TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256'],
        enabledFeatures: ['TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256'],
      }),
    )
    expect(r.gcloud).toEqual([
      [
        'gcloud compute ssl-policies create compat \\',
        `    --project=${P} \\`,
        '    --region=us-east1 \\',
        '    --profile=CUSTOM \\',
        '    --min-tls-version=1.2 \\',
        '    --custom-features=TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256',
      ].join('\n'),
    ])
    expect((r.rest as RestCall).body).not.toHaveProperty('enabledFeatures')
    expect(r.tofu).toContain('resource "google_compute_region_ssl_policy" "compat" {')
    expect(r.tofu).toContain('  min_tls_version = "TLS_1_2"')
  })
})
