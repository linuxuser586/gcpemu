import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, expect, it, vi } from 'vitest'

import { POLL_MS } from '@/api/queries'
import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import type { Cluster, KubePod } from './api'
import { clusterRecipe, nodePoolRecipe } from './copy'

const P = 'alpha-project'
const base = `/gke/locations/us-central1-a/clusters/web`
const gw = `*/connectgateway/v1/projects/${P}/locations/us-central1-a/gkeMemberships/web`

function cluster(over: Partial<Cluster> = {}): Cluster {
  return {
    name: 'web',
    id: 'abc123',
    location: 'us-central1-a',
    locations: ['us-central1-a'],
    status: 'RUNNING',
    endpoint: '172.18.0.5',
    currentMasterVersion: '1.36.5-gke.100',
    currentNodeCount: 1,
    masterAuth: { clusterCaCertificate: 'LS0tQ0E=' },
    releaseChannel: { channel: 'REGULAR' },
    resourceLabels: { team: 'web' },
    labelFingerprint: 'fp1',
    loggingService: 'logging.googleapis.com/kubernetes',
    monitoringService: 'monitoring.googleapis.com/kubernetes',
    nodePools: [
      {
        name: 'default-pool',
        status: 'RUNNING',
        initialNodeCount: 1,
        version: '1.36.5-gke.100',
        config: { machineType: 'e2-medium', labels: { tier: 'a' } },
      },
    ],
    ...over,
  }
}

/** autopilot is an Autopilot cluster, with GKE's own node pool. */
function autopilot(over: Partial<Cluster> = {}): Cluster {
  return cluster({
    name: 'auto',
    location: 'us-central1',
    locations: ['us-central1-a', 'us-central1-b', 'us-central1-c'],
    autopilot: { enabled: true },
    nodePools: [
      {
        name: 'default-pool',
        status: 'RUNNING',
        initialNodeCount: 1,
        version: '1.36.5-gke.100',
        config: { machineType: 'ek-standard-2' },
      },
    ],
    ...over,
  })
}

const autoBase = `/gke/locations/us-central1/clusters/auto`
const autoGw = `*/connectgateway/v1/projects/${P}/locations/us-central1/gkeMemberships/auto`
const getAutopilot = http.get(
  `*/container/v1/projects/${P}/locations/us-central1/clusters/auto`,
  () => HttpResponse.json(autopilot()),
)

const op = (type: string) => ({
  name: `operation-${type}`,
  operationType: type,
  status: 'DONE',
  location: 'us-central1-a',
})

const pod = (name: string, over: Partial<KubePod['status']> = {}): KubePod => ({
  metadata: { name, namespace: 'default', creationTimestamp: new Date().toISOString() },
  spec: { nodeName: 'gke-web-node-1', containers: [{ name: 'app' }, { name: 'sidecar' }] },
  status: {
    phase: 'Running',
    podIP: '10.4.0.7',
    containerStatuses: [
      { name: 'app', ready: true, restartCount: 0 },
      { name: 'sidecar', ready: true, restartCount: 0 },
    ],
    ...over,
  },
})

/** requests records the bodies the fake container API received. */
let requests: { method: string; path: string; body: unknown }[] = []

async function record(request: Request) {
  const text = await request.text()
  requests.push({
    method: request.method,
    path: new URL(request.url).pathname,
    body: text ? (JSON.parse(text) as unknown) : undefined,
  })
}

beforeEach(() => {
  requests = []
  fake.info.services = ['iam', 'gke', 'compute']
  server.use(
    http.get(`*/container/v1/projects/${P}/locations/-/clusters`, () =>
      HttpResponse.json({
        clusters: [
          cluster(),
          cluster({ name: 'batch', location: 'europe-west1', status: 'PROVISIONING' }),
        ],
      }),
    ),
    http.get(`*/container/v1/projects/${P}/locations/us-central1-a/clusters/web`, () =>
      HttpResponse.json(cluster()),
    ),
    http.get(`*/container/v1/projects/${P}/locations/:loc/serverConfig`, () =>
      HttpResponse.json({
        defaultClusterVersion: '1.36.5-gke.100',
        validMasterVersions: ['1.37.1-gke.100', '1.36.5-gke.100'],
        validNodeVersions: ['1.37.1-gke.100', '1.36.5-gke.100'],
      }),
    ),
  )
})

it('says how to enable GKE when it is off', async () => {
  fake.info.services = ['iam']
  renderApp(`/gke?project=${P}`)
  expect(
    await screen.findByRole('heading', { name: /Service gke is not enabled/ }),
  ).toBeInTheDocument()
})

it('asks for a Project first', async () => {
  renderApp('/gke')
  expect(await screen.findByText(/Choose a Project/)).toBeInTheDocument()
})

it('lists clusters, filtering by location and name in the URL', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/gke?project=${P}`)
  const rows = () => screen.getAllByTestId('cluster')
  await screen.findByRole('table', { name: 'Clusters' })
  expect(rows()).toHaveLength(2)
  expect(screen.getByText('PROVISIONING')).toBeInTheDocument()
  await user.type(screen.getByLabelText('Location'), 'us-central1')
  expect(rows()).toHaveLength(1)
  expect(router.state.location.search).toContain('location=us-central1')
  await user.clear(screen.getByLabelText('Location'))
  await user.type(screen.getByLabelText('Filter'), 'bat')
  expect(rows()).toHaveLength(1)
  expect(screen.getByRole('link', { name: 'batch' })).toBeInTheDocument()
})

it('shows each cluster’s mode, Standard or Autopilot, in the list', async () => {
  server.use(
    http.get(`*/container/v1/projects/${P}/locations/-/clusters`, () =>
      HttpResponse.json({ clusters: [cluster(), autopilot()] }),
    ),
  )
  renderApp(`/gke?project=${P}`)
  await screen.findByRole('table', { name: 'Clusters' })
  const [web, auto] = screen.getAllByTestId('cluster')
  expect(web).toHaveTextContent('Standard')
  expect(auto).toHaveTextContent('Autopilot')
})

it('shows an Autopilot cluster’s mode in its details', async () => {
  server.use(getAutopilot)
  renderApp(`${autoBase}?project=${P}`)
  const details = await screen.findByLabelText('Cluster details')
  expect(details).toHaveTextContent('ModeAutopilot')
})

it('creates a cluster, showing the API’s validation messages', async () => {
  const user = userEvent.setup()
  server.use(
    http.post(`*/container/v1/projects/${P}/locations/:loc/clusters`, async ({ request }) => {
      await record(request)
      const body = requests.at(-1)!.body as { cluster: { name: string } }
      if (body.cluster.name === 'taken') {
        return HttpResponse.json(
          { error: { code: 409, status: 'ALREADY_EXISTS', message: 'Already exists: taken.' } },
          { status: 409 },
        )
      }
      return HttpResponse.json(op('CREATE_CLUSTER'))
    }),
  )
  const { router } = renderApp(`/gke/create?project=${P}`)
  const name = await screen.findByLabelText('Name')
  await user.type(name, 'Bad_Name')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(
    await screen.findByText(/Invalid value for field 'cluster.name': "Bad_Name"/),
  ).toBeVisible()
  expect(requests).toHaveLength(0)

  await user.clear(name)
  await user.type(name, 'taken')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(await screen.findByText('Already exists: taken.')).toBeVisible()

  await user.clear(name)
  await user.type(name, 'api')
  await user.clear(screen.getByLabelText('Nodes in the default pool'))
  await user.type(screen.getByLabelText('Nodes in the default pool'), '2')
  await user.type(screen.getByLabelText('Labels'), 'env=dev')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/gke/locations/us-central1-a/clusters/api'),
  )
  expect(requests.at(-1)).toEqual({
    method: 'POST',
    path: `/container/v1/projects/${P}/locations/us-central1-a/clusters`,
    body: {
      cluster: {
        name: 'api',
        initialNodeCount: 2,
        nodeConfig: { machineType: 'e2-medium' },
        workloadIdentityConfig: { workloadPool: `${P}.svc.id.goog` },
        resourceLabels: { env: 'dev' },
      },
    },
  })
  expect(router.state.location.search).toBe(`?project=${P}`)
})

it('creates an Autopilot cluster, with no node pool fields and the API’s message for a zone', async () => {
  const user = userEvent.setup()
  server.use(
    http.post(
      `*/container/v1/projects/${P}/locations/:loc/clusters`,
      async ({ request, params }) => {
        await record(request)
        if (params.loc === 'us-central1-a') {
          return HttpResponse.json(
            {
              error: {
                code: 400,
                status: 'INVALID_ARGUMENT',
                message: 'Autopilot clusters must be regional clusters.',
              },
            },
            { status: 400 },
          )
        }
        return HttpResponse.json(op('CREATE_CLUSTER'))
      },
    ),
  )
  const { router } = renderApp(`/gke/create?project=${P}`)
  await user.type(await screen.findByLabelText('Name'), 'auto')
  expect(screen.queryByLabelText(/Allow NET_ADMIN/)).not.toBeInTheDocument()
  await user.click(screen.getByLabelText('Autopilot'))
  expect(screen.queryByLabelText('Nodes in the default pool')).not.toBeInTheDocument()
  expect(screen.queryByLabelText('Machine type')).not.toBeInTheDocument()
  expect(
    within(screen.getByLabelText('Release channel')).queryByRole('option', { name: 'UNSPECIFIED' }),
  ).not.toBeInTheDocument()

  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(await screen.findByText('Autopilot clusters must be regional clusters.')).toBeVisible()

  const location = screen.getByLabelText('Location')
  await user.clear(location)
  await user.type(location, 'us-central1')
  await user.click(screen.getByLabelText(/Allow NET_ADMIN/))
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/gke/locations/us-central1/clusters/auto'),
  )
  expect(requests.at(-1)).toEqual({
    method: 'POST',
    path: `/container/v1/projects/${P}/locations/us-central1/clusters`,
    body: {
      cluster: {
        name: 'auto',
        autopilot: { enabled: true, workloadPolicyConfig: { allowNetAdmin: true } },
        workloadIdentityConfig: { workloadPool: `${P}.svc.id.goog` },
      },
    },
  })
})

it('edits the create request as JSON, keeping fields the form omits', async () => {
  const user = userEvent.setup()
  server.use(
    http.post(`*/container/v1/projects/${P}/locations/:loc/clusters`, async ({ request }) => {
      await record(request)
      return HttpResponse.json(op('CREATE_CLUSTER'))
    }),
  )
  renderApp(`/gke/create?project=${P}`)
  await user.type(await screen.findByLabelText('Name'), 'json')
  await user.click(screen.getByRole('tab', { name: 'JSON' }))
  const editor = screen.getByLabelText('Request body')
  const body = JSON.parse((editor as HTMLTextAreaElement).value) as { cluster: Cluster }
  expect(body.cluster.name).toBe('json')

  // Invalid JSON keeps the editor open.
  await user.clear(editor)
  await user.type(editor, '{{')
  await user.click(screen.getByRole('tab', { name: 'Form' }))
  expect(screen.getByRole('alert')).toHaveTextContent(/JSON/)

  body.cluster.description = 'from JSON'
  body.cluster.name = 'renamed'
  await user.clear(editor)
  await user.click(editor)
  await user.paste(JSON.stringify(body))
  await user.click(screen.getByRole('tab', { name: 'Form' }))
  expect(screen.getByLabelText('Name')).toHaveValue('renamed')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() => expect(requests).toHaveLength(1))
  expect(requests[0]!.body).toMatchObject({
    cluster: { name: 'renamed', description: 'from JSON' },
  })
})

it('shows a cluster, downloads its kubeconfig and deletes it after confirming', async () => {
  const user = userEvent.setup()
  const createObjectURL = vi.fn((_: Blob) => 'blob:kubeconfig')
  URL.createObjectURL = createObjectURL
  URL.revokeObjectURL = vi.fn()
  const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
  server.use(
    http.delete(
      `*/container/v1/projects/${P}/locations/us-central1-a/clusters/web`,
      async ({ request }) => {
        await record(request)
        return HttpResponse.json(op('DELETE_CLUSTER'))
      },
    ),
  )
  const { router } = renderApp(`${base}?project=${P}`)
  const details = await screen.findByLabelText('Cluster details')
  expect(details).toHaveTextContent('172.18.0.5')
  expect(details).toHaveTextContent('team=web')

  await user.click(screen.getByRole('button', { name: 'Download kubeconfig' }))
  expect(click).toHaveBeenCalled()
  const yaml = await createObjectURL.mock.calls[0]![0].text()
  expect(yaml).toContain('current-context: "gke_alpha-project_us-central1-a_web"')
  click.mockRestore()

  await user.click(screen.getByRole('button', { name: 'Delete' }))
  const dialog = await screen.findByRole('dialog')
  expect(dialog).toHaveTextContent('Delete cluster web?')
  await user.click(within(dialog).getByRole('button', { name: 'Delete cluster' }))
  await waitFor(() => expect(router.state.location.pathname).toBe('/gke'))
  expect(requests).toEqual([
    {
      method: 'DELETE',
      path: `/container/v1/projects/${P}/locations/us-central1-a/clusters/web`,
      body: undefined,
    },
  ])
})

it('edits a cluster: labels through setResourceLabels, then the ClusterUpdate', async () => {
  const user = userEvent.setup()
  server.use(
    http.post(
      `*/container/v1/projects/${P}/locations/us-central1-a/clusters/web\\:setResourceLabels`,
      async ({ request }) => {
        await record(request)
        return HttpResponse.json(op('SET_LABELS'))
      },
    ),
    http.put(
      `*/container/v1/projects/${P}/locations/us-central1-a/clusters/web`,
      async ({ request }) => {
        await record(request)
        return HttpResponse.json(op('UPDATE_CLUSTER'))
      },
    ),
  )
  const { router } = renderApp(`${base}/edit?project=${P}`)
  const labels = await screen.findByLabelText('Labels')
  expect(labels).toHaveValue('team=web')
  await user.type(labels, '\nenv=prod')
  await user.selectOptions(screen.getByLabelText('Release channel'), 'RAPID')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(router.state.location.pathname).toBe(base))
  expect(requests).toEqual([
    {
      method: 'POST',
      path: `/container/v1/projects/${P}/locations/us-central1-a/clusters/web:setResourceLabels`,
      body: { resourceLabels: { team: 'web', env: 'prod' }, labelFingerprint: 'fp1' },
    },
    {
      method: 'PUT',
      path: `/container/v1/projects/${P}/locations/us-central1-a/clusters/web`,
      body: { update: { desiredReleaseChannel: { channel: 'RAPID' } } },
    },
  ])
})

it('offers a Standard cluster’s edit no Autopilot workload policy', async () => {
  renderApp(`${base}/edit?project=${P}`)
  const channel = await screen.findByLabelText('Release channel')
  expect(within(channel).getByRole('option', { name: 'UNSPECIFIED' })).toBeInTheDocument()
  expect(screen.queryByLabelText(/Allow NET_ADMIN/)).not.toBeInTheDocument()
})

it('edits an Autopilot cluster’s NET_ADMIN policy, keeping the rest of it', async () => {
  const user = userEvent.setup()
  server.use(
    http.get(`*/container/v1/projects/${P}/locations/us-central1/clusters/auto`, () =>
      HttpResponse.json(
        autopilot({
          autopilot: {
            enabled: true,
            workloadPolicyConfig: { autopilotCompatibilityAuditingEnabled: true },
          },
        }),
      ),
    ),
    http.put(
      `*/container/v1/projects/${P}/locations/us-central1/clusters/auto`,
      async ({ request }) => {
        await record(request)
        return HttpResponse.json(op('UPDATE_CLUSTER'))
      },
    ),
  )
  const { router } = renderApp(`${autoBase}/edit?project=${P}`)
  const channel = await screen.findByLabelText('Release channel')
  expect(channel).toHaveValue('REGULAR')
  expect(within(channel).queryByRole('option', { name: 'UNSPECIFIED' })).not.toBeInTheDocument()
  const netAdmin = screen.getByLabelText(/Allow NET_ADMIN/)
  expect(netAdmin).not.toBeChecked()
  await user.click(netAdmin)
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(router.state.location.pathname).toBe(autoBase))
  expect(requests).toEqual([
    {
      method: 'PUT',
      path: `/container/v1/projects/${P}/locations/us-central1/clusters/auto`,
      body: {
        update: {
          desiredAutopilotWorkloadPolicyConfig: {
            autopilotCompatibilityAuditingEnabled: true,
            allowNetAdmin: true,
          },
        },
      },
    },
  ])
})

it('resizes a node pool, with the API’s message for a bad size', async () => {
  const user = userEvent.setup()
  server.use(
    http.post(
      `*/container/v1/projects/${P}/locations/us-central1-a/clusters/web/nodePools/default-pool\\:setSize`,
      async ({ request }) => {
        await record(request)
        return HttpResponse.json(op('SET_NODE_POOL_SIZE'))
      },
    ),
  )
  renderApp(`${base}/nodePools?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Node pools' })
  expect(within(table).getByRole('link', { name: 'default-pool' })).toBeInTheDocument()
  await user.click(within(table).getByRole('button', { name: 'Resize default-pool' }))
  const dialog = await screen.findByRole('dialog')
  const count = within(dialog).getByLabelText('Number of nodes')
  await user.clear(count)
  await user.type(count, '-1')
  await user.click(within(dialog).getByRole('button', { name: 'Resize' }))
  expect(await within(dialog).findByText('node_count must be non-negative.')).toBeVisible()
  await user.clear(count)
  await user.type(count, '3')
  await user.click(within(dialog).getByRole('button', { name: 'Resize' }))
  await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  expect(requests.at(-1)?.body).toEqual({ nodeCount: 3 })
})

it('offers no action that changes an Autopilot cluster’s node pools', async () => {
  server.use(
    getAutopilot,
    http.get(`${autoGw}/api/v1/nodes`, () => HttpResponse.json({ items: [] })),
  )
  renderApp(`${autoBase}/nodePools?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Node pools' })
  expect(within(table).getByRole('link', { name: 'default-pool' })).toBeInTheDocument()
  expect(screen.getByText(/These are GKE’s own node pools/)).toBeInTheDocument()
  expect(screen.queryByRole('button', { name: /Resize/ })).not.toBeInTheDocument()
  expect(screen.queryByRole('link', { name: 'Add node pool' })).not.toBeInTheDocument()

  await userEvent.setup().click(within(table).getByRole('link', { name: 'default-pool' }))
  await screen.findByLabelText('Node pool details')
  expect(screen.queryByRole('button', { name: /Resize/ })).not.toBeInTheDocument()
  // The cluster's own Edit and Delete stay; the node pool's are gone.
  expect(screen.getAllByRole('link', { name: 'Edit' })).toHaveLength(1)
  expect(screen.getAllByRole('button', { name: 'Delete' })).toHaveLength(1)
  expect(screen.queryByRole('button', { name: 'Copy node pool as' })).not.toBeInTheDocument()
})

it.each([
  ['create', `${autoBase}/nodePools/create`],
  ['edit', `${autoBase}/nodePools/default-pool/edit`],
])('shows no node pool %s form on an Autopilot cluster', async (_, path) => {
  server.use(getAutopilot)
  renderApp(`${path}?project=${P}`)
  expect(await screen.findByText(/These are GKE’s own node pools/)).toBeInTheDocument()
  expect(screen.queryByRole('textbox')).not.toBeInTheDocument()
  expect(screen.queryByRole('button', { name: /Create|Save/ })).not.toBeInTheDocument()
})

it('lists nodes Autopilot adds, read-only, without a reload', async () => {
  const node = (name: string) => ({
    metadata: {
      name,
      creationTimestamp: new Date().toISOString(),
      labels: {
        'cloud.google.com/gke-nodepool': 'default-pool',
        'topology.kubernetes.io/zone': 'us-central1-a',
      },
    },
    status: { conditions: [{ type: 'Ready', status: 'True' }] },
  })
  const nodes = [node('gk3-auto-default-pool-1')]
  server.use(
    getAutopilot,
    http.get(`${autoGw}/api/v1/nodes`, () => HttpResponse.json({ items: nodes })),
  )
  vi.useFakeTimers({ shouldAdvanceTime: true })
  try {
    renderApp(`${autoBase}/nodes?project=${P}`)
    const table = await screen.findByRole('table', { name: 'Nodes' })
    expect(within(table).getAllByTestId('node')).toHaveLength(1)
    expect(within(table).queryByRole('button')).not.toBeInTheDocument()

    nodes.push(node('gk3-auto-default-pool-2'))
    await vi.advanceTimersByTimeAsync(POLL_MS)
    await waitFor(() => expect(within(table).getAllByTestId('node')).toHaveLength(2))
    expect(table).toHaveTextContent('gk3-auto-default-pool-2')
  } finally {
    vi.useRealTimers()
  }
})

it('creates a node pool with labels and taints', async () => {
  const user = userEvent.setup()
  server.use(
    http.post(
      `*/container/v1/projects/${P}/locations/us-central1-a/clusters/web/nodePools`,
      async ({ request }) => {
        await record(request)
        return HttpResponse.json(op('CREATE_NODE_POOL'))
      },
    ),
  )
  const { router } = renderApp(`${base}/nodePools/create?project=${P}`)
  await user.type(await screen.findByLabelText('Name'), 'gpu')
  await user.type(screen.getByLabelText('Taints'), 'dedicated=gpu')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(await screen.findByText(/Invalid taint "dedicated=gpu"/)).toBeVisible()
  await user.type(screen.getByLabelText('Taints'), ':NoSchedule')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() => expect(router.state.location.pathname).toBe(`${base}/nodePools/gpu`))
  expect(requests.at(-1)?.body).toEqual({
    nodePool: {
      name: 'gpu',
      initialNodeCount: 1,
      config: {
        machineType: 'e2-medium',
        taints: [{ key: 'dedicated', value: 'gpu', effect: 'NO_SCHEDULE' }],
      },
    },
  })
})

it('shows pods with their status through the Connect gateway, and their logs', async () => {
  const user = userEvent.setup()
  server.use(
    http.get(`${gw}/api/v1/namespaces`, () =>
      HttpResponse.json({
        items: [{ metadata: { name: 'default' }, status: { phase: 'Active' } }],
      }),
    ),
    http.get(`${gw}/api/v1/pods`, () =>
      HttpResponse.json({
        items: [
          pod('web-1'),
          pod('web-2', {
            containerStatuses: [
              {
                name: 'app',
                ready: false,
                restartCount: 4,
                state: { waiting: { reason: 'CrashLoopBackOff' } },
              },
              { name: 'sidecar', ready: true, restartCount: 0 },
            ],
          }),
        ],
      }),
    ),
    http.get(`${gw}/api/v1/namespaces/default/pods/web-2`, () => HttpResponse.json(pod('web-2'))),
    http.get(`${gw}/api/v1/namespaces/default/pods/web-2/log`, ({ request }) => {
      const q = new URL(request.url).searchParams
      return HttpResponse.text(`log of ${q.get('container')} (${q.get('tailLines')} lines)\n`)
    }),
  )
  renderApp(`${base}/pods?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Pods' })
  const [ok, crashing] = within(table).getAllByTestId('pod')
  expect(within(ok!).getByText('Running')).toBeInTheDocument()
  expect(within(ok!).getByText('2/2')).toBeInTheDocument()
  expect(within(crashing!).getByText('CrashLoopBackOff')).toBeInTheDocument()
  expect(within(crashing!).getByText('4')).toBeInTheDocument()

  await user.click(within(crashing!).getByRole('link', { name: 'Logs of web-2' }))
  expect(await screen.findByLabelText('Log')).toHaveTextContent('log of app (100 lines)')
  await user.selectOptions(screen.getByLabelText('Container'), 'sidecar')
  // Both selects write the URL; let the first land before the second.
  await waitFor(() => expect(screen.getByLabelText('Container')).toHaveValue('sidecar'))
  await user.selectOptions(screen.getByLabelText('Lines'), '500')
  expect(await screen.findByText(/log of sidecar \(500 lines\)/)).toBeInTheDocument()
})

it('lists workloads of every kind in a namespace', async () => {
  server.use(
    http.get(`${gw}/api/v1/namespaces`, () =>
      HttpResponse.json({ items: [{ metadata: { name: 'kube-system' } }] }),
    ),
    http.get(`${gw}/apis/apps/v1/namespaces/kube-system/deployments`, () =>
      HttpResponse.json({
        items: [
          {
            metadata: { name: 'coredns', namespace: 'kube-system' },
            spec: { replicas: 1 },
            status: { readyReplicas: 1 },
          },
        ],
      }),
    ),
    http.get(`${gw}/apis/apps/v1/namespaces/kube-system/statefulsets`, () =>
      HttpResponse.json({ items: [] }),
    ),
    http.get(`${gw}/apis/apps/v1/namespaces/kube-system/daemonsets`, () =>
      HttpResponse.json({
        items: [
          {
            metadata: { name: 'svclb', namespace: 'kube-system' },
            status: { desiredNumberScheduled: 2, numberReady: 1 },
          },
        ],
      }),
    ),
    http.get(`${gw}/apis/batch/v1/namespaces/kube-system/jobs`, () =>
      HttpResponse.json({ items: [] }),
    ),
  )
  renderApp(`${base}/workloads?project=${P}&namespace=kube-system`)
  const table = await screen.findByRole('table', { name: 'Workloads' })
  const [dns, lb] = within(table).getAllByTestId('workload')
  expect(dns).toHaveTextContent('Deployment')
  expect(within(dns!).getByText('OK')).toBeInTheDocument()
  expect(lb).toHaveTextContent('DaemonSet')
  expect(within(lb!).getByText('Not ready')).toBeInTheDocument()
  expect(within(lb!).getByText('1/2')).toBeInTheDocument()
})

it('lists Services with the host address a LoadBalancer Service is served at', async () => {
  fake.endpoints = { ...fake.endpoints, 'gke:web/default/shop/443': '127.0.0.1:52011' }
  server.use(
    http.get(`${gw}/api/v1/services`, () =>
      HttpResponse.json({
        items: [
          {
            metadata: { name: 'shop', namespace: 'default' },
            spec: {
              type: 'LoadBalancer',
              clusterIP: '34.118.230.10',
              ports: [
                { port: 80, protocol: 'TCP', nodePort: 30080 },
                { port: 443, protocol: 'TCP', nodePort: 30443 },
              ],
            },
            status: { loadBalancer: { ingress: [{ ip: '127.0.0.1' }] } },
          },
          {
            metadata: { name: 'queued', namespace: 'default' },
            spec: { type: 'LoadBalancer', clusterIP: '34.118.230.11', ports: [{ port: 8080 }] },
          },
          {
            metadata: { name: 'kubernetes', namespace: 'default' },
            spec: { type: 'ClusterIP', clusterIP: '34.118.224.1', ports: [{ port: 443 }] },
          },
        ],
      }),
    ),
  )
  renderApp(`${base}/services?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Services' })
  const [api, queued, shop] = within(table).getAllByTestId('service')
  expect(api).toHaveTextContent('ClusterIP')
  expect(shop).toHaveTextContent('80/TCP, 443/TCP')
  await waitFor(() => expect(shop).toHaveTextContent('127.0.0.1:80, 127.0.0.1:52011'))
  expect(queued).toHaveTextContent('Pending')
})

it('explains a Kubernetes API error, such as RBAC refusing the Principal', async () => {
  server.use(
    http.get(`${gw}/api/v1/nodes`, () =>
      HttpResponse.json(
        {
          kind: 'Status',
          status: 'Failure',
          message: 'nodes is forbidden: User "dev@example.com" cannot list resource "nodes"',
          code: 403,
        },
        { status: 403 },
      ),
    ),
  )
  renderApp(`${base}/nodes?project=${P}`)
  expect(await screen.findByRole('alert')).toHaveTextContent(/nodes is forbidden/)
})

it('lists the cluster’s NEGs from the compute API', async () => {
  const neg = (name: string, uid: string) => ({
    name,
    zone: `https://compute.googleapis.com/compute/v1/projects/${P}/zones/us-central1-a`,
    networkEndpointType: 'GCE_VM_IP_PORT',
    size: 2,
    description: JSON.stringify({
      'cluster-uid': uid,
      namespace: 'default',
      'service-name': 'web',
      port: '80',
    }),
  })
  server.use(
    http.get(`*/compute/v1/projects/${P}/aggregated/networkEndpointGroups`, () =>
      HttpResponse.json({
        items: {
          'zones/us-central1-a': {
            networkEndpointGroups: [neg('k8s1-mine', 'abc123'), neg('k8s1-other', 'zzz')],
          },
        },
      }),
    ),
  )
  renderApp(`${base}/negs?project=${P}`)
  const table = await screen.findByRole('table', { name: 'NEGs' })
  const rows = within(table).getAllByTestId('neg')
  expect(rows).toHaveLength(1)
  expect(rows[0]).toHaveTextContent('k8s1-mine')
  expect(rows[0]).toHaveTextContent('default/web')
  expect(rows[0]).toHaveTextContent('us-central1-a')
})

it('waits for a running cluster before reading Kubernetes', async () => {
  server.use(
    http.get(`*/container/v1/projects/${P}/locations/us-central1-a/clusters/web`, () =>
      HttpResponse.json(cluster({ status: 'PROVISIONING' })),
    ),
  )
  renderApp(`${base}/pods?project=${P}`)
  expect(await screen.findByText(/The cluster is PROVISIONING/)).toBeInTheDocument()
  expect(screen.getByRole('button', { name: 'Download kubeconfig' })).toBeDisabled()
})

it('copies a cluster as gcloud, curl and OpenTofu', async () => {
  const user = userEvent.setup()
  renderApp(`${base}?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Copy cluster as' }))
  const dialog = await screen.findByRole('dialog', { name: 'Copy cluster as' })
  const gcloud = [
    'gcloud container clusters create web \\',
    `    --project=${P} \\`,
    '    --location=us-central1-a \\',
    '    --release-channel=regular \\',
    '    --labels=team=web \\',
    '    --num-nodes=1 \\',
    '    --machine-type=e2-medium \\',
    '    --node-labels=tier=a',
  ].join('\n')
  expect(within(dialog).getByLabelText('gcloud for the cluster')).toHaveTextContent(gcloud, {
    normalizeWhitespace: false,
  })

  await user.click(within(dialog).getByRole('tab', { name: 'curl' }))
  const curl = within(dialog).getByLabelText('curl for the cluster')
  expect(curl).toHaveTextContent(
    `curl ${location.origin}/container/v1/projects/${P}/locations/us-central1-a/clusters \\ -X POST`,
  )
  expect(curl).toHaveTextContent('"cluster": { "name": "web"')
  expect(curl).not.toHaveTextContent('"endpoint"')
  expect(curl).not.toHaveTextContent('"status"')

  await user.click(within(dialog).getByRole('tab', { name: 'OpenTofu' }))
  const tofu = within(dialog).getByLabelText('OpenTofu for the cluster')
  expect(tofu).toHaveTextContent('resource "google_container_cluster" "web" {')
  expect(tofu).toHaveTextContent('resource "google_container_node_pool" "default-pool" {')
  expect(tofu).toHaveTextContent('cluster = google_container_cluster.web.name')
})

it('copies a node pool from its page', async () => {
  const user = userEvent.setup()
  renderApp(`${base}/nodePools/default-pool?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Copy node pool as' }))
  const dialog = await screen.findByRole('dialog', { name: 'Copy node pool as' })
  const gcloud = [
    'gcloud container node-pools create default-pool \\',
    `    --project=${P} \\`,
    '    --cluster=web \\',
    '    --location=us-central1-a \\',
    '    --node-version=1.36.5-gke.100 \\',
    '    --num-nodes=1 \\',
    '    --machine-type=e2-medium \\',
    '    --node-labels=tier=a',
  ].join('\n')
  expect(within(dialog).getByLabelText('gcloud for the node pool')).toHaveTextContent(gcloud, {
    normalizeWhitespace: false,
  })
  await user.click(within(dialog).getByRole('tab', { name: 'curl' }))
  const curl = within(dialog).getByLabelText('curl for the node pool')
  expect(curl).toHaveTextContent(
    `curl ${location.origin}/container/v1/projects/${P}/locations/us-central1-a/clusters/web/nodePools \\ -X POST`,
  )
  expect(curl).toHaveTextContent('"nodePool": { "name": "default-pool"')
  await user.click(within(dialog).getByRole('tab', { name: 'OpenTofu' }))
  expect(within(dialog).getByLabelText('OpenTofu for the node pool')).toHaveTextContent(
    'resource "google_container_node_pool" "default-pool" {',
  )
})

it('copies a private cluster with several node pools', () => {
  const c = {
    ...cluster({
      description: 'API servers',
      locations: ['us-central1-a', 'us-central1-b'],
      initialClusterVersion: '1.36.5-gke.100',
      network: 'vpc',
      subnetwork: 'nodes',
      workloadIdentityConfig: { workloadPool: `${P}.svc.id.goog` },
      privateClusterConfig: { enablePrivateNodes: true },
      loggingService: undefined,
      monitoringService: undefined,
      nodePools: [
        {
          name: 'default-pool',
          status: 'RUNNING',
          initialNodeCount: 2,
          config: {
            machineType: 'e2-standard-4',
            diskSizeGb: 50,
            taints: [{ key: 'dedicated', value: 'api', effect: 'NO_SCHEDULE' }],
          },
          autoscaling: { enabled: true, minNodeCount: 1, maxNodeCount: 3 },
        },
        {
          name: 'spot',
          status: 'RUNNING',
          initialNodeCount: 1,
          locations: ['us-central1-b'],
          config: { machineType: 'e2-small' },
        },
      ],
    }),
    // The default pool's legacy fields, which clusters.create refuses with nodePools.
    initialNodeCount: 2,
  } as Cluster
  const r = clusterRecipe(P, c)
  expect(r.gcloud).toEqual([
    [
      'gcloud container clusters create web \\',
      `    --project=${P} \\`,
      '    --location=us-central1-a \\',
      '    --node-locations=us-central1-a,us-central1-b \\',
      '    --cluster-version=1.36.5-gke.100 \\',
      '    --release-channel=regular \\',
      '    --network=vpc \\',
      '    --subnetwork=nodes \\',
      '    --labels=team=web \\',
      `    --workload-pool=${P}.svc.id.goog \\`,
      '    --enable-private-nodes \\',
      '    --num-nodes=2 \\',
      '    --machine-type=e2-standard-4 \\',
      '    --disk-size=50 \\',
      '    --node-taints=dedicated=api:NoSchedule \\',
      '    --enable-autoscaling \\',
      '    --min-nodes=1 \\',
      '    --max-nodes=3',
    ].join('\n'),
    [
      'gcloud container node-pools create spot \\',
      `    --project=${P} \\`,
      '    --cluster=web \\',
      '    --location=us-central1-a \\',
      '    --node-locations=us-central1-b \\',
      '    --num-nodes=1 \\',
      '    --machine-type=e2-small',
    ].join('\n'),
  ])
  expect(r.rest).toEqual({
    method: 'POST',
    path: `/container/v1/projects/${P}/locations/us-central1-a/clusters`,
    body: {
      cluster: {
        name: 'web',
        description: 'API servers',
        locations: ['us-central1-a', 'us-central1-b'],
        initialClusterVersion: '1.36.5-gke.100',
        network: 'vpc',
        subnetwork: 'nodes',
        releaseChannel: { channel: 'REGULAR' },
        resourceLabels: { team: 'web' },
        workloadIdentityConfig: { workloadPool: `${P}.svc.id.goog` },
        privateClusterConfig: { enablePrivateNodes: true },
        nodePools: [
          {
            name: 'default-pool',
            initialNodeCount: 2,
            config: c.nodePools![0]!.config,
            autoscaling: { enabled: true, minNodeCount: 1, maxNodeCount: 3 },
          },
          {
            name: 'spot',
            initialNodeCount: 1,
            locations: ['us-central1-b'],
            config: { machineType: 'e2-small' },
          },
        ],
      },
    },
  })
  expect(r.tofu).toContain(
    [
      '  private_cluster_config {',
      '    enable_private_nodes    = true',
      '    enable_private_endpoint = false',
      '  }',
    ].join('\n'),
  )
  expect(r.tofu).toContain(
    [
      'resource "google_container_node_pool" "spot" {',
      `  project            = "${P}"`,
      '  name               = "spot"',
      '  cluster            = google_container_cluster.web.name',
      '  location           = "us-central1-a"',
      '  node_locations     = ["us-central1-b"]',
      '  initial_node_count = 1',
      '  node_config {',
      '    machine_type = "e2-small"',
      '  }',
      '}',
    ].join('\n'),
  )
  // A standalone node pool names its cluster, and inherits its zones.
  const pool = nodePoolRecipe(
    { project: P, location: 'us-central1-a', cluster: 'web' },
    c.nodePools![0]!,
    c.locations,
  )
  expect(pool.tofu).toContain('  cluster            = "web"')
  expect(pool.tofu).not.toContain('node_locations')
  expect(pool.tofu).toContain(
    [
      '  autoscaling {',
      '    min_node_count = 1',
      '    max_node_count = 3',
      '  }',
      '  node_config {',
      '    machine_type = "e2-standard-4"',
      '    disk_size_gb = 50',
      '    taint {',
      '      key    = "dedicated"',
      '      value  = "api"',
      '      effect = "NO_SCHEDULE"',
      '    }',
      '  }',
    ].join('\n'),
  )
})
