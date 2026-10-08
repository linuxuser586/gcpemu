import { readFileSync } from 'node:fs'

import type { APIRequestContext, Page } from '@playwright/test'

import { expect, projectId, test } from './fixtures'

// The GKE view (SRS 4.8.3, FR-UI-011) against real k3s clusters. The
// Instance runs gke on Linux (emulator.ts); clusters need a container
// runtime, so the tests that create one skip without it.

const ZONE = 'us-central1-a'
const allowed = /^\/(console\/|_emu\/v1\/|[a-z]+\/v\d[a-z0-9]*\/)/

async function runtime(request: APIRequestContext) {
  const info = (await (await request.get('/_emu/v1/info')).json()) as {
    services: string[]
    runtime?: { reachable?: boolean }
  }
  return { gke: info.services.includes('gke'), reachable: !!info.runtime?.reachable }
}

const tab = (page: Page, name: string) =>
  page.getByRole('navigation', { name: 'Cluster' }).getByRole('link', { name, exact: true })

test('GKE: the create form shows the API’s validation messages', async ({
  page,
  request,
  browserName,
}) => {
  test.skip(!(await runtime(request)).gke, 'GKE runs on Linux only')
  const id = projectId('e2e-gkeform', browserName)
  await page.goto(`/console/gke?project=${id}`)
  await expect(page.getByText('No clusters in this Project yet.')).toBeVisible()
  await page.getByRole('link', { name: 'Create cluster' }).click()

  await page.getByLabel('Name').fill('Bad_Name')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page.getByText(/Invalid value for field 'cluster.name': "Bad_Name"/)).toBeVisible()

  // The API rejects what the form cannot know is wrong.
  await page.getByLabel('Name').fill('nowhere')
  await page.getByLabel('Location', { exact: true }).fill('mars-north1-a')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page.getByRole('alert').filter({ hasText: /mars-north1-a/ })).toBeVisible()

  // The JSON editor holds the whole request body.
  await page.getByRole('tab', { name: 'JSON' }).click()
  const body = JSON.parse(await page.getByLabel('Request body').inputValue()) as {
    cluster: { name: string }
  }
  expect(body.cluster.name).toBe('nowhere')
})

test('GKE: a cluster’s lifecycle, Kubernetes objects and actions', async ({
  page,
  request,
  browserName,
  requests,
}, testInfo) => {
  const rt = await runtime(request)
  test.skip(!rt.gke || !rt.reachable, 'needs GKE and a container runtime')
  test.setTimeout(10 * 60_000)
  const id = projectId('e2e-gke', browserName)
  const name = `c${testInfo.retry}`
  const gateway = `/connectgateway/v1/projects/${id}/locations/${ZONE}/gkeMemberships/${name}`

  // Create (FR-GKE-001) with the form.
  await page.goto(`/console/gke/create?project=${id}`)
  await page.getByLabel('Name').fill(name)
  await page.getByLabel('Location', { exact: true }).fill(ZONE)
  await page.getByLabel('Nodes in the default pool').fill('1')
  await page.getByLabel('Labels').fill('team=e2e')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(
    new RegExp(`/console/gke/locations/${ZONE}/clusters/${name}\\?project=${id}$`),
  )
  const heading = page.getByRole('heading', { level: 1 })
  await expect(heading).toContainText(name)
  await expect(heading.getByText('RUNNING')).toBeVisible({ timeout: 4 * 60_000 })
  const details = page.getByLabel('Cluster details')
  await expect(details).toContainText('team=e2e')
  await expect(details).toContainText('Endpoint')

  // Download kubeconfig.
  const download = page.waitForEvent('download')
  await page.getByRole('button', { name: 'Download kubeconfig' }).click()
  const file = await download
  expect(file.suggestedFilename()).toBe(`kubeconfig-gke_${id}_${ZONE}_${name}.yaml`)
  const kubeconfig = readFileSync(await file.path(), 'utf8')
  expect(kubeconfig).toContain('command: gke-gcloud-auth-plugin')
  expect(kubeconfig).toMatch(/server: "https:\/\/[\d.]+(:\d+)?"/)

  // Nodes, namespaces and workloads through the Connect gateway.
  await tab(page, 'Nodes').click()
  const nodes = page.getByRole('table', { name: 'Nodes' }).getByTestId('node')
  await expect(nodes).toHaveCount(1, { timeout: 2 * 60_000 })
  await expect(nodes.first()).toContainText('Ready', { timeout: 2 * 60_000 })
  await expect(nodes.first()).toContainText('default-pool')

  await tab(page, 'Namespaces').click()
  const kubeSystem = page.getByTestId('namespace').filter({ hasText: 'kube-system' })
  await expect(kubeSystem).toContainText('Active')
  await kubeSystem.getByRole('link', { name: 'Workloads' }).click()
  await expect(page).toHaveURL(/\/workloads\?.*namespace=kube-system/)
  const coredns = page.getByTestId('workload').filter({ hasText: 'coredns' })
  await expect(coredns).toContainText('Deployment')
  await expect(coredns).toContainText('OK', { timeout: 2 * 60_000 })

  // A pod and a NEG-annotated Service (FR-GKE-008), created as kubectl
  // would through the same gateway.
  const pod = await request.post(`${gateway}/api/v1/namespaces/default/pods`, {
    data: {
      apiVersion: 'v1',
      kind: 'Pod',
      metadata: { name: 'echo', labels: { app: 'echo' } },
      spec: {
        containers: [
          {
            name: 'echo',
            image: 'rancher/mirrored-library-busybox:1.37.0',
            imagePullPolicy: 'IfNotPresent',
            command: ['sh', '-c', 'echo hello-from-gcpemu; exec sleep 3600'],
          },
        ],
      },
    },
  })
  expect(pod.status(), await pod.text()).toBe(201)
  const svc = await request.post(`${gateway}/api/v1/namespaces/default/services`, {
    data: {
      apiVersion: 'v1',
      kind: 'Service',
      metadata: {
        name: 'echo',
        annotations: { 'cloud.google.com/neg': '{"exposed_ports": {"80": {}}}' },
      },
      spec: { selector: { app: 'echo' }, ports: [{ port: 80, targetPort: 8080 }] },
    },
  })
  expect(svc.status(), await svc.text()).toBe(201)

  await tab(page, 'Pods').click()
  await page.getByLabel('Namespace').selectOption('default')
  const echo = page.getByTestId('pod').filter({ hasText: 'echo' })
  await expect(echo).toContainText('Running', { timeout: 2 * 60_000 })
  await echo.getByRole('link', { name: 'Logs of echo' }).click()
  await expect(page.getByLabel('Log', { exact: true })).toContainText('hello-from-gcpemu')

  await tab(page, 'NEGs').click()
  const neg = page.getByTestId('neg')
  await expect(neg).toHaveCount(1, { timeout: 60_000 })
  await expect(neg).toContainText('default/echo')
  await expect(neg).toContainText(ZONE)

  // Edit (labels through setResourceLabels).
  await page.getByRole('link', { name: 'Edit', exact: true }).click()
  await page.getByLabel('Labels').fill('team=e2e\nenv=test')
  await page.getByRole('button', { name: 'Save' }).click()
  await expect(details).toContainText('env=test', { timeout: 60_000 })

  // Node pools: create one through the JSON editor, edit and delete it.
  await tab(page, 'Node pools').click()
  await page.getByRole('link', { name: 'Add node pool' }).click()
  await page.getByRole('tab', { name: 'JSON' }).click()
  await page.getByLabel('Request body').fill(
    JSON.stringify({
      nodePool: { name: 'extra', initialNodeCount: 0, config: { labels: { x: 'y' } } },
    }),
  )
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(new RegExp(`/nodePools/extra\\?`))
  await expect(
    page.getByRole('heading', { name: /Node pool extra/ }).getByText('RUNNING'),
  ).toBeVisible({
    timeout: 60_000,
  })
  await expect(page.getByLabel('Node pool details')).toContainText('x=y')
  await page.getByRole('link', { name: 'Edit', exact: true }).last().click()
  await page.getByLabel('Kubernetes labels').fill('x=z')
  await page.getByRole('button', { name: 'Save' }).click()
  await expect(page.getByLabel('Node pool details')).toContainText('x=z', { timeout: 60_000 })
  await page.getByRole('button', { name: 'Delete' }).last().click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete node pool' }).click()
  await expect(page).toHaveURL(/\/nodePools\?/)
  await expect(page.getByTestId('node-pool').filter({ hasText: 'extra' })).toHaveCount(0, {
    timeout: 60_000,
  })

  // Resize (FR-GKE-003): a node container is added.
  await page.getByRole('button', { name: 'Resize default-pool' }).click()
  await page.getByRole('dialog').getByLabel('Number of nodes').fill('2')
  await page.getByRole('dialog').getByRole('button', { name: 'Resize' }).click()
  await tab(page, 'Nodes').click()
  await expect(nodes).toHaveCount(2, { timeout: 3 * 60_000 })

  // Delete.
  await page.getByRole('button', { name: 'Delete', exact: true }).first().click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete cluster' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/gke\\?project=${id}`))
  await expect(page.getByTestId('cluster').filter({ hasText: name })).toHaveCount(0, {
    timeout: 3 * 60_000,
  })

  // FR-UI-003, 004: only public APIs and /_emu/v1, without credentials.
  for (const r of requests) {
    const url = new URL(r.url())
    // The kubeconfig is saved from a blob: URL.
    if (url.protocol === 'blob:') continue
    expect(url.pathname, r.url()).toMatch(allowed)
    expect(await r.headerValue('authorization'), r.url()).toBeNull()
  }
  expect(requests.some((r) => new URL(r.url()).pathname.startsWith('/connectgateway/v1/'))).toBe(
    true,
  )
})
