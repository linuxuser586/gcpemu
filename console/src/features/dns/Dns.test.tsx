import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, expect, it } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import type { Change, DnsQueryResult, ManagedZone, ResourceRecordSet } from './api'
import { fullName, shortName } from './RecordSetForm'
import { networkUrls } from './ZoneForm'

const P = 'alpha-project'
const NET = (n: string) =>
  `https://www.googleapis.com/compute/v1/projects/${P}/global/networks/${n}`

// A small in-memory Cloud DNS REST API: zones, record sets and changes,
// the admin query endpoint, and the requests the view sends.
let zones: ManagedZone[] = []
let rrsets: Record<string, ResourceRecordSet[]> = {}
let changes: Record<string, Change[]> = {}
let requests: { method: string; path: string; search: URLSearchParams; body: unknown }[] = []
let answer: DnsQueryResult

const err = (code: number, status: string, message: string) =>
  HttpResponse.json({ error: { code, status, message } }, { status: code })

const apex = (z: ManagedZone): ResourceRecordSet[] => [
  { name: z.dnsName, type: 'NS', ttl: 21600, rrdatas: ['ns-cloud-a1.googledomains.com.'] },
  {
    name: z.dnsName,
    type: 'SOA',
    ttl: 21600,
    rrdatas: [
      'ns-cloud-a1.googledomains.com. cloud-dns-hostmaster.google.com. 1 21600 3600 259200 300',
    ],
  },
]

const same = (a: ResourceRecordSet, b: ResourceRecordSet) =>
  a.name.toLowerCase() === b.name.toLowerCase() && a.type === b.type

beforeEach(() => {
  requests = []
  fake.info.services = ['iam', 'compute', 'dns']
  zones = [
    {
      name: 'example',
      dnsName: 'example.com.',
      description: 'Public site',
      visibility: 'public',
      id: '42',
      nameServers: ['ns-cloud-a1.googledomains.com.'],
      creationTime: '2026-10-01T10:00:00.000Z',
    },
    {
      name: 'internal',
      dnsName: 'corp.internal.',
      description: '',
      visibility: 'private',
      privateVisibilityConfig: { networks: [{ networkUrl: NET('vpc-a') }] },
      labels: { team: 'net' },
    },
  ]
  rrsets = {
    example: [
      ...apex(zones[0]!),
      { name: 'www.example.com.', type: 'A', ttl: 300, rrdatas: ['192.0.2.10', '192.0.2.11'] },
      { name: 'mail.example.com.', type: 'MX', ttl: 300, rrdatas: ['10 mx.example.com.'] },
      {
        name: 'wrr.example.com.',
        type: 'A',
        ttl: 30,
        routingPolicy: { wrr: { items: [{ weight: 1, rrdatas: ['192.0.2.41'] }] } },
      },
    ],
    internal: [
      ...apex(zones[1]!),
      { name: 'db.corp.internal.', type: 'A', ttl: 60, rrdatas: ['10.0.0.5'] },
    ],
  }
  changes = {
    example: [
      {
        id: '0',
        status: 'done',
        startTime: '2026-10-01T10:00:00.000Z',
        additions: apex(zones[0]!),
      },
    ],
    internal: [],
  }
  answer = {
    server: '127.0.0.1:5353',
    name: 'www.example.com.',
    type: 'A',
    rcode: 'NOERROR',
    authoritative: true,
    recursionAvailable: true,
    zone: `projects/${P}/managedZones/example`,
    answer: [{ name: 'www.example.com.', type: 'A', ttl: 300, data: '192.0.2.10' }],
    authority: [],
    additional: [],
    latencyMs: 0.4,
  }
  server.use(
    http.get('*/compute/v1/projects/:p/global/networks', () =>
      HttpResponse.json({ items: [{ name: 'vpc-a' }, { name: 'vpc-b' }] }),
    ),
    http.get('*/_emu/v1/dns/query', ({ request }) => {
      const url = new URL(request.url)
      requests.push({ method: 'GET', path: 'query', search: url.searchParams, body: undefined })
      return HttpResponse.json({ ...answer, network: url.searchParams.get('network') ?? undefined })
    }),
    http.all(/\/dns\/v1\/(.*)$/, async ({ request }) => {
      const url = new URL(request.url)
      const text =
        request.method === 'GET' || request.method === 'DELETE' ? '' : await request.text()
      const body: unknown = text ? JSON.parse(text) : undefined
      const path = decodeURIComponent(url.pathname.replace(/^.*\/dns\/v1\//, ''))
      requests.push({ method: request.method, path, search: url.searchParams, body })
      const m =
        /^projects\/[^/]+\/managedZones(?:\/([^/]+)(?:\/(rrsets|changes)(?:\/([^/]+))?)?)?$/.exec(
          path,
        )
      if (!m) return err(400, 'INVALID_ARGUMENT', `bad path ${path}`)
      const [, name, sub, id] = m
      if (!name) {
        if (request.method === 'GET') return HttpResponse.json({ managedZones: zones })
        const z = body as ManagedZone
        if (zones.some((x) => x.name === z.name)) {
          return err(
            409,
            'ALREADY_EXISTS',
            `The resource 'entity.managedZone' named '${z.name}' already exists`,
          )
        }
        zones.push(z)
        rrsets[z.name] = apex(z)
        changes[z.name] = []
        return HttpResponse.json(z)
      }
      const z = zones.find((x) => x.name === name)
      if (!z)
        return err(
          404,
          'NOT_FOUND',
          `The 'parameters.managedZone' resource named '${name}' does not exist.`,
        )
      if (sub === 'rrsets') return HttpResponse.json({ rrsets: rrsets[name] })
      if (sub === 'changes') {
        const cs = changes[name]!
        if (id) return HttpResponse.json({ ...cs.find((c) => c.id === id)!, status: 'done' })
        if (request.method === 'GET') return HttpResponse.json({ changes: [...cs].reverse() })
        const c = body as Change
        const set = rrsets[name]!
        for (const [i, a] of (c.additions ?? []).entries()) {
          if (a.rrdatas?.includes('bogus')) {
            return err(
              400,
              'INVALID_ARGUMENT',
              `Invalid value for 'entity.change.additions[${i}].rrdata[0]': 'bogus'`,
            )
          }
        }
        for (const d of c.deletions ?? []) {
          const i = set.findIndex((x) => same(x, d))
          if (i < 0) return err(404, 'NOT_FOUND', 'gone')
          set.splice(i, 1)
        }
        set.push(...(c.additions ?? []))
        const out = { ...c, id: String(cs.length + 1), status: 'pending' as const }
        cs.push(out)
        return HttpResponse.json(out)
      }
      if (request.method === 'DELETE') {
        if (rrsets[name]!.length > 2) {
          return err(
            400,
            'FAILED_PRECONDITION',
            `The resource named '${name}' cannot be deleted because it is not empty`,
          )
        }
        zones = zones.filter((x) => x !== z)
        return new HttpResponse(null, { status: 204 })
      }
      if (request.method === 'PATCH') {
        Object.assign(z, body)
        return HttpResponse.json({ kind: 'dns#operation', status: 'done', type: 'UPDATE' })
      }
      return HttpResponse.json(z)
    }),
  )
})

const sent = (method: string, path: RegExp) =>
  requests.filter((r) => r.method === method && path.test(r.path))

it('says how to enable Cloud DNS when it is off', async () => {
  fake.info.services = ['iam']
  renderApp(`/dns?project=${P}`)
  expect(
    await screen.findByRole('heading', { name: /Service dns is not enabled/ }),
  ).toBeInTheDocument()
})

it('asks for a Project before listing zones', async () => {
  renderApp('/dns')
  expect(await screen.findByText(/Choose a Project/)).toBeInTheDocument()
})

it('lists zones with their visibility and network bindings, filtered in the URL', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/dns?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Zones' })
  const rows = within(table).getAllByTestId('zone')
  expect(rows[0]).toHaveTextContent('example.com.')
  expect(rows[0]).toHaveTextContent('Public')
  expect(rows[1]).toHaveTextContent('Private')
  expect(rows[1]).toHaveTextContent('vpc-a')

  await user.type(screen.getByLabelText('Filter'), 'corp')
  expect(router.state.location.search).toContain('q=corp')
  expect(within(table).getAllByTestId('zone')).toHaveLength(1)
})

it('creates a private zone with the API’s validation messages and network bindings', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/dns/create?project=${P}`)
  await user.type(await screen.findByLabelText('Zone name'), 'Bad_Name')
  await user.type(screen.getByLabelText('DNS name'), 'app.test')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(
    await screen.findByText("Invalid value for 'entity.managedZone.name': 'Bad_Name'"),
  ).toBeInTheDocument()
  expect(
    screen.getByText("Invalid value for 'entity.managedZone.dnsName': 'app.test'"),
  ).toBeInTheDocument()
  expect(sent('POST', /managedZones$/)).toHaveLength(0)

  await user.clear(screen.getByLabelText('Zone name'))
  await user.type(screen.getByLabelText('Zone name'), 'app')
  await user.type(screen.getByLabelText('DNS name'), '.')
  await user.selectOptions(screen.getByLabelText('Visibility'), 'private')
  await user.click(await screen.findByRole('button', { name: 'Bind network vpc-b' }))
  expect(screen.getByLabelText('Networks')).toHaveValue('vpc-b')
  await user.type(screen.getByLabelText('Networks'), '\nnot a network')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(
    await screen.findByText(
      "Invalid value for 'entity.managedZone.privateVisibilityConfig.networks[1].networkUrl': 'not a network'",
    ),
  ).toBeInTheDocument()
  await user.clear(screen.getByLabelText('Networks'))
  await user.type(screen.getByLabelText('Networks'), `vpc-b\nprojects/other/global/networks/shared`)
  await user.type(screen.getByLabelText('Labels'), 'env=dev')

  await user.click(screen.getByRole('tab', { name: 'JSON' }))
  const json: unknown = JSON.parse(screen.getByLabelText<HTMLTextAreaElement>('Request body').value)
  expect(json).toEqual({
    name: 'app',
    dnsName: 'app.test.',
    visibility: 'private',
    labels: { env: 'dev' },
    privateVisibilityConfig: {
      networks: [
        { networkUrl: NET('vpc-b') },
        {
          networkUrl: 'https://www.googleapis.com/compute/v1/projects/other/global/networks/shared',
        },
      ],
    },
  })
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() => expect(router.state.location.pathname).toBe('/dns/zones/app'))
  expect(sent('POST', /managedZones$/)[0]!.body).toEqual(json)
})

it('shows an API error on the field it names', async () => {
  const user = userEvent.setup()
  renderApp(`/dns/create?project=${P}`)
  await user.type(await screen.findByLabelText('Zone name'), 'example')
  await user.type(screen.getByLabelText('DNS name'), 'example.org.')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  const msg = "The resource 'entity.managedZone' named 'example' already exists"
  // On the field and above the buttons.
  expect(await screen.findAllByText(msg)).toHaveLength(2)
  expect(screen.getByLabelText('Zone name')).toHaveAttribute('aria-invalid', 'true')
})

it('edits a zone’s description and network bindings with managedZones.patch', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/dns/zones/internal/edit?project=${P}`)
  const nets = await screen.findByLabelText('Networks')
  expect(nets).toHaveValue(`projects/${P}/global/networks/vpc-a`)
  await user.click(await screen.findByRole('button', { name: 'Bind network vpc-b' }))
  await user.type(screen.getByLabelText('Description'), 'Corp')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(router.state.location.pathname).toBe('/dns/zones/internal/overview'))
  expect(sent('PATCH', /managedZones\/internal$/)[0]!.body).toEqual({
    description: 'Corp',
    privateVisibilityConfig: {
      networks: [{ networkUrl: NET('vpc-a') }, { networkUrl: NET('vpc-b') }],
    },
  })
  const bindings = await screen.findByRole('list', { name: 'Network bindings' })
  expect(bindings).toHaveTextContent('vpc-b')
})

it('lists record sets and keeps the apex SOA and NS', async () => {
  renderApp(`/dns/zones/example?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Record sets' })
  const www = within(table)
    .getAllByTestId('rrset')
    .find((r) => r.textContent?.includes('www.'))!
  expect(www).toHaveTextContent('192.0.2.10')
  expect(www).toHaveTextContent('192.0.2.11')
  expect(within(table).getByText(/Weighted round robin/)).toBeInTheDocument()
  expect(screen.getByLabelText('Select example.com. (SOA)')).toBeDisabled()
  expect(screen.queryByRole('button', { name: 'Delete example.com. (NS)' })).not.toBeInTheDocument()
})

it('adds a record set as a change, with the API’s messages', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/dns/zones/example/rrsets/create?project=${P}`)
  await user.selectOptions(await screen.findByLabelText('Type'), 'CNAME')
  await user.click(screen.getByRole('button', { name: 'Add' }))
  expect(
    await screen.findByText(
      "The resource 'entity.change.additions[0]' named 'example.com. (CNAME)' cannot be created because a CNAME record set is not permitted at the zone apex.",
    ),
  ).toBeInTheDocument()
  expect(
    screen.getByText("Required field 'entity.change.additions[0].rrdata' not specified"),
  ).toBeInTheDocument()

  await user.selectOptions(screen.getByLabelText('Type'), 'A')
  await user.type(screen.getByLabelText('DNS name'), 'api')
  await user.clear(screen.getByLabelText('TTL (seconds)'))
  await user.type(screen.getByLabelText('TTL (seconds)'), '60')
  await user.type(screen.getByLabelText('Data'), 'bogus')
  await user.click(screen.getByRole('button', { name: 'Add' }))
  expect(
    await screen.findAllByText("Invalid value for 'entity.change.additions[0].rrdata[0]': 'bogus'"),
  ).not.toHaveLength(0)

  await user.clear(screen.getByLabelText('Data'))
  await user.type(screen.getByLabelText('Data'), '192.0.2.20\n192.0.2.21')
  await user.click(screen.getByRole('button', { name: 'Add' }))
  await waitFor(() => expect(router.state.location.pathname).toBe('/dns/zones/example'))
  const posts = sent('POST', /changes$/)
  expect(posts.at(-1)!.body).toEqual({
    additions: [
      { name: 'api.example.com.', type: 'A', ttl: 60, rrdatas: ['192.0.2.20', '192.0.2.21'] },
    ],
  })
  // The change was pending: the view asked about it until it was done.
  expect(sent('GET', /changes\/\d+$/)).not.toHaveLength(0)
  expect(
    await screen.findByText(/Added record set api.example.com. \(A\) in change/),
  ).toBeInTheDocument()
})

it('edits a record set by deleting and adding it in one change', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/dns/zones/example?project=${P}`)
  await user.click(await screen.findByRole('link', { name: 'Edit www.example.com. (A)' }))
  const data = await screen.findByLabelText('Data')
  expect(data).toHaveValue('192.0.2.10\n192.0.2.11')
  expect(screen.getByLabelText('DNS name')).toHaveValue('www')
  await user.clear(data)
  await user.type(data, '192.0.2.99')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(router.state.location.pathname).toBe('/dns/zones/example'))
  expect(sent('POST', /changes$/)[0]!.body).toEqual({
    additions: [{ name: 'www.example.com.', type: 'A', ttl: 300, rrdatas: ['192.0.2.99'] }],
    deletions: [
      { name: 'www.example.com.', type: 'A', ttl: 300, rrdatas: ['192.0.2.10', '192.0.2.11'] },
    ],
  })
})

it('keeps a routing policy, which the JSON tab edits', async () => {
  renderApp(`/dns/zones/example/rrsets/wrr.example.com./A/edit?project=${P}`)
  expect(await screen.findByText(/answers through a routing policy/)).toBeInTheDocument()
  expect(screen.queryByLabelText('Data')).not.toBeInTheDocument()
})

it('deletes the selected record sets in one change', async () => {
  const user = userEvent.setup()
  renderApp(`/dns/zones/example?project=${P}`)
  await user.click(await screen.findByLabelText('Select www.example.com. (A)'))
  await user.click(screen.getByLabelText('Select mail.example.com. (MX)'))
  await user.click(screen.getByRole('button', { name: 'Delete selected' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete record sets' }),
  )
  await waitFor(() => expect(sent('POST', /changes$/)).toHaveLength(1))
  const body = sent('POST', /changes$/)[0]!.body as Change
  expect(body.additions).toBeUndefined()
  expect(body.deletions?.map((d) => `${d.name} ${d.type}`)).toEqual([
    'www.example.com. A',
    'mail.example.com. MX',
  ])
  expect(await screen.findByText(/Deleted 2 record sets in change/)).toBeInTheDocument()
})

it('shows the change history', async () => {
  renderApp(`/dns/zones/example/changes?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Changes' })
  expect(within(table).getByTestId('change')).toHaveTextContent('+ example.com. SOA')
})

it('deletes a zone after its record sets', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/dns/zones/internal/overview?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Delete zone' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete zone' }),
  )
  await waitFor(() => expect(router.state.location.pathname).toBe('/dns'))
  expect(sent('POST', /internal\/changes$/)[0]!.body).toEqual({
    deletions: [{ name: 'db.corp.internal.', type: 'A', ttl: 60, rrdatas: ['10.0.0.5'] }],
  })
  expect(sent('DELETE', /managedZones\/internal$/)).toHaveLength(1)
})

it('resolves a name from the host or a bound network in the query box', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/dns/query?project=${P}`)
  await user.type(await screen.findByLabelText('Name'), 'www.example.com')
  await user.click(screen.getByRole('button', { name: 'Resolve' }))
  const response = await screen.findByRole('region', { name: 'DNS response' })
  expect(within(response).getByText('NOERROR')).toBeInTheDocument()
  expect(within(response).getByRole('table', { name: 'Answer' })).toHaveTextContent('192.0.2.10')
  expect(
    within(response).getByRole('link', { name: `projects/${P}/managedZones/example` }),
  ).toHaveAttribute('href', `/dns/zones/example?project=${P}`)
  expect(router.state.location.search).toContain('name=www.example.com')
  expect(requests.at(-1)!.search.get('type')).toBe('A')
  expect(requests.at(-1)!.search.has('network')).toBe(false)

  await user.selectOptions(screen.getByLabelText('From'), `projects/${P}/global/networks/vpc-a`)
  await user.selectOptions(screen.getByLabelText('Type'), 'TXT')
  await user.click(screen.getByRole('button', { name: 'Resolve' }))
  await waitFor(() =>
    expect(requests.at(-1)!.search.get('network')).toBe(`projects/${P}/global/networks/vpc-a`),
  )
  expect(requests.at(-1)!.search.get('type')).toBe('TXT')
})

it('asks again a question from a link', async () => {
  answer = {
    ...answer,
    rcode: 'REFUSED',
    zone: undefined,
    authoritative: false,
    recursionAvailable: false,
    answer: [],
  }
  renderApp(`/dns/query?project=${P}&name=db.corp.internal.&type=A`)
  expect(await screen.findByText('REFUSED')).toBeInTheDocument()
  expect(screen.getByText(/forwarding to the host resolver is off/)).toBeInTheDocument()
  expect(screen.getByLabelText('Name')).toHaveValue('db.corp.internal.')
})

it('names record sets within their zone', () => {
  const z = { name: 'example', dnsName: 'example.com.' }
  expect(fullName('', z)).toBe('example.com.')
  expect(fullName('@', z)).toBe('example.com.')
  expect(fullName('www', z)).toBe('www.example.com.')
  expect(fullName('other.test.', z)).toBe('other.test.')
  expect(shortName('www.example.com.', z)).toBe('www')
  expect(shortName('example.com.', z)).toBe('')
  expect(networkUrls('vpc-a\nprojects/x/global/networks/y', P)).toEqual([
    NET('vpc-a'),
    'https://www.googleapis.com/compute/v1/projects/x/global/networks/y',
  ])
})
