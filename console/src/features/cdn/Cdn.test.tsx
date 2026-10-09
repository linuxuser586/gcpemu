import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, expect, it } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import type { Resource } from '../lb/api'
import type { CdnBackend, CdnEntry } from './api'

const P = 'alpha-project'
const API = `*/compute/v1/projects/${P}`
const link = (p: string) => `https://www.googleapis.com/compute/v1/projects/${P}/${p}`

/** state is the fake compute API's backends and URL maps, by collection. */
let state: Record<string, Resource[]>
let backends: CdnBackend[]
let entries: CdnEntry[]
/** requests records the writes the fakes received. */
let requests: { method: string; path: string; body: unknown }[] = []

async function record(request: Request) {
  const text = await request.text()
  requests.push({
    method: request.method,
    path: new URL(request.url).pathname,
    body: text ? (JSON.parse(text) as unknown) : undefined,
  })
}

const res = (coll: string, name: string, over: Partial<Resource> = {}): Resource => ({
  name,
  selfLink: link(`global/${coll}/${name}`),
  fingerprint: `fp-${name}`,
  ...over,
})

const op = { name: 'op-1', status: 'DONE', selfLink: link('global/operations/op-1') }

beforeEach(() => {
  requests = []
  state = {
    backendServices: [
      res('backendServices', 'api', {
        enableCdn: true,
        cdnPolicy: {
          cacheMode: 'CACHE_ALL_STATIC',
          defaultTtl: 3600,
          maxTtl: 86400,
          clientTtl: 3600,
          cacheKeyPolicy: { includeProtocol: true, includeHost: true, includeQueryString: true },
        },
      }),
      res('backendServices', 'plain', { protocol: 'HTTP' }),
    ],
    backendBuckets: [
      res('backendBuckets', 'static', {
        bucketName: 'assets',
        enableCdn: true,
        cdnPolicy: { cacheMode: 'FORCE_CACHE_ALL', defaultTtl: 60, clientTtl: 60 },
      }),
    ],
    urlMaps: [
      res('urlMaps', 'web-map', {
        defaultService: link('global/backendBuckets/static'),
        pathMatchers: [
          {
            name: 'app',
            defaultService: link('global/backendBuckets/static'),
            pathRules: [{ paths: ['/api/*'], service: link('global/backendServices/api') }],
          },
        ],
      }),
      res('urlMaps', 'other-map', { defaultService: link('global/backendServices/plain') }),
    ],
  }
  backends = [
    {
      backend: `projects/${P}/global/backendBuckets/static`,
      entries: 2,
      bytes: 3072,
      hits: 3,
      misses: 1,
      revalidated: 0,
      uncacheable: 0,
    },
  ]
  entries = [
    {
      host: 'app.example.test',
      path: '/logo.png',
      cacheKey: 'app.example.test/logo.png',
      status: 200,
      bytes: 2048,
      contentType: 'image/png',
      stored: '2026-10-09T10:00:00Z',
      age: 30,
      ttl: 60,
      tags: ['img'],
      tier: 'memory',
    },
    {
      host: 'app.example.test',
      path: '/old.css',
      cacheKey: 'app.example.test/old.css',
      status: 200,
      bytes: 1024,
      contentType: 'text/css',
      stored: '2026-10-09T09:00:00Z',
      age: 3600,
      ttl: 60,
      tier: 'disk',
    },
  ]
  fake.info.services = ['iam', 'compute', 'lb', 'cdn']
  server.use(
    http.get(`${API}/aggregated/:coll`, ({ params }) => {
      const coll = params.coll as string
      return HttpResponse.json({ items: { global: { [coll]: state[coll] ?? [] } } })
    }),
    http.get(`${API}/global/backendBuckets`, () =>
      HttpResponse.json({ items: state.backendBuckets }),
    ),
    http.get(`${API}/global/:coll/:name`, ({ params }) => {
      const r = state[params.coll as string]?.find((x) => x.name === params.name)
      return r
        ? HttpResponse.json(r)
        : HttpResponse.json({ error: { code: 404, message: 'not found' } }, { status: 404 })
    }),
    http.patch(`${API}/global/:coll/:name`, async ({ request }) => {
      await record(request)
      return HttpResponse.json(op)
    }),
    http.post(`${API}/global/urlMaps/:name/invalidateCache`, async ({ request }) => {
      await record(request)
      return HttpResponse.json(op)
    }),
    http.get('*/_emu/v1/cdn', () =>
      HttpResponse.json({ entries: 7, bytes: 5 << 20, limitBytes: 1 << 30, backends }),
    ),
    http.get('*/_emu/v1/cdn/entries', ({ request }) => {
      const b = new URL(request.url).searchParams.get('backend')
      return HttpResponse.json({
        entries: b === `projects/${P}/global/backendBuckets/static` ? entries : [],
        truncated: false,
      })
    }),
    http.post('*/_emu/v1/cdn/purge', async ({ request }) => {
      await record(request)
      return HttpResponse.json({ purged: 7 })
    }),
  )
})

it('says how to enable the Service when it is off', async () => {
  fake.info.services = ['iam', 'lb']
  renderApp(`/cdn?project=${P}`)
  expect(await screen.findByRole('heading', { name: /Service cdn is not enabled/ })).toBeVisible()
})

it('needs the load balancer, which serves the origins', async () => {
  fake.info.services = ['iam', 'cdn']
  renderApp(`/cdn?project=${P}`)
  expect(await screen.findByRole('heading', { name: /Service lb is not enabled/ })).toBeVisible()
})

it('asks for a Project first', async () => {
  renderApp('/cdn')
  expect(await screen.findByText(/Choose a Project/)).toBeInTheDocument()
})

it('lists origins with their cache usage and hit ratio', async () => {
  const user = userEvent.setup()
  renderApp(`/cdn?project=${P}`)
  await screen.findByRole('table', { name: 'Origins' })
  const rows = () => screen.getAllByTestId('origin')
  // A backend without Cloud CDN is not an origin.
  expect(rows()).toHaveLength(2)
  const st = rows().find((r) => within(r).queryByText('static'))!
  expect(st).toHaveTextContent('backend bucket')
  expect(st).toHaveTextContent('FORCE_CACHE_ALL')
  expect(st).toHaveTextContent('3.0 KiB')
  expect(st).toHaveTextContent('75.0%')
  expect(within(st).getByRole('link', { name: 'static' })).toHaveAttribute(
    'href',
    `/cdn/global/backendBuckets/static?project=${P}`,
  )
  const api = rows().find((r) => within(r).queryByText('api'))!
  expect(api).toHaveTextContent('—')

  const usage = screen.getByLabelText('Cache usage')
  expect(usage).toHaveTextContent('5.0 MiB of 1.0 GiB')
  expect(screen.getByRole('meter', { name: 'Cache size' })).toHaveAttribute(
    'aria-valuenow',
    String(5 << 20),
  )

  await user.type(screen.getByLabelText('Filter'), 'sta')
  expect(rows()).toHaveLength(1)
})

it('purges the whole cache', async () => {
  const user = userEvent.setup()
  renderApp(`/cdn?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Purge all' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Purge cache' }),
  )
  expect(await screen.findByText('Purged 7 cached entries.')).toBeVisible()
  expect(requests).toEqual([{ method: 'POST', path: '/_emu/v1/cdn/purge', body: undefined }])
})

it('invalidates a path through a URL map that routes to an origin', async () => {
  const user = userEvent.setup()
  renderApp(`/cdn?project=${P}`)
  const card = await screen.findByRole('region', { name: 'Invalidate cached content' })
  const map = await within(card).findByLabelText('URL map')
  // other-map routes only to a backend without Cloud CDN.
  expect(
    within(map)
      .getAllByRole('option')
      .map((o) => o.textContent),
  ).toEqual(['web-map'])

  const path = within(card).getByLabelText('Path')
  await user.clear(path)
  await user.type(path, '/img/*.png')
  await user.click(within(card).getByRole('button', { name: 'Invalidate' }))
  expect(
    await within(card).findByText(
      "Invalid value for field 'path': '/img/*.png'. A wildcard * is only allowed at the end of the path.",
    ),
  ).toBeVisible()
  expect(requests).toHaveLength(0)

  await user.clear(path)
  await user.type(path, '/img/*')
  await user.type(within(card).getByLabelText('Host'), 'app.example.test')
  await user.click(within(card).getByRole('button', { name: 'Invalidate' }))
  await waitFor(() =>
    expect(requests).toEqual([
      {
        method: 'POST',
        path: `/compute/v1/projects/${P}/global/urlMaps/web-map/invalidateCache`,
        body: { host: 'app.example.test', path: '/img/*' },
      },
    ]),
  )
})

it('shows an origin’s traffic, cache settings and cached entries', async () => {
  renderApp(`/cdn/global/backendBuckets/static?project=${P}`)
  const traffic = await screen.findByLabelText('Origin traffic')
  expect(traffic).toHaveTextContent('Hit ratio75.0%')
  expect(traffic).toHaveTextContent('Hits3')
  const settings = screen.getByLabelText('Origin cache settings')
  expect(settings).toHaveTextContent('FORCE_CACHE_ALL')
  expect(settings).toHaveTextContent('Default TTL1 min')
  const rows = await screen.findAllByTestId('entry')
  expect(rows).toHaveLength(2)
  expect(rows[0]).toHaveTextContent('app.example.test/logo.png')
  expect(rows[0]).toHaveTextContent('tags img')
  expect(rows[0]).toHaveTextContent('Fresh')
  expect(rows[1]).toHaveTextContent('Stale')
  expect(rows[1]).toHaveTextContent('disk')
  const inv = screen.getByRole('region', { name: 'Invalidate cached content' })
  expect(within(inv).getByLabelText('URL map')).toHaveValue(`projects/${P}/global/urlMaps/web-map`)
})

it('edits cache settings with a merge patch, showing the API’s messages', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/cdn/global/backendServices/api/edit?project=${P}`)
  const def = await screen.findByLabelText('Default TTL (seconds)')
  await user.clear(def)
  await user.type(def, '90000')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  expect(
    await screen.findByText(
      "Invalid value for field 'resource.cdnPolicy.defaultTtl': '90000'. Default TTL must be less than or equal to max TTL.",
    ),
  ).toBeVisible()
  expect(requests).toHaveLength(0)

  await user.selectOptions(screen.getByLabelText('Cache mode'), 'USE_ORIGIN_HEADERS')
  expect(screen.queryByLabelText('Default TTL (seconds)')).not.toBeInTheDocument()
  await user.click(screen.getByLabelText('Negative caching'))
  await user.type(screen.getByLabelText('Negative caching TTLs'), '404=60')
  await user.click(screen.getByLabelText('Cache key: host'))
  await user.type(screen.getByLabelText('Query parameters to include'), 'page')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/cdn/global/backendServices/api'),
  )
  expect(requests).toEqual([
    {
      method: 'PATCH',
      path: `/compute/v1/projects/${P}/global/backendServices/api`,
      body: {
        cdnPolicy: {
          cacheMode: 'USE_ORIGIN_HEADERS',
          defaultTtl: null,
          maxTtl: null,
          clientTtl: null,
          negativeCaching: true,
          negativeCachingPolicy: [{ code: 404, ttl: 60 }],
          cacheKeyPolicy: { includeHost: false, queryStringWhitelist: ['page'] },
        },
        fingerprint: 'fp-api',
      },
    },
  ])
})

it('adds a backend as an origin', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/cdn/add?project=${P}`)
  const backend = await screen.findByLabelText('Backend')
  // Only backends without Cloud CDN.
  await waitFor(() => expect(within(backend).getAllByRole('option')).toHaveLength(2))
  await user.selectOptions(backend, 'plain (backend service)')
  await user.type(await screen.findByLabelText('Serve while stale (seconds)'), '600')
  await user.click(screen.getByRole('button', { name: 'Add origin' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/cdn/global/backendServices/plain'),
  )
  expect(requests).toEqual([
    {
      method: 'PATCH',
      path: `/compute/v1/projects/${P}/global/backendServices/plain`,
      body: {
        enableCdn: true,
        cdnPolicy: {
          serveWhileStale: 600,
          cacheKeyPolicy: { includeProtocol: true, includeHost: true, includeQueryString: true },
        },
        fingerprint: 'fp-plain',
      },
    },
  ])
})

it('removes an origin by turning Cloud CDN off', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/cdn/global/backendBuckets/static?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Remove origin' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Remove origin' }),
  )
  await waitFor(() => expect(router.state.location.pathname).toBe('/cdn'))
  expect(requests).toEqual([
    {
      method: 'PATCH',
      path: `/compute/v1/projects/${P}/global/backendBuckets/static`,
      body: { enableCdn: false, fingerprint: 'fp-static' },
    },
  ])
})
