import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, describe, expect, it } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import type { Firewall, Network, Route } from './api'
import { firewallRecipe, routeRecipe } from './copy'

const P = 'alpha-project'
const link = (p: string) => `https://www.googleapis.com/compute/v1/projects/${P}/${p}`

const sshRule = (over: Partial<Firewall> = {}): Firewall => ({
  name: 'allow-ssh',
  id: '301',
  kind: 'compute#firewall',
  selfLink: link('global/firewalls/allow-ssh'),
  creationTimestamp: '2026-10-12T09:02:00Z',
  network: link('global/networks/vpc'),
  direction: 'INGRESS',
  priority: 1000,
  allowed: [{ IPProtocol: 'tcp', ports: ['22', '8000-8080'] }, { IPProtocol: 'icmp' }],
  sourceRanges: ['35.235.240.0/20'],
  targetTags: ['ssh'],
  disabled: false,
  logConfig: { enable: false },
  ...over,
})

const viaProxy = (over: Partial<Route> = {}): Route => ({
  name: 'via-proxy',
  id: '401',
  kind: 'compute#route',
  selfLink: link('global/routes/via-proxy'),
  creationTimestamp: '2026-10-12T09:03:00Z',
  network: link('global/networks/vpc'),
  destRange: '10.50.0.0/16',
  priority: 900,
  nextHopIp: '10.0.0.9',
  tags: ['web', 'api'],
  routeType: 'STATIC',
  ...over,
})

describe('Copy as', () => {
  it('copies an ingress allow rule', () => {
    const r = firewallRecipe(sshRule({ description: 'SSH from IAP' }), P)
    expect(r.gcloud).toBe(
      [
        'gcloud compute firewall-rules create allow-ssh \\',
        `    --project=${P} \\`,
        '    --network=vpc \\',
        '    --direction=INGRESS \\',
        '    --priority=1000 \\',
        '    --action=ALLOW \\',
        '    --rules=tcp:22,tcp:8000-8080,icmp \\',
        '    --source-ranges=35.235.240.0/20 \\',
        '    --target-tags=ssh \\',
        "    --description='SSH from IAP'",
      ].join('\n'),
    )
    expect(r.rest).toEqual({
      method: 'POST',
      path: `/compute/v1/projects/${P}/global/firewalls`,
      body: {
        name: 'allow-ssh',
        description: 'SSH from IAP',
        network: `projects/${P}/global/networks/vpc`,
        direction: 'INGRESS',
        priority: 1000,
        allowed: [{ IPProtocol: 'tcp', ports: ['22', '8000-8080'] }, { IPProtocol: 'icmp' }],
        sourceRanges: ['35.235.240.0/20'],
        targetTags: ['ssh'],
        disabled: false,
        logConfig: { enable: false },
      },
    })
    expect(r.tofu).toBe(
      [
        'resource "google_compute_firewall" "allow-ssh" {',
        `  project       = "${P}"`,
        '  name          = "allow-ssh"',
        `  network       = "projects/${P}/global/networks/vpc"`,
        '  description   = "SSH from IAP"',
        '  direction     = "INGRESS"',
        '  priority      = 1000',
        '  source_ranges = ["35.235.240.0/20"]',
        '  target_tags   = ["ssh"]',
        '  allow {',
        '    protocol = "tcp"',
        '    ports    = ["22", "8000-8080"]',
        '  }',
        '  allow {',
        '    protocol = "icmp"',
        '  }',
        '}',
      ].join('\n'),
    )
  })

  it('copies a disabled, logged egress deny rule', () => {
    const r = firewallRecipe(
      sshRule({
        direction: 'EGRESS',
        priority: 0,
        allowed: undefined,
        denied: [{ IPProtocol: 'all' }],
        sourceRanges: undefined,
        destinationRanges: ['0.0.0.0/0'],
        targetTags: undefined,
        disabled: true,
        logConfig: { enable: true },
      }),
      P,
    )
    expect(r.gcloud).toContain('--priority=0 \\')
    expect(r.gcloud).toContain('--action=DENY \\')
    expect(r.gcloud).toContain('--rules=all \\')
    expect(r.gcloud).toContain('--destination-ranges=0.0.0.0/0 \\')
    expect(r.gcloud).toContain('--disabled \\')
    expect(r.gcloud).toMatch(/--enable-logging$/)
    expect(r.tofu).toContain('  priority           = 0\n')
    expect(r.tofu).toContain('  disabled           = true\n')
    expect(r.tofu).toContain('  deny {\n    protocol = "all"\n  }')
    expect(r.tofu).toContain('  log_config {\n    metadata = "INCLUDE_ALL_METADATA"\n  }')
  })

  it('copies a route to a next hop address', () => {
    const r = routeRecipe(viaProxy({ description: 'Through the proxy' }), P)
    expect(r.gcloud).toBe(
      [
        'gcloud compute routes create via-proxy \\',
        `    --project=${P} \\`,
        '    --network=vpc \\',
        '    --destination-range=10.50.0.0/16 \\',
        '    --priority=900 \\',
        '    --next-hop-address=10.0.0.9 \\',
        '    --tags=web,api \\',
        "    --description='Through the proxy'",
      ].join('\n'),
    )
    expect(r.rest).toEqual({
      method: 'POST',
      path: `/compute/v1/projects/${P}/global/routes`,
      body: {
        name: 'via-proxy',
        description: 'Through the proxy',
        network: `projects/${P}/global/networks/vpc`,
        destRange: '10.50.0.0/16',
        priority: 900,
        nextHopIp: '10.0.0.9',
        tags: ['web', 'api'],
      },
    })
    expect(r.tofu).toBe(
      [
        'resource "google_compute_route" "via-proxy" {',
        `  project     = "${P}"`,
        '  name        = "via-proxy"',
        `  network     = "projects/${P}/global/networks/vpc"`,
        '  description = "Through the proxy"',
        '  dest_range  = "10.50.0.0/16"',
        '  priority    = 900',
        '  next_hop_ip = "10.0.0.9"',
        '  tags        = ["web", "api"]',
        '}',
      ].join('\n'),
    )
  })

  it('copies a route to the internet gateway by name', () => {
    const r = routeRecipe(
      viaProxy({
        nextHopIp: undefined,
        tags: undefined,
        nextHopGateway: link('global/gateways/default-internet-gateway'),
      }),
      P,
    )
    expect(r.gcloud).toMatch(/--next-hop-gateway=default-internet-gateway$/)
    expect(r.tofu).toContain('next_hop_gateway = "default-internet-gateway"')
    expect(r.rest).toMatchObject({
      body: { nextHopGateway: `projects/${P}/global/gateways/default-internet-gateway` },
    })
  })
})

// A small in-memory compute API: networks, firewall rules and routes.
let firewalls: Firewall[]
let routes: Route[]
let requests: { method: string; path: string; body: unknown }[]

const op = { name: 'op-1', status: 'DONE', selfLink: link('global/operations/op-1') }
const err = (code: number, message: string) =>
  HttpResponse.json({ error: { code, message } }, { status: code })

const NOTICE =
  'Recorded: stored and returned, not enforced by the emulator. Use a Kubernetes NetworkPolicy to restrict GKE pod traffic.'

/** defaultRoute is the route the API creates with network vpc. */
const defaultRoute = viaProxy({
  name: 'default-route-1a2b3c4d',
  selfLink: link('global/routes/default-route-1a2b3c4d'),
  description: 'Default route to the Internet.',
  destRange: '0.0.0.0/0',
  priority: 1000,
  nextHopIp: undefined,
  tags: undefined,
  nextHopGateway: link('global/gateways/default-internet-gateway'),
})

beforeEach(() => {
  requests = []
  fake.info.services = ['iam', 'compute']
  const networks: Network[] = [
    {
      name: 'vpc',
      selfLink: link('global/networks/vpc'),
      autoCreateSubnetworks: false,
    },
    {
      name: 'other',
      selfLink: link('global/networks/other'),
      autoCreateSubnetworks: false,
    },
  ]
  firewalls = [
    sshRule(),
    sshRule({
      name: 'deny-egress',
      selfLink: link('global/firewalls/deny-egress'),
      network: link('global/networks/other'),
      direction: 'EGRESS',
      priority: 65534,
      allowed: undefined,
      denied: [{ IPProtocol: 'all' }],
      sourceRanges: undefined,
      destinationRanges: ['0.0.0.0/0'],
      targetTags: undefined,
    }),
  ]
  routes = [defaultRoute, viaProxy()]
  const record = async (request: Request) => {
    const text = await request.text()
    const body = text ? (JSON.parse(text) as Record<string, unknown>) : undefined
    requests.push({ method: request.method, path: new URL(request.url).pathname, body })
    return body ?? {}
  }
  const N = '*/compute/v1/projects/:p/global/networks'
  const F = '*/compute/v1/projects/:p/global/firewalls'
  const R = '*/compute/v1/projects/:p/global/routes'
  const fw = (name: unknown) => firewalls.find((f) => f.name === name)
  const rt = (name: unknown) => routes.find((r) => r.name === name)
  server.use(
    http.get(N, () => HttpResponse.json({ items: networks })),
    http.get(`${N}/:name`, ({ params }) => {
      const n = networks.find((x) => x.name === params.name)
      return n ? HttpResponse.json(n) : err(404, 'not found')
    }),
    http.get('*/compute/v1/projects/:p/aggregated/subnetworks', () =>
      HttpResponse.json({ items: {} }),
    ),
    http.get('*/_emu/v1/vpc/containerNetworks', () => HttpResponse.json({ containerNetworks: [] })),
    http.get(F, () => HttpResponse.json({ items: firewalls })),
    http.post(F, async ({ request }) => {
      const body = (await record(request)) as Firewall
      if (body.sourceRanges?.includes('10.0.0.1/8')) {
        return err(
          400,
          "Invalid value for field 'resource.sourceRanges': '10.0.0.1/8'. Must be a valid IPv4 CIDR address range.",
        )
      }
      firewalls.push({
        ...body,
        selfLink: link(`global/firewalls/${body.name}`),
        network: link(`global/networks/${body.network?.split('/').pop() ?? ''}`),
      })
      return HttpResponse.json(op)
    }),
    http.get(`${F}/:name`, ({ params }) => {
      const f = fw(params.name)
      return f ? HttpResponse.json(f) : err(404, 'not found')
    }),
    http.patch(`${F}/:name`, async ({ request, params }) => {
      const body = await record(request)
      const f = fw(params.name)!
      if ('allowed' in body) delete f.denied
      if ('denied' in body) delete f.allowed
      Object.assign(f, body)
      return HttpResponse.json(op)
    }),
    http.delete(`${F}/:name`, async ({ request, params }) => {
      await record(request)
      firewalls = firewalls.filter((f) => f.name !== params.name)
      return HttpResponse.json(op)
    }),
    http.get(R, () => HttpResponse.json({ items: routes })),
    http.post(R, async ({ request }) => {
      const body = (await record(request)) as Route
      routes.push({
        ...body,
        selfLink: link(`global/routes/${body.name}`),
        network: link(`global/networks/${body.network?.split('/').pop() ?? ''}`),
        routeType: 'STATIC',
      })
      return HttpResponse.json(op)
    }),
    http.get(`${R}/:name`, ({ params }) => {
      const r = rt(params.name)
      return r ? HttpResponse.json(r) : err(404, 'not found')
    }),
    http.delete(`${R}/:name`, async ({ request, params }) => {
      await record(request)
      routes = routes.filter((r) => r.name !== params.name)
      return HttpResponse.json(op)
    }),
  )
})

const writes = () => requests.filter((r) => r.method !== 'GET')

describe('firewall rules', () => {
  it('lists firewall rules, saying they are not enforced', async () => {
    renderApp(`/compute/firewalls?project=${P}`)
    const table = await screen.findByRole('table', { name: 'Firewall rules' })
    const rows = within(table).getAllByTestId('firewall')
    expect(rows).toHaveLength(2)
    expect(rows[0]).toHaveTextContent('allow-ssh')
    expect(rows[0]).toHaveTextContent('vpc')
    expect(rows[0]).toHaveTextContent('INGRESS')
    expect(rows[0]).toHaveTextContent('1000')
    expect(rows[0]).toHaveTextContent('Allow')
    expect(rows[0]).toHaveTextContent('tcp:22,tcp:8000-8080,icmp')
    expect(rows[0]).toHaveTextContent('Tags: ssh')
    expect(rows[0]).toHaveTextContent('35.235.240.0/20')
    expect(rows[1]).toHaveTextContent('EGRESS')
    expect(rows[1]).toHaveTextContent('Deny')
    expect(rows[1]).toHaveTextContent('All VM instances')
    expect(within(rows[0]!).getByRole('link', { name: 'allow-ssh' })).toHaveAttribute(
      'href',
      `/compute/global/firewalls/allow-ssh?project=${P}`,
    )
    expect(screen.getByRole('note')).toHaveTextContent(NOTICE)
    expect(screen.getByRole('link', { name: 'Firewall rules' })).toHaveAttribute(
      'aria-current',
      'page',
    )
  })

  it('creates a firewall rule, showing the API’s messages', async () => {
    const user = userEvent.setup()
    const { router } = renderApp(`/compute/create-firewall?network=vpc&project=${P}`)
    expect(await screen.findByLabelText('Network')).toHaveValue('vpc')
    expect(screen.getByRole('note')).toHaveTextContent(NOTICE)
    await user.type(screen.getByLabelText('Name'), 'Allow_web')
    await user.type(screen.getByLabelText('Protocols and ports'), 'tcp:99999')
    await user.type(screen.getByLabelText('Priority'), '70000')
    await user.click(screen.getByRole('button', { name: 'Create' }))
    expect(
      await screen.findByText(/Invalid value for field 'resource.name': 'Allow_web'/),
    ).toBeVisible()
    expect(
      screen.getByText(
        "Invalid value for field 'resource.allowed': 'tcp:99999'. Ports are 0 to 65535.",
      ),
    ).toBeVisible()
    expect(
      screen.getByText(
        "Invalid value for field 'resource.priority': '70000'. Must be between 0 and 65535.",
      ),
    ).toBeVisible()

    await user.clear(screen.getByLabelText('Name'))
    await user.type(screen.getByLabelText('Name'), 'allow-web')
    await user.clear(screen.getByLabelText('Protocols and ports'))
    await user.type(screen.getByLabelText('Protocols and ports'), 'tcp:80,tcp:443,icmp')
    await user.clear(screen.getByLabelText('Priority'))
    await user.type(screen.getByLabelText('Source ranges'), '10.0.0.1/8')
    await user.type(screen.getByLabelText('Target tags'), 'web, api')
    await user.click(screen.getByRole('button', { name: 'Create' }))
    expect(
      await screen.findByText(
        "Invalid value for field 'resource.sourceRanges': '10.0.0.1/8'. Must be a valid IPv4 CIDR address range.",
        { selector: '[id$="-error"]' },
      ),
    ).toBeVisible()
    await user.clear(screen.getByLabelText('Source ranges'))
    await user.type(screen.getByLabelText('Source ranges'), '10.0.0.0/8')
    await user.click(screen.getByRole('button', { name: 'Create' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/compute/global/firewalls/allow-web'),
    )
    expect(writes().at(-1)).toEqual({
      method: 'POST',
      path: `/compute/v1/projects/${P}/global/firewalls`,
      body: {
        name: 'allow-web',
        network: `projects/${P}/global/networks/vpc`,
        direction: 'INGRESS',
        allowed: [{ IPProtocol: 'tcp', ports: ['80', '443'] }, { IPProtocol: 'icmp' }],
        sourceRanges: ['10.0.0.0/8'],
        targetTags: ['web', 'api'],
      },
    })
  })

  it('creates an egress deny rule to destination ranges', async () => {
    const user = userEvent.setup()
    renderApp(`/compute/create-firewall?project=${P}`)
    await user.type(await screen.findByLabelText('Name'), 'deny-all')
    await user.type(screen.getByLabelText('Network'), 'other')
    await user.selectOptions(screen.getByLabelText('Direction'), 'EGRESS')
    await user.selectOptions(screen.getByLabelText('Action'), 'DENY')
    expect(screen.queryByLabelText('Source ranges')).toBeNull()
    await user.type(screen.getByLabelText('Protocols and ports'), 'all')
    await user.type(screen.getByLabelText('Priority'), '0')
    await user.type(screen.getByLabelText('Destination ranges'), '0.0.0.0/0')
    await user.click(screen.getByLabelText('Disabled'))
    await user.click(screen.getByLabelText('Logs'))
    await user.click(screen.getByRole('button', { name: 'Create' }))
    await waitFor(() => expect(writes()).toHaveLength(1))
    expect(writes()[0]!.body).toEqual({
      name: 'deny-all',
      network: `projects/${P}/global/networks/other`,
      direction: 'EGRESS',
      priority: 0,
      denied: [{ IPProtocol: 'all' }],
      destinationRanges: ['0.0.0.0/0'],
      disabled: true,
      logConfig: { enable: true },
    })
  })

  it('shows a firewall rule, copies it and deletes it', async () => {
    const user = userEvent.setup()
    const { router } = renderApp(`/compute/global/firewalls/allow-ssh?project=${P}`)
    const details = await screen.findByLabelText('Firewall rule details')
    expect(details).toHaveTextContent('INGRESS')
    expect(details).toHaveTextContent('tcp:22,tcp:8000-8080,icmp')
    expect(details).toHaveTextContent('35.235.240.0/20')
    expect(details).toHaveTextContent('Tags: ssh')
    expect(within(details).getByRole('link', { name: 'vpc' })).toHaveAttribute(
      'href',
      `/compute/global/networks/vpc?project=${P}`,
    )
    expect(screen.getByRole('note')).toHaveTextContent(NOTICE)
    expect(screen.getByRole('link', { name: 'Edit firewall rule' })).toHaveAttribute(
      'href',
      `/compute/global/firewalls/allow-ssh/edit?project=${P}`,
    )

    await user.click(screen.getByRole('button', { name: 'Copy firewall rule as' }))
    const copy = await screen.findByRole('dialog', { name: 'Copy firewall rule as' })
    await user.click(within(copy).getByRole('tab', { name: 'gcloud' }))
    expect(within(copy).getByLabelText('gcloud for the firewall rule')).toHaveTextContent(
      'gcloud compute firewall-rules create allow-ssh',
    )
    await user.keyboard('{Escape}')

    await user.click(screen.getByRole('button', { name: 'Delete firewall rule' }))
    await user.click(
      within(await screen.findByRole('dialog')).getByRole('button', {
        name: 'Delete firewall rule',
      }),
    )
    await waitFor(() => expect(router.state.location.pathname).toBe('/compute/firewalls'))
    expect(writes()).toEqual([
      {
        method: 'DELETE',
        path: `/compute/v1/projects/${P}/global/firewalls/allow-ssh`,
        body: undefined,
      },
    ])
  })

  it('edits a firewall rule as a merge patch of what changed', async () => {
    const user = userEvent.setup()
    const { router } = renderApp(`/compute/global/firewalls/allow-ssh/edit?project=${P}`)
    expect(await screen.findByLabelText('Protocols and ports')).toHaveValue(
      'tcp:22,tcp:8000-8080,icmp',
    )
    expect(screen.queryByLabelText('Name')).toBeNull()
    expect(screen.queryByLabelText('Direction')).toBeNull()
    expect(screen.getByLabelText('Target tags')).toHaveValue('ssh')
    await user.selectOptions(screen.getByLabelText('Action'), 'DENY')
    await user.clear(screen.getByLabelText('Protocols and ports'))
    await user.type(screen.getByLabelText('Protocols and ports'), 'tcp:23')
    await user.clear(screen.getByLabelText('Priority'))
    await user.type(screen.getByLabelText('Priority'), '0')
    await user.clear(screen.getByLabelText('Target tags'))
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/compute/global/firewalls/allow-ssh'),
    )
    expect(writes()).toEqual([
      {
        method: 'PATCH',
        path: `/compute/v1/projects/${P}/global/firewalls/allow-ssh`,
        body: { denied: [{ IPProtocol: 'tcp', ports: ['23'] }], priority: 0, targetTags: [] },
      },
    ])
    const details = await screen.findByLabelText('Firewall rule details')
    await waitFor(() => expect(details).toHaveTextContent('Deny'))
    expect(details).toHaveTextContent('All VM instances')
  })
})

describe('routes', () => {
  it('lists routes, saying they are not enforced and which the system made', async () => {
    renderApp(`/compute/routes?project=${P}`)
    const table = await screen.findByRole('table', { name: 'Routes' })
    const rows = within(table).getAllByTestId('route')
    expect(rows).toHaveLength(2)
    expect(rows[0]).toHaveTextContent('via-proxy')
    expect(rows[0]).toHaveTextContent('10.50.0.0/16')
    expect(rows[0]).toHaveTextContent('900')
    expect(rows[0]).toHaveTextContent('IP 10.0.0.9')
    expect(rows[0]).toHaveTextContent('web, api')
    expect(rows[0]).not.toHaveTextContent('System-generated')
    expect(rows[1]).toHaveTextContent('default-route-1a2b3c4d')
    expect(rows[1]).toHaveTextContent('Gateway default-internet-gateway')
    expect(rows[1]).toHaveTextContent('System-generated')
    expect(within(rows[0]!).getByRole('link', { name: 'via-proxy' })).toHaveAttribute(
      'href',
      `/compute/global/routes/via-proxy?project=${P}`,
    )
    expect(screen.getByRole('note')).toHaveTextContent(NOTICE)
    expect(screen.getByRole('link', { name: 'Routes' })).toHaveAttribute('aria-current', 'page')
  })

  it('shows a system-generated route read-only', async () => {
    renderApp(`/compute/global/routes/default-route-1a2b3c4d?project=${P}`)
    const details = await screen.findByLabelText('Route details')
    expect(details).toHaveTextContent('0.0.0.0/0')
    expect(details).toHaveTextContent('Gateway default-internet-gateway')
    expect(screen.getByText(/made by GCP with the network/)).toBeVisible()
    expect(screen.getByRole('button', { name: 'Copy route as' })).toBeVisible()
    expect(screen.queryByRole('button', { name: 'Delete route' })).toBeNull()
  })

  it('shows a route, copies it and deletes it', async () => {
    const user = userEvent.setup()
    const { router } = renderApp(`/compute/global/routes/via-proxy?project=${P}`)
    const details = await screen.findByLabelText('Route details')
    expect(details).toHaveTextContent('IP 10.0.0.9')
    expect(details).toHaveTextContent('web, api')
    expect(screen.getByRole('note')).toHaveTextContent(NOTICE)
    expect(screen.queryByRole('link', { name: /Edit/ })).toBeNull()

    await user.click(screen.getByRole('button', { name: 'Copy route as' }))
    const copy = await screen.findByRole('dialog', { name: 'Copy route as' })
    await user.click(within(copy).getByRole('tab', { name: 'OpenTofu' }))
    expect(within(copy).getByLabelText('OpenTofu for the route')).toHaveTextContent(
      'resource "google_compute_route" "via-proxy" {',
    )
    await user.keyboard('{Escape}')

    await user.click(screen.getByRole('button', { name: 'Delete route' }))
    await user.click(
      within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete route' }),
    )
    await waitFor(() => expect(router.state.location.pathname).toBe('/compute/routes'))
    expect(writes()).toEqual([
      {
        method: 'DELETE',
        path: `/compute/v1/projects/${P}/global/routes/via-proxy`,
        body: undefined,
      },
    ])
  })

  it('creates a route to a next hop address, checking what the API checks', async () => {
    const user = userEvent.setup()
    const { router } = renderApp(`/compute/create-route?network=vpc&project=${P}`)
    expect(await screen.findByLabelText('Network')).toHaveValue('vpc')
    expect(screen.getByRole('note')).toHaveTextContent(NOTICE)
    await user.type(screen.getByLabelText('Name'), 'via-ip')
    await user.type(screen.getByLabelText('Destination range'), '10.60.0.1/16')
    await user.selectOptions(screen.getByLabelText('Next hop'), 'nextHopIp')
    await user.click(screen.getByRole('button', { name: 'Create' }))
    expect(
      await screen.findByText(
        "Invalid value for field 'resource.destRange': '10.60.0.1/16'. Must be a valid IPv4 CIDR address range.",
      ),
    ).toBeVisible()
    expect(screen.getByText("Required field 'resource.nextHopIp' not specified")).toBeVisible()
    await user.clear(screen.getByLabelText('Destination range'))
    await user.type(screen.getByLabelText('Destination range'), '10.60.0.0/16')
    await user.type(screen.getByLabelText('Next hop IP address'), '10.0.0.5')
    await user.type(screen.getByLabelText('Priority'), '800')
    await user.type(screen.getByLabelText('Network tags'), 'web')
    await user.click(screen.getByRole('button', { name: 'Create' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/compute/global/routes/via-ip'),
    )
    expect(writes()).toEqual([
      {
        method: 'POST',
        path: `/compute/v1/projects/${P}/global/routes`,
        body: {
          name: 'via-ip',
          network: `projects/${P}/global/networks/vpc`,
          destRange: '10.60.0.0/16',
          priority: 800,
          nextHopIp: '10.0.0.5',
          tags: ['web'],
        },
      },
    ])
  })

  it('creates a route to the internet gateway by default', async () => {
    const user = userEvent.setup()
    renderApp(`/compute/create-route?network=vpc&project=${P}`)
    await user.type(await screen.findByLabelText('Name'), 'egress')
    await user.type(screen.getByLabelText('Destination range'), '0.0.0.0/0')
    expect(screen.getByLabelText('Next hop gateway')).toHaveValue('default-internet-gateway')
    await user.click(screen.getByRole('button', { name: 'Create' }))
    await waitFor(() => expect(writes()).toHaveLength(1))
    expect(writes()[0]!.body).toEqual({
      name: 'egress',
      network: `projects/${P}/global/networks/vpc`,
      destRange: '0.0.0.0/0',
      nextHopGateway: `projects/${P}/global/gateways/default-internet-gateway`,
    })
  })
})

it('shows a network’s firewall rules and routes, with links to add more', async () => {
  renderApp(`/compute/global/networks/vpc?project=${P}`)
  const fwTable = await screen.findByRole('table', { name: 'Firewall rules' })
  const fws = within(fwTable).getAllByTestId('firewall')
  expect(fws).toHaveLength(1)
  expect(within(fws[0]!).getByRole('link', { name: 'allow-ssh' })).toHaveAttribute(
    'href',
    `/compute/global/firewalls/allow-ssh?project=${P}`,
  )
  const rts = within(screen.getByRole('table', { name: 'Routes' })).getAllByTestId('route')
  expect(rts.map((r) => within(r).getAllByRole('link')[0]!.textContent)).toEqual([
    'via-proxy',
    'default-route-1a2b3c4d',
  ])
  expect(screen.getByRole('link', { name: 'Add firewall rule' })).toHaveAttribute(
    'href',
    `/compute/create-firewall?network=vpc&project=${P}`,
  )
  expect(screen.getByRole('link', { name: 'Add route' })).toHaveAttribute(
    'href',
    `/compute/create-route?network=vpc&project=${P}`,
  )
  expect(screen.getByRole('note')).toHaveTextContent(NOTICE)
})
