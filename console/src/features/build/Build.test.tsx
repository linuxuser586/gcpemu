import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, expect, it } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import type { Build, BuildTrigger } from './api'

const P = 'alpha-project'
const G = `projects/${P}/locations/global`

// A small in-memory Cloud Build: Builds and Build triggers by location
// over cloudbuild/v1 REST, each Build's log and the Build daemon over the
// Admin API, and the requests the view sends.
let builds: Record<string, Build[]> = {}
let triggers: Record<string, BuildTrigger[]> | undefined
let logs: Record<string, string> = {}
// objects are Cloud Storage objects by bucket/name.
let objects: Record<string, string> = {}
let daemon = { running: true, cacheBytes: 3 * 1024 * 1024, privilegedRefused: false }
let requests: { method: string; path: string }[] = []

const err = (code: number, status: string, message: string) =>
  HttpResponse.json({ error: { code, status, message } }, { status: code })

const build = (id: string, over: Partial<Build> = {}): Build => ({
  name: `${G}/builds/${id}`,
  id,
  projectId: P,
  status: 'SUCCESS',
  createTime: '2026-10-20T10:00:00Z',
  startTime: '2026-10-20T10:00:01Z',
  finishTime: '2026-10-20T10:01:06Z',
  serviceAccount: `projects/${P}/serviceAccounts/111111111111-compute@developer.gserviceaccount.com`,
  source: { storageSource: { bucket: `${P}_cloudbuild`, object: 'source/app.tgz' } },
  steps: [
    {
      name: 'golang',
      id: 'compile',
      status: 'SUCCESS',
      timing: { startTime: '2026-10-20T10:00:02Z', endTime: '2026-10-20T10:00:40Z' },
    },
    {
      name: 'gcr.io/cloud-builders/docker',
      status: 'SUCCESS',
      timing: { startTime: '2026-10-20T10:00:40Z', endTime: '2026-10-20T10:01:05Z' },
    },
  ],
  ...over,
})

beforeEach(() => {
  requests = []
  fake.info.services = ['iam', 'build', 'gcs']
  builds = {
    global: [
      build('b-running', {
        status: 'WORKING',
        finishTime: undefined,
        buildTriggerId: 'nightly',
        steps: [
          {
            name: 'golang',
            id: 'compile',
            status: 'WORKING',
            timing: { startTime: '2026-10-20T10:00:02Z' },
          },
          { name: 'gcr.io/cloud-builders/docker', status: 'QUEUED' },
        ],
      }),
      build('b-done'),
    ],
    'us-central1': [
      { ...build('b-regional'), name: `projects/${P}/locations/us-central1/builds/b-regional` },
    ],
  }
  triggers = {
    global: [
      { id: 'nightly', name: 'nightly', pubsubConfig: { topic: `projects/${P}/topics/nightly` } },
      {
        id: 'manual',
        name: 'manual',
        sourceToBuild: { uri: 'https://example.com/r.git', ref: 'main' },
      },
    ],
  }
  logs = {
    'b-running': 'BUILD\nStarting Step #0 - "compile"\nStep #0 - "compile": compiling\n',
    'b-done': 'BUILD\nStep #0 - "compile": compiled\nStep #1: pushed\nDONE\n',
  }
  objects = {}
  daemon = { running: true, cacheBytes: 3 * 1024 * 1024, privilegedRefused: false }
  server.use(
    http.all(/\/cloudbuild\/v1\/(.*)$/, async ({ request }) => {
      const url = new URL(request.url)
      const path = decodeURIComponent(url.pathname.replace(/^.*\/cloudbuild\/v1\//, ''))
      requests.push({ method: request.method, path })
      const m =
        /^projects\/[^/]+\/locations\/([^/]+)\/(builds|triggers)(?:\/([^/:]+))?(?::(\w+))?$/.exec(
          path,
        )
      if (!m) return err(400, 'INVALID_ARGUMENT', `bad path ${path}`)
      const [, loc = '', kind, id, verb] = m
      if (kind === 'triggers') {
        // The API's field behaviour requires project_id on Build trigger calls.
        const text = request.method === 'POST' ? await request.text() : ''
        const projectId = id
          ? (JSON.parse(text || '{}') as { projectId?: string }).projectId
          : url.searchParams.get('projectId')
        if (projectId !== P)
          return err(400, 'INVALID_ARGUMENT', 'Missing required field: project_id.')
        if (!triggers) return err(501, 'UNIMPLEMENTED', 'method ListBuildTriggers not implemented')
        const list = triggers[loc] ?? []
        if (!id) return HttpResponse.json({ triggers: list })
        const t = list.find((x) => x.id === id)
        if (!t) return err(404, 'NOT_FOUND', `Build trigger ${id} not found.`)
        const b = build(`b-${id}`, { status: 'QUEUED', buildTriggerId: id, finishTime: undefined })
        ;(builds[loc] ??= []).unshift(b)
        return HttpResponse.json({ name: `${G}/operations/op-run`, metadata: { build: b } })
      }
      const list = builds[loc] ?? []
      if (!id) return HttpResponse.json({ builds: list })
      const b = list.find((x) => x.id === id)
      if (!b) return err(404, 'NOT_FOUND', `Build ${id} not found.`)
      if (verb === 'cancel') {
        b.status = 'CANCELLED'
        return HttpResponse.json(b)
      }
      if (verb === 'retry') {
        const nb = build(`${id}-retry`, { status: 'QUEUED', finishTime: undefined })
        list.unshift(nb)
        return HttpResponse.json({ name: `${G}/operations/op-retry`, metadata: { build: nb } })
      }
      return HttpResponse.json(b)
    }),
    http.get(
      /\/_emu\/v1\/build\/projects\/([^/]+)\/locations\/([^/]+)\/builds\/([^/]+)\/log$/,
      ({ request }) => {
        const id = decodeURIComponent(new URL(request.url).pathname.split('/').at(-2) ?? '')
        requests.push({ method: 'GET', path: `log/${id}` })
        return new HttpResponse(logs[id] ?? '', { headers: { 'Content-Type': 'text/plain' } })
      },
    ),
    http.get(/\/storage\/v1\/b\/([^/]+)\/o\/([^/?]+)$/, ({ request }) => {
      const url = new URL(request.url)
      const [, , , , bucket = '', , object = ''] = url.pathname.split('/')
      requests.push({ method: 'GET', path: `gcs/${bucket}/${decodeURIComponent(object)}` })
      const text = objects[`${bucket}/${decodeURIComponent(object)}`]
      return text === undefined || url.searchParams.get('alt') !== 'media'
        ? err(404, 'NOT_FOUND', 'No such object.')
        : new HttpResponse(text, { headers: { 'Content-Type': 'text/plain' } })
    }),
    http.get(/\/_emu\/v1\/build\/daemon$/, () => HttpResponse.json(daemon)),
    http.post(/\/_emu\/v1\/build\/daemon\/prune$/, () => {
      requests.push({ method: 'POST', path: 'prune' })
      const reclaimedBytes = daemon.cacheBytes
      daemon = { ...daemon, cacheBytes: 0 }
      return HttpResponse.json({ reclaimedBytes, cacheBytes: 0 })
    }),
  )
})

const sent = (method: string, path: RegExp) =>
  requests.filter((r) => r.method === method && path.test(r.path))

it('says how to enable Cloud Build when it is off', async () => {
  fake.info.services = ['iam']
  renderApp(`/build?project=${P}`)
  expect(
    await screen.findByRole('heading', { name: /Service build is not enabled/ }),
  ).toBeInTheDocument()
})

it('asks for a Project before listing Builds', async () => {
  renderApp('/build')
  expect(await screen.findByText(/Choose a Project/)).toBeInTheDocument()
})

it('lists a location’s Builds with status, duration, source, trigger and service account', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/build?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Builds' })
  const rows = within(table).getAllByTestId('build')
  expect(rows.map((r) => r.textContent)).toEqual([
    expect.stringContaining('b-running'),
    expect.stringContaining('b-done'),
  ])
  expect(rows[0]).toHaveTextContent('WORKING')
  expect(rows[0]).toHaveTextContent('nightly')
  expect(rows[1]).toHaveTextContent('SUCCESS')
  expect(rows[1]).toHaveTextContent('1m 05s')
  expect(rows[1]).toHaveTextContent(`gs://${P}_cloudbuild/source/app.tgz`)
  expect(rows[1]).toHaveTextContent('111111111111-compute@developer.gserviceaccount.com')

  await user.clear(screen.getByLabelText('Location'))
  await user.type(screen.getByLabelText('Location'), 'us-central1')
  await user.click(screen.getByRole('button', { name: 'Show' }))
  expect(router.state.location.search).toContain('location=us-central1')
  expect(await screen.findByText('b-regional')).toBeInTheDocument()
})

it('opens a Build from its logUrl with its steps’ status, timing and logs', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/build/projects/${P}/locations/global/builds/b-done`)
  expect(await screen.findByRole('heading', { name: 'b-done' })).toBeInTheDocument()
  expect(router.state.location.search).toContain(`project=${P}`)
  const steps = within(screen.getByRole('table', { name: 'Steps' })).getAllByTestId('step')
  expect(steps[0]).toHaveTextContent('compile')
  expect(steps[0]).toHaveTextContent('golang')
  expect(steps[0]).toHaveTextContent('SUCCESS')
  expect(steps[0]).toHaveTextContent('38s')
  expect(steps[1]).toHaveTextContent('gcr.io/cloud-builders/docker')
  expect(steps[1]).toHaveTextContent('25s')

  const log = await screen.findByLabelText('Build log')
  await waitFor(() => expect(log).toHaveTextContent('DONE'))
  await user.click(within(steps[0]!).getByRole('button', { name: 'Show the log of step 0' }))
  expect(router.state.location.search).toContain('step=0')
  expect(screen.getByLabelText('Log of step 0')).toHaveTextContent(/^compiled$/)
  await user.click(screen.getByRole('button', { name: 'Show the whole log' }))
  expect(screen.getByLabelText('Build log')).toHaveTextContent('pushed')
})

it('updates a running Build’s step log live and moves it to its final status', async () => {
  const { queryClient } = renderApp(
    `/build/projects/${P}/locations/global/builds/b-running?project=${P}&step=0`,
  )
  const log = await screen.findByLabelText('Log of step 0')
  await waitFor(() => expect(log).toHaveTextContent('compiling'))
  expect(screen.getByRole('button', { name: 'Cancel Build' })).toBeInTheDocument()

  logs['b-running'] += 'Step #0 - "compile": linked\n'
  await waitFor(() => expect(screen.getByLabelText('Log of step 0')).toHaveTextContent('linked'))

  // The build Service writes the final status; the event bridge marks
  // the Service's queries stale.
  const b = builds.global![0]!
  b.status = 'SUCCESS'
  b.finishTime = '2026-10-20T10:01:06Z'
  b.steps![0]!.status = 'SUCCESS'
  await queryClient.invalidateQueries({ queryKey: ['build'] })
  expect(await screen.findByRole('button', { name: 'Retry Build' })).toBeInTheDocument()
  expect(screen.queryByRole('button', { name: 'Cancel Build' })).not.toBeInTheDocument()
  expect(screen.getByTestId('build-status')).toHaveTextContent('SUCCESS')
})

it('cancels a running Build and retries it as a new Build', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(
    `/build/projects/${P}/locations/global/builds/b-running?project=${P}`,
  )
  await user.click(await screen.findByRole('button', { name: 'Cancel Build' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Cancel Build' }),
  )
  await waitFor(() => expect(screen.getByTestId('build-status')).toHaveTextContent('CANCELLED'))
  expect(sent('POST', /builds\/b-running:cancel$/)).toHaveLength(1)

  await user.click(screen.getByRole('button', { name: 'Retry Build' }))
  expect(await screen.findByRole('heading', { name: 'b-running-retry' })).toBeInTheDocument()
  expect(sent('POST', /builds\/b-running:retry$/)).toHaveLength(1)
  expect(router.state.location.pathname).toBe(
    `/build/projects/${P}/locations/global/builds/b-running-retry`,
  )
})

it('reads a finished Build’s log object once the Instance’s copy is gone', async () => {
  // A restart or Reset empties the in-memory log; the log object stays.
  logs['b-done'] = ''
  builds.global![1]!.logsBucket = 'gs://logs-bucket/ci'
  objects['logs-bucket/ci/log-b-done.txt'] = 'BUILD\nStep #0 - "compile": from the object\nDONE\n'
  renderApp(`/build/projects/${P}/locations/global/builds/b-done?project=${P}&step=0`)
  const log = await screen.findByLabelText('Log of step 0')
  await waitFor(() => expect(log).toHaveTextContent(/^from the object$/))
  expect(sent('GET', /^gcs\/logs-bucket\/ci\/log-b-done\.txt$/)).toHaveLength(1)
})

it('says when a Build does not exist', async () => {
  renderApp(`/build/projects/${P}/locations/global/builds/nope?project=${P}`)
  expect(await screen.findByRole('alert')).toHaveTextContent('Build nope not found.')
})

it('lists Build triggers and runs one, landing on its Build', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/build?project=${P}`)
  await user.click(await screen.findByRole('link', { name: 'Build triggers' }))
  const table = await screen.findByRole('table', { name: 'Build triggers' })
  const rows = within(table).getAllByTestId('trigger')
  expect(rows[0]).toHaveTextContent('nightly')
  expect(rows[0]).toHaveTextContent('Pub/Sub')
  expect(rows[0]).toHaveTextContent(`projects/${P}/topics/nightly`)
  expect(rows[1]).toHaveTextContent('Manual')

  await user.click(within(rows[1]!).getByRole('button', { name: 'Run Build trigger manual' }))
  expect(await screen.findByRole('heading', { name: 'b-manual' })).toBeInTheDocument()
  expect(sent('POST', /triggers\/manual:run$/)).toHaveLength(1)
  expect(router.state.location.pathname).toBe(
    `/build/projects/${P}/locations/global/builds/b-manual`,
  )
})

it('says when the Instance does not emulate Build triggers', async () => {
  triggers = undefined
  renderApp(`/build/triggers?project=${P}`)
  expect(
    await screen.findByText('Build triggers are not available on this Instance yet.'),
  ).toBeInTheDocument()
})

it('shows the Build daemon’s cache and prunes it', async () => {
  const user = userEvent.setup()
  renderApp(`/build/daemon?project=${P}`)
  const details = await screen.findByLabelText('Build daemon details')
  await waitFor(() => expect(details).toHaveTextContent('3.0 MiB'))
  expect(details).toHaveTextContent('Running')

  await user.click(screen.getByRole('button', { name: 'Prune cache' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Prune cache' }),
  )
  expect(await screen.findByText('Pruned the Build daemon: freed 3.0 MiB.')).toBeInTheDocument()
  await waitFor(() => expect(details).toHaveTextContent('0 B'))
  expect(sent('POST', /^prune$/)).toHaveLength(1)
})

it('says why the Build daemon cannot run', async () => {
  daemon = { running: false, cacheBytes: 0, privilegedRefused: true }
  renderApp(`/build/daemon`)
  expect(
    await screen.findByText(
      /The container runtime refused the Build daemon’s privileged container/,
    ),
  ).toBeInTheDocument()
})
