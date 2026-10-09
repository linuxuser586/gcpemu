import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, expect, it } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import { decodePayload, encodePayload, type Secret, type SecretVersion } from './api'
import { patchBody, updateMask } from './SecretForm'

const P = 'alpha-project'
const N = 'projects/111111111111'

// A small in-memory Secret Manager REST API: secrets in two locations,
// versions with payloads, and the requests the view sends.
let secrets: Record<string, Secret[]> = {}
let versions: Record<string, (SecretVersion & { data: string })[]> = {}
let requests: { method: string; path: string; search: URLSearchParams; body: unknown }[] = []

const err = (code: number, status: string, message: string) =>
  HttpResponse.json({ error: { code, status, message } }, { status: code })

const secret = (id: string, over: Partial<Secret> = {}): Secret => ({
  name: `${N}/secrets/${id}`,
  replication: { automatic: {} },
  createTime: '2026-10-01T10:00:00Z',
  etag: '"1"',
  ...over,
})

beforeEach(() => {
  requests = []
  fake.info.services = ['iam', 'secrets', 'pubsub']
  secrets = {
    '': [
      secret('db-password', { labels: { team: 'web' }, versionAliases: { prod: '1' } }),
      secret('api-key', { expireTime: '2030-01-01T00:00:00Z' }),
    ],
    'us-central1': [
      {
        name: `${N}/locations/us-central1/secrets/app-db`,
        secretType: 'CLOUD_SQL_DB_CREDENTIALS',
        rotation: { managedRotationStatus: { state: 'ACTIVE' } },
      },
    ],
  }
  versions = {
    'db-password': [
      {
        name: `${N}/secrets/db-password/versions/2`,
        state: 'ENABLED',
        data: encodePayload('s3cret-v2'),
      },
      {
        name: `${N}/secrets/db-password/versions/1`,
        state: 'DISABLED',
        data: encodePayload('old'),
      },
    ],
    'app-db': [
      {
        name: `${N}/locations/us-central1/secrets/app-db/versions/1`,
        state: 'ENABLED',
        data: encodePayload('pw'),
      },
    ],
  }
  server.use(
    http.all(/\/secretmanager\/v1\/(.*)$/, async ({ request }) => {
      const url = new URL(request.url)
      const text = request.method === 'GET' ? '' : await request.text()
      const body: unknown = text ? JSON.parse(text) : undefined
      const path = decodeURIComponent(url.pathname.replace(/^.*\/secretmanager\/v1\//, ''))
      requests.push({ method: request.method, path, search: url.searchParams, body })
      const m =
        /^projects\/[^/]+(?:\/locations\/([^/]+))?(?:\/secrets(?:\/([^/:]+))?(?:\/versions(?:\/([^/:]+))?)?)?(?::(\w+))?$/.exec(
          path,
        )
      if (path === `projects/${P}/locations`) {
        return HttpResponse.json({
          locations: [
            {
              name: `projects/${P}/locations/us-central1`,
              locationId: 'us-central1',
              displayName: 'Iowa',
            },
            { name: `projects/${P}/locations/europe-west1`, locationId: 'europe-west1' },
          ],
        })
      }
      if (!m) return err(400, 'INVALID_ARGUMENT', `bad path ${path}`)
      const [, loc = '', id, ver, verb] = m
      const list = (secrets[loc] ??= [])
      if (!id) {
        if (request.method === 'GET') return HttpResponse.json({ secrets: list })
        const sid = url.searchParams.get('secretId') ?? ''
        if (list.some((s) => s.name.endsWith(`/secrets/${sid}`))) {
          return err(409, 'ALREADY_EXISTS', `Secret [${N}/secrets/${sid}] already exists.`)
        }
        const s = {
          ...(body as Secret),
          name: `${N}${loc ? `/locations/${loc}` : ''}/secrets/${sid}`,
        }
        list.push(s)
        return HttpResponse.json(s)
      }
      const s = list.find((x) => x.name.endsWith(`/secrets/${id}`))
      if (!s) return err(404, 'NOT_FOUND', `Secret [${id}] not found or has no versions.`)
      const vs = (versions[id] ??= [])
      if (verb === 'addVersion') {
        const v = {
          name: `${s.name}/versions/${vs.length + 1}`,
          state: 'ENABLED' as const,
          data: (body as { payload: { data: string } }).payload.data,
        }
        vs.unshift(v)
        return HttpResponse.json(v)
      }
      if (verb === 'rotateSecret') return HttpResponse.json({ name: `${s.name}/versions/2` })
      if (ver === undefined && path.endsWith('/versions'))
        return HttpResponse.json({ versions: vs })
      if (ver !== undefined) {
        const v = ver === 'latest' ? vs[0] : vs.find((x) => x.name.endsWith(`/versions/${ver}`))
        if (!v) return err(404, 'NOT_FOUND', 'Secret Version not found.')
        if (verb === 'access') return HttpResponse.json({ name: v.name, payload: { data: v.data } })
        if (verb === 'disable') v.state = 'DISABLED'
        if (verb === 'enable') v.state = 'ENABLED'
        if (verb === 'destroy') v.state = 'DESTROYED'
        return HttpResponse.json(v)
      }
      if (request.method === 'DELETE') {
        secrets[loc] = list.filter((x) => x !== s)
        return HttpResponse.json({})
      }
      if (request.method === 'PATCH') return HttpResponse.json({ ...s, ...(body as Secret) })
      return HttpResponse.json(s)
    }),
  )
})

const sent = (method: string, path: RegExp) =>
  requests.filter((r) => r.method === method && path.test(r.path))

it('says how to enable Secret Manager when it is off', async () => {
  fake.info.services = ['iam']
  renderApp(`/secrets?project=${P}`)
  expect(
    await screen.findByRole('heading', { name: /Service secrets is not enabled/ }),
  ).toBeInTheDocument()
})

it('asks for a Project before listing secrets', async () => {
  renderApp('/secrets')
  expect(await screen.findByText(/Choose a Project/)).toBeInTheDocument()
})

it('lists global secrets, switches to a region and filters by name in the URL', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/secrets?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Secrets' })
  expect(
    within(table)
      .getAllByTestId('secret')
      .map((r) => r.textContent),
  ).toEqual([expect.stringContaining('db-password'), expect.stringContaining('api-key')])
  expect(within(table).getByText('team=web')).toBeInTheDocument()

  await user.type(screen.getByLabelText('Filter'), 'api')
  expect(router.state.location.search).toContain('q=api')
  expect(within(table).getAllByTestId('secret')).toHaveLength(1)

  await user.clear(screen.getByLabelText('Filter'))
  await user.selectOptions(await screen.findByLabelText('Location'), 'us-central1')
  expect(router.state.location.search).toContain('location=us-central1')
  expect(await screen.findByText('app-db')).toBeInTheDocument()
  expect(screen.getByText('Regional (us-central1)')).toBeInTheDocument()
})

it('creates a secret with the API’s validation messages and adds its value', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/secrets/create?project=${P}`)
  await user.type(await screen.findByLabelText('Name'), 'bad id!')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(
    await screen.findByText(
      'Secret ID "bad id!" is invalid: it must be 1-255 characters of letters, numbers, hyphens and underscores.',
    ),
  ).toBeInTheDocument()
  expect(sent('POST', /secrets$/)).toHaveLength(0)

  await user.clear(screen.getByLabelText('Name'))
  await user.type(screen.getByLabelText('Name'), 'new-key')
  await user.type(screen.getByLabelText('Secret value'), 'hunter2')
  await user.type(screen.getByLabelText('Labels'), 'env=dev')
  await user.selectOptions(screen.getByLabelText('Expiration'), 'ttl')
  await user.type(screen.getByLabelText('TTL (seconds)'), '3600')
  await user.click(screen.getByRole('tab', { name: 'JSON' }))
  const json: unknown = JSON.parse(screen.getByLabelText<HTMLTextAreaElement>('Request body').value)
  expect(json).toEqual({ replication: { automatic: {} }, labels: { env: 'dev' }, ttl: '3600s' })
  await user.click(screen.getByRole('button', { name: 'Create' }))

  await waitFor(() => expect(router.state.location.pathname).toBe('/secrets/secrets/new-key'))
  const [create] = sent('POST', /secrets$/)
  expect(create!.search.get('secretId')).toBe('new-key')
  const [add] = sent('POST', /new-key:addVersion$/)
  expect(decodePayload((add!.body as { payload: { data: string } }).payload.data)).toBe('hunter2')
})

it('asks for replicas of a user-managed policy, and creates regional secrets without one', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/secrets/create?project=${P}`)
  await user.type(await screen.findByLabelText('Name'), 'eu')
  await user.selectOptions(screen.getByLabelText('Replication'), 'user')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(
    await screen.findByText('A user-managed replication policy needs at least one replica.'),
  ).toBeInTheDocument()

  await user.selectOptions(await screen.findByLabelText('Location'), 'europe-west1')
  expect(screen.queryByLabelText('Replication')).not.toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/secrets/locations/europe-west1/secrets/eu'),
  )
  const [create] = sent('POST', /locations\/europe-west1\/secrets$/)
  expect(create!.body).toEqual({})
})

it('lists versions with aliases, reveals a value and changes version states', async () => {
  const user = userEvent.setup()
  renderApp(`/secrets/secrets/db-password?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Versions' })
  const rows = within(table).getAllByTestId('version')
  expect(rows[0]).toHaveTextContent('latest')
  expect(rows[1]).toHaveTextContent('prod')
  expect(rows[1]).toHaveTextContent('DISABLED')

  await user.click(within(rows[0]!).getByRole('button', { name: 'View value of version 2' }))
  const dialog = await screen.findByRole('dialog')
  const value = await within(dialog).findByLabelText('Secret value')
  expect(value).not.toHaveTextContent('s3cret-v2')
  await user.click(within(dialog).getByRole('button', { name: 'Reveal' }))
  expect(value).toHaveTextContent('s3cret-v2')
  await user.click(within(dialog).getByRole('button', { name: 'Close' }))

  await user.click(screen.getByRole('button', { name: 'Enable version 1' }))
  await waitFor(() => expect(sent('POST', /versions\/1:enable$/)).toHaveLength(1))
  await user.click(screen.getByRole('button', { name: 'Disable version 2' }))
  await waitFor(() => expect(sent('POST', /versions\/2:disable$/)).toHaveLength(1))
  await user.click(screen.getByRole('button', { name: 'Destroy version 1' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Destroy version' }),
  )
  await waitFor(() => expect(sent('POST', /versions\/1:destroy$/)).toHaveLength(1))
})

it('adds a version from the dialog', async () => {
  const user = userEvent.setup()
  renderApp(`/secrets/secrets/db-password?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Add version' }))
  const dialog = await screen.findByRole('dialog')
  await user.type(within(dialog).getByLabelText('Secret value'), 'v3 ✓')
  await user.click(within(dialog).getByRole('button', { name: 'Add version' }))
  await waitFor(() => expect(sent('POST', /:addVersion$/)).toHaveLength(1))
  const [add] = sent('POST', /:addVersion$/)
  expect(decodePayload((add!.body as { payload: { data: string } }).payload.data)).toBe('v3 ✓')
  expect(await screen.findByText(/Added version 3/)).toBeInTheDocument()
})

it('rotates a managed Cloud SQL secret instead of adding versions', async () => {
  const user = userEvent.setup()
  renderApp(`/secrets/locations/us-central1/secrets/app-db?project=${P}`)
  expect(await screen.findByText(/Managed rotation adds the versions/)).toBeInTheDocument()
  expect(screen.queryByRole('button', { name: 'Add version' })).not.toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Rotate now' }))
  await waitFor(() => expect(sent('POST', /app-db:rotateSecret$/)).toHaveLength(1))
})

it('edits only what changed, with an updateMask', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/secrets/secrets/api-key/edit?project=${P}`)
  await user.type(await screen.findByLabelText('Labels'), 'tier=gold')
  await user.selectOptions(screen.getByLabelText('Expiration'), 'never')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/secrets/secrets/api-key/overview'),
  )
  const [patch] = sent('PATCH', /secrets\/api-key$/)
  expect(patch!.search.get('updateMask')).toBe('labels,expire_time')
  expect(patch!.body).toEqual({ labels: { tier: 'gold' }, expireTime: null })
})

it('shows the configuration and deletes a secret', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/secrets/secrets/db-password/overview?project=${P}`)
  const details = await screen.findByLabelText('Secret details')
  expect(details).toHaveTextContent('Automatic')
  expect(details).toHaveTextContent('prod=1')
  await user.click(screen.getByRole('button', { name: 'Delete secret' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete secret' }),
  )
  await waitFor(() => expect(router.state.location.pathname).toBe('/secrets'))
  expect(sent('DELETE', /secrets\/db-password$/)).toHaveLength(1)
})

it('builds patch bodies and masks for every field', () => {
  const s = secret('x', {
    labels: { a: 'b' },
    rotation: { nextRotationTime: '2030-01-01T00:00:00Z', rotationPeriod: '3600s' },
    versionDestroyTtl: '86400s',
  })
  const body = patchBody(
    s,
    {
      labels: 'a=b',
      annotations: 'owner=me',
      topics: `projects/${P}/topics/t`,
      expiration: 'ttl',
      ttlSeconds: '60',
      expireTime: '',
      nextRotationTime: '',
      rotationPeriodSeconds: '',
      destroyTtlSeconds: '',
      aliases: 'prod=2',
    },
    {},
  )
  expect(body).toEqual({
    annotations: { owner: 'me' },
    topics: [{ name: `projects/${P}/topics/t` }],
    ttl: '60s',
    rotation: null,
    versionDestroyTtl: null,
    versionAliases: { prod: 2 },
  })
  expect(updateMask(body)).toEqual([
    'annotations',
    'topics',
    'ttl',
    'rotation',
    'version_destroy_ttl',
    'version_aliases',
  ])
})
