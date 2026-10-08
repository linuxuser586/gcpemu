import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, expect, it, vi } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import type { Cluster, KubePod } from './api'

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
