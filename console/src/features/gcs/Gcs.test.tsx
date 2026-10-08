import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, expect, it } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import type { Bucket, Notification, StorageObject } from './api'

const P = 'alpha-project'

// A small in-memory Cloud Storage JSON API: enough of buckets, objects
// (with generations) and notification configs for the view.
let buckets: Bucket[] = []
let objects: StorageObject[] = []
let notifications: Notification[] = []
let requests: { method: string; url: URL; body: unknown }[] = []

const obj = (name: string, gen: string, over: Partial<StorageObject> = {}): StorageObject => ({
  name,
  bucket: 'photos',
  generation: gen,
  metageneration: '1',
  size: '2048',
  contentType: 'image/png',
  timeCreated: '2026-10-01T10:00:00Z',
  updated: '2026-10-01T10:00:00Z',
  ...over,
})

async function record(request: Request) {
  const text = await request.clone().text()
  let body: unknown = text
  try {
    body = text ? JSON.parse(text) : undefined
  } catch {
    // media upload
  }
  requests.push({ method: request.method, url: new URL(request.url), body })
}

const notFound = (message: string) =>
  HttpResponse.json({ error: { code: 404, status: 'NOT_FOUND', message } }, { status: 404 })

beforeEach(() => {
  requests = []
  fake.info.services = ['iam', 'gcs', 'pubsub']
  buckets = [
    {
      name: 'photos',
      location: 'US',
      locationType: 'multi-region',
      storageClass: 'STANDARD',
      versioning: { enabled: true },
      labels: { team: 'web', env: 'dev' },
      iamConfiguration: { uniformBucketLevelAccess: { enabled: true } },
      timeCreated: '2026-10-01T10:00:00Z',
    },
    { name: 'logs-eu', location: 'EUROPE-WEST1', storageClass: 'NEARLINE' },
  ]
  objects = [
    obj('cat.png', '3'),
    obj('cat.png', '1', { timeDeleted: '2026-10-02T10:00:00Z', size: '1024' }),
    obj('2026/', '5', { size: '0' }),
    obj('2026/jan.png', '6'),
    obj('2026/deep/feb.png', '7'),
  ]
  notifications = [
    {
      id: '1',
      topic: `//pubsub.googleapis.com/projects/${P}/topics/uploads`,
      event_types: ['OBJECT_FINALIZE'],
      payload_format: 'JSON_API_V1',
    },
  ]
  server.use(
    http.get('*/storage/v1/b', () => HttpResponse.json({ items: buckets })),
    http.post('*/storage/v1/b', async ({ request }) => {
      await record(request)
      const b = requests.at(-1)!.body as Bucket
      if (buckets.some((x) => x.name === b.name)) {
        return HttpResponse.json(
          {
            error: {
              code: 409,
              status: 'ALREADY_EXISTS',
              message:
                'Your previous request to create the named bucket succeeded and you already own it.',
            },
          },
          { status: 409 },
        )
      }
      buckets.push(b)
      return HttpResponse.json(b)
    }),
    http.get('*/storage/v1/b/:bucket', ({ params }) => {
      const b = buckets.find((x) => x.name === params.bucket)
      return b ? HttpResponse.json(b) : notFound('The specified bucket does not exist.')
    }),
    http.patch('*/storage/v1/b/:bucket', async ({ request, params }) => {
      await record(request)
      const b = buckets.find((x) => x.name === params.bucket)!
      return HttpResponse.json(b)
    }),
    http.delete('*/storage/v1/b/:bucket', async ({ request }) => {
      await record(request)
      return new HttpResponse(null, { status: 204 })
    }),
    http.get('*/storage/v1/b/:bucket/o', ({ request }) => {
      const q = new URL(request.url).searchParams
      const prefix = q.get('prefix') ?? ''
      const versions = q.get('versions') === 'true'
      const under = objects.filter((o) => o.name.startsWith(prefix) && (versions || !o.timeDeleted))
      if (q.get('delimiter') !== '/') return HttpResponse.json({ items: under })
      const prefixes = new Set<string>()
      const items: StorageObject[] = []
      for (const o of under) {
        const rest = o.name.slice(prefix.length)
        const i = rest.indexOf('/')
        if (i >= 0) prefixes.add(prefix + rest.slice(0, i + 1))
        else items.push(o)
      }
      return HttpResponse.json({ items, prefixes: [...prefixes] })
    }),
    http.get('*/storage/v1/b/:bucket/o/:name', ({ params }) => {
      const o = objects.find((x) => x.name === params.name && !x.timeDeleted)
      return o ? HttpResponse.json(o) : notFound('No such object: photos/' + String(params.name))
    }),
    http.patch('*/storage/v1/b/:bucket/o/:name', async ({ request, params }) => {
      await record(request)
      return HttpResponse.json(objects.find((x) => x.name === params.name))
    }),
    http.delete('*/storage/v1/b/:bucket/o/:name', async ({ request }) => {
      await record(request)
      return new HttpResponse(null, { status: 204 })
    }),
    http.post('*/storage/v1/b/:bucket/o/:name/copyTo/b/:dst/o/:dstName', async ({ request }) => {
      await record(request)
      return HttpResponse.json(obj('cat.png', '9'))
    }),
    http.post('*/upload/storage/v1/b/:bucket/o', async ({ request }) => {
      await record(request)
      const name = new URL(request.url).searchParams.get('name')!
      return HttpResponse.json(obj(name, '10'))
    }),
    http.get('*/storage/v1/b/:bucket/notificationConfigs', () =>
      HttpResponse.json({ items: notifications }),
    ),
    http.post('*/storage/v1/b/:bucket/notificationConfigs', async ({ request }) => {
      await record(request)
      const n = { ...(requests.at(-1)!.body as Notification), id: '2' }
      if (n.topic.endsWith('/missing')) {
        return HttpResponse.json(
          {
            error: {
              code: 400,
              status: 'INVALID_ARGUMENT',
              message: `The Cloud Pub/Sub topic projects/${P}/topics/missing does not exist.`,
            },
          },
          { status: 400 },
        )
      }
      notifications.push(n)
      return HttpResponse.json(n)
    }),
    http.delete('*/storage/v1/b/:bucket/notificationConfigs/:id', async ({ request }) => {
      await record(request)
      return new HttpResponse(null, { status: 204 })
    }),
    http.get('*/pubsub/v1/projects/:p/topics', () =>
      HttpResponse.json({ topics: [{ name: `projects/${P}/topics/uploads` }] }),
    ),
    http.get('*/iam/v1/projects/:p/serviceAccounts', () =>
      HttpResponse.json({ accounts: [{ email: `signer@${P}.iam.gserviceaccount.com` }] }),
    ),
  )
})

const sent = (method: string, path: RegExp) =>
  requests.filter((r) => r.method === method && path.test(r.url.pathname))

it('says how to enable Cloud Storage when it is off', async () => {
  fake.info.services = ['iam']
  renderApp(`/gcs?project=${P}`)
  expect(
    await screen.findByRole('heading', { name: /Service gcs is not enabled/ }),
  ).toBeInTheDocument()
})

it('asks for a Project before listing buckets', async () => {
  renderApp('/gcs')
  expect(await screen.findByText(/Choose a Project/)).toBeInTheDocument()
})

it('lists buckets, filtering by location and name in the URL', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/gcs?project=${P}`)
  await screen.findByRole('table', { name: 'Buckets' })
  const rows = () => screen.getAllByTestId('bucket')
  expect(rows()).toHaveLength(2)
  await user.type(screen.getByLabelText('Location'), 'europe')
  expect(rows()).toHaveLength(1)
  expect(router.state.location.search).toContain('location=europe')
  await user.clear(screen.getByLabelText('Location'))
  await user.type(screen.getByLabelText('Filter'), 'pho')
  expect(rows()).toHaveLength(1)
  expect(screen.getByRole('link', { name: 'photos' })).toBeInTheDocument()
})

it('creates a bucket, showing the API’s validation messages', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/gcs/create?project=${P}`)
  const name = await screen.findByLabelText('Name')
  await user.type(name, 'Bad_Name')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(await screen.findByText("Invalid bucket name: 'Bad_Name'")).toBeInTheDocument()
  expect(sent('POST', /\/storage\/v1\/b$/)).toHaveLength(0)

  await user.clear(name)
  await user.type(name, 'photos')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(await screen.findByText(/you already own it/)).toBeInTheDocument()

  await user.clear(name)
  await user.type(name, 'fresh-bucket')
  await user.clear(screen.getByLabelText('Location'))
  await user.type(screen.getByLabelText('Location'), 'us-east1')
  await user.click(screen.getByLabelText('Object versioning'))
  await user.type(screen.getByLabelText('Labels'), 'team=data')

  // The JSON editor holds the whole buckets.insert body.
  await user.click(screen.getByRole('tab', { name: 'JSON' }))
  const body = JSON.parse(
    screen.getByLabelText<HTMLTextAreaElement>('Request body').value,
  ) as Bucket
  expect(body).toMatchObject({
    name: 'fresh-bucket',
    location: 'US-EAST1',
    versioning: { enabled: true },
    labels: { team: 'data' },
    iamConfiguration: { uniformBucketLevelAccess: { enabled: true } },
  })
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() => expect(router.state.location.pathname).toBe('/gcs/b/fresh-bucket'))
  expect(router.state.location.search).toBe(`?project=${P}`)
  expect(
    sent('POST', /\/storage\/v1\/b$/)
      .at(-1)!
      .url.searchParams.get('project'),
  ).toBe(P)
})

it('edits a bucket with a merge patch of what changed', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/gcs/b/photos/edit?project=${P}`)
  const labels = await screen.findByLabelText('Labels')
  expect(labels).toHaveValue('team=web\nenv=dev')
  await user.clear(labels)
  await user.type(labels, 'team=platform')
  await user.click(screen.getByLabelText('Object versioning'))
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(router.state.location.pathname).toBe('/gcs/b/photos/configuration'))
  expect(sent('PATCH', /\/b\/photos$/).at(-1)!.body).toEqual({
    versioning: { enabled: false },
    labels: { team: 'platform', env: null },
  })
})

it('shows a bucket’s configuration and deletes it', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/gcs/b/photos/configuration?project=${P}`)
  const details = await screen.findByLabelText('Bucket details')
  expect(within(details).getByText('US (multi-region)')).toBeInTheDocument()
  expect(within(details).getByText('team=web, env=dev')).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Delete bucket' }))
  await user.click(
    await within(await screen.findByRole('dialog')).findByRole('button', { name: 'Delete bucket' }),
  )
  await waitFor(() => expect(router.state.location.pathname).toBe('/gcs'))
  expect(sent('DELETE', /\/b\/photos$/)).toHaveLength(1)
})

it('browses a bucket by prefix', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/gcs/b/photos?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Objects' })
  expect(within(table).getAllByTestId('folder')).toHaveLength(1)
  expect(within(table).getAllByTestId('object')).toHaveLength(1)
  expect(within(table).getByText('2.0 KiB')).toBeInTheDocument()

  await user.click(within(table).getByRole('link', { name: '2026/' }))
  await waitFor(() => expect(router.state.location.search).toContain('prefix=2026%2F'))
  const inner = await screen.findByRole('link', { name: 'jan.png' })
  expect(inner).toHaveAttribute('href', `/gcs/b/photos/o/2026/jan.png?project=${P}`)
  // The folder's own zero-byte marker object is not listed.
  expect(screen.getAllByTestId('object')).toHaveLength(1)
  expect(screen.getByRole('link', { name: 'deep/' })).toBeInTheDocument()

  // The breadcrumb goes back up.
  await user.click(within(screen.getByRole('navigation', { name: 'Folder' })).getByText('photos'))
  await waitFor(() => expect(router.state.location.search).not.toContain('prefix'))
})

it('renders only the rows in view and loads more pages as it scrolls', async () => {
  const page = (from: number, token?: string) => ({
    items: Array.from({ length: 1000 }, (_, i) =>
      obj(`f${String(from + i).padStart(6, '0')}`, '1'),
    ),
    nextPageToken: token,
  })
  const tokens: (string | null)[] = []
  server.use(
    http.get('*/storage/v1/b/:bucket/o', ({ request }) => {
      const t = new URL(request.url).searchParams.get('pageToken')
      tokens.push(t)
      return HttpResponse.json(t ? page(1000) : page(0, 'next'))
    }),
  )
  renderApp(`/gcs/b/photos?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Objects' })
  expect(table).toHaveAttribute('aria-rowcount', '1001')
  expect(within(table).getAllByTestId('object').length).toBeLessThan(50)
  expect(tokens).toEqual([null])

  table.scrollTop = 1000 * 40 - 560
  fireEvent.scroll(table)
  await waitFor(() => expect(table).toHaveAttribute('aria-rowcount', '2001'))
  expect(tokens).toEqual([null, 'next'])
  expect(within(table).getByText('f000999')).toBeInTheDocument()
})

it('uploads chosen files into the current folder', async () => {
  const user = userEvent.setup()
  renderApp(`/gcs/b/photos?project=${P}&prefix=2026%2F`)
  await screen.findByRole('link', { name: 'jan.png' })
  const file = new File(['hello'], 'mar.txt', { type: 'text/plain' })
  await user.upload(screen.getByLabelText('Files to upload'), file)
  await waitFor(() => expect(screen.getByTestId('upload')).toHaveAttribute('data-status', 'done'))
  const up = sent('POST', /\/upload\/storage\/v1\/b\/photos\/o$/).at(-1)!
  expect(up.url.searchParams.get('name')).toBe('2026/mar.txt')
  expect(up.url.searchParams.get('uploadType')).toBe('media')
  expect(up.body).toBe('hello')
})

it('uploads files dropped on the browser', async () => {
  renderApp(`/gcs/b/photos?project=${P}`)
  const browser = await screen.findByTestId('object-browser')
  const file = new File(['x'], 'dropped.bin')
  fireEvent.drop(browser, { dataTransfer: { files: [file], items: [], types: ['Files'] } })
  await waitFor(() => expect(screen.getByTestId('upload')).toHaveAttribute('data-status', 'done'))
  expect(
    sent('POST', /\/upload\//)
      .at(-1)!
      .url.searchParams.get('name'),
  ).toBe('dropped.bin')
})

it('shows an object’s metadata and generations, restoring and deleting one', async () => {
  const user = userEvent.setup()
  renderApp(`/gcs/b/photos/o/cat.png?project=${P}`)
  const gens = await screen.findByRole('table', { name: 'Generations' })
  const rows = within(gens).getAllByTestId('generation')
  expect(rows).toHaveLength(2)
  expect(within(rows[0]!).getByText('Live')).toBeInTheDocument()
  expect(within(rows[1]!).getByText('Noncurrent')).toBeInTheDocument()
  expect(within(rows[0]!).queryByRole('button', { name: /Restore/ })).toBeNull()
  expect(within(rows[1]!).getByRole('link', { name: 'Download generation 1' })).toHaveAttribute(
    'href',
    '/download/storage/v1/b/photos/o/cat.png?alt=media&generation=1',
  )
  expect(screen.getByLabelText('Object details')).toHaveTextContent('image/png')

  await user.click(within(rows[1]!).getByRole('button', { name: 'Restore generation 1' }))
  await user.click(await screen.findByRole('button', { name: 'Restore' }))
  await waitFor(() => expect(sent('POST', /copyTo/)).toHaveLength(1))
  const copy = sent('POST', /copyTo/)[0]!
  expect(copy.url.pathname).toBe('/storage/v1/b/photos/o/cat.png/copyTo/b/photos/o/cat.png')
  expect(copy.url.searchParams.get('sourceGeneration')).toBe('1')

  await user.click(within(rows[1]!).getByRole('button', { name: 'Delete generation 1' }))
  await user.click(await screen.findByRole('button', { name: 'Delete generation' }))
  await waitFor(() => expect(sent('DELETE', /\/o\/cat\.png$/)).toHaveLength(1))
  expect(sent('DELETE', /\/o\/cat\.png$/)[0]!.url.searchParams.get('generation')).toBe('1')
})

it('offers to restore an object whose live version was deleted', async () => {
  objects = [obj('gone.txt', '4', { timeDeleted: '2026-10-03T10:00:00Z' })]
  renderApp(`/gcs/b/photos/o/gone.txt?project=${P}`)
  expect(await screen.findByText(/no live version/)).toBeInTheDocument()
  expect(await screen.findByRole('button', { name: 'Restore generation 4' })).toBeInTheDocument()
  expect(screen.queryByRole('button', { name: 'Edit metadata' })).toBeNull()
})

it('escapes object names with slashes and spaces in API paths', async () => {
  objects = [obj('a dir/b c.txt', '2')]
  renderApp(`/gcs/b/photos/o/a%20dir/b%20c.txt?project=${P}`)
  expect(await screen.findByRole('heading', { name: 'b c.txt' })).toBeInTheDocument()
  expect(screen.getByRole('link', { name: 'Download' })).toHaveAttribute(
    'href',
    '/download/storage/v1/b/photos/o/a%20dir%2Fb%20c.txt?alt=media',
  )
})

it('edits an object’s metadata with a merge patch', async () => {
  objects = [obj('cat.png', '3', { metadata: { owner: 'ann', note: 'x' } })]
  const user = userEvent.setup()
  const { router } = renderApp(`/gcs/b/photos/edit/cat.png?project=${P}`)
  const ct = await screen.findByLabelText('Content type')
  await user.clear(ct)
  await user.type(ct, 'image/webp')
  const meta = screen.getByLabelText('Custom metadata')
  await user.clear(meta)
  await user.type(meta, 'owner: bob')
  await user.click(screen.getByLabelText('Temporary hold'))
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(router.state.location.pathname).toBe('/gcs/b/photos/o/cat.png'))
  expect(sent('PATCH', /\/o\/cat\.png$/).at(-1)!.body).toEqual({
    contentType: 'image/webp',
    metadata: { owner: 'bob', note: null },
    temporaryHold: true,
  })
})

it('lists, creates and deletes notifications', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/gcs/b/photos/notifications?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Notifications' })
  expect(within(table).getByText(`projects/${P}/topics/uploads`)).toBeInTheDocument()

  await user.click(screen.getByRole('link', { name: 'Create notification' }))
  const topic = await screen.findByLabelText('Topic')
  await user.clear(topic)
  await user.type(topic, 'uploads')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(await screen.findByText('Invalid Cloud Pub/Sub topic name: uploads')).toBeInTheDocument()

  await user.clear(topic)
  await user.type(topic, `projects/${P}/topics/missing`)
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(await screen.findByText(/topics\/missing does not exist/)).toBeInTheDocument()

  await user.clear(topic)
  await user.type(topic, `projects/${P}/topics/uploads`)
  await user.click(screen.getByLabelText('OBJECT_DELETE'))
  await user.type(screen.getByLabelText('Object name prefix'), 'in/')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() => expect(router.state.location.pathname).toBe('/gcs/b/photos/notifications/2'))
  expect(sent('POST', /notificationConfigs$/).at(-1)!.body).toEqual({
    topic: `projects/${P}/topics/uploads`,
    payload_format: 'JSON_API_V1',
    event_types: ['OBJECT_DELETE'],
    object_name_prefix: 'in/',
  })

  const card = (await screen.findByRole('heading', { name: 'Notification 2' })).closest('section')!
  await user.click(within(card).getByRole('button', { name: 'Delete' }))
  await user.click(await screen.findByRole('button', { name: 'Delete notification' }))
  await waitFor(() => expect(router.state.location.pathname).toBe('/gcs/b/photos/notifications'))
  expect(sent('DELETE', /notificationConfigs\/2$/)).toHaveLength(1)
})

it('generates a signed URL through signBlob', async () => {
  const user = userEvent.setup()
  server.use(
    http.post('*/iamcredentials/v1/projects/-/serviceAccounts/:sa', () =>
      HttpResponse.json({ keyId: 'k', signedBlob: btoa('\x0f') }),
    ),
  )
  renderApp(`/gcs/b/photos/o/cat.png?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Signed URL' }))
  const dialog = await screen.findByRole('dialog')
  const expires = within(dialog).getByLabelText('Expires after (seconds)')
  await user.clear(expires)
  await user.type(expires, '700000')
  await user.click(within(dialog).getByRole('button', { name: 'Generate' }))
  expect(
    await within(dialog).findByText(/must be between 1 and 604800 seconds/),
  ).toBeInTheDocument()
  expect(within(dialog).getByText(/A service account signs the URL/)).toBeInTheDocument()

  await user.clear(expires)
  await user.type(expires, '600')
  await user.type(
    within(dialog).getByLabelText('Service account'),
    `signer@${P}.iam.gserviceaccount.com`,
  )
  await user.click(within(dialog).getByRole('button', { name: 'Generate' }))
  const url = await within(dialog).findByLabelText<HTMLTextAreaElement>('Signed URL')
  expect(url.value).toMatch(
    /^http:\/\/localhost(:\d+)?\/photos\/cat\.png\?X-Goog-Algorithm=GOOG4-RSA-SHA256&.*X-Goog-Expires=600&X-Goog-SignedHeaders=host&X-Goog-Signature=0f$/,
  )
})

it('says signed URLs need the iam Service', async () => {
  fake.info.services = ['gcs']
  const user = userEvent.setup()
  renderApp(`/gcs/b/photos/o/cat.png?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Signed URL' }))
  expect(await screen.findByText(/enable the/)).toHaveTextContent('iam Service')
})
