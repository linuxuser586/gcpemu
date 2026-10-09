import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, expect, it } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import type { DockerImage, Repository } from './api'
import { imageDigests } from './ImagePage'
import { patchBody, updateMask } from './RepositoryForm'

const P = 'alpha-project'
const US = `projects/${P}/locations/us-central1`
const REPO = `${US}/repositories/images`
const d = (c: string) => `sha256:${c.repeat(64)}`

// A small in-memory Artifact Registry REST API: repositories in two
// locations, one holding a multi-arch image and a nested one, and the
// requests the view sends.
let repos: Repository[] = []
let images: DockerImage[] = []
let requests: { method: string; path: string; search: URLSearchParams; body: unknown }[] = []

const err = (code: number, status: string, message: string) =>
  HttpResponse.json({ error: { code, status, message } }, { status: code })

const done = (response?: unknown) =>
  HttpResponse.json({ name: `${US}/operations/op-1`, done: true, response })

const docker = (image: string, digest: string, over: Partial<DockerImage> = {}): DockerImage => ({
  name: `${REPO}/dockerImages/${image.replaceAll('/', '%2F')}@${digest}`,
  uri: `us-central1-docker.pkg.dev/${P}/images/${image}@${digest}`,
  imageSizeBytes: '1000',
  uploadTime: '2026-10-01T10:00:00Z',
  mediaType: 'application/vnd.oci.image.manifest.v1+json',
  ...over,
})

beforeEach(() => {
  requests = []
  fake.info.services = ['iam', 'ar']
  fake.endpoints = { gateway: '127.0.0.1:4510', ar: '127.0.0.1:5000' }
  repos = [
    {
      name: REPO,
      format: 'DOCKER',
      mode: 'STANDARD_REPOSITORY',
      description: 'App images',
      labels: { team: 'web' },
      sizeBytes: '3000',
      registryUri: `us-central1-docker.pkg.dev/${P}/images`,
    },
    {
      name: `projects/${P}/locations/europe-west1/repositories/hub`,
      format: 'DOCKER',
      mode: 'REMOTE_REPOSITORY',
      remoteRepositoryConfig: { dockerRepository: { publicRepository: 'DOCKER_HUB' } },
    },
  ]
  images = [
    docker('app', d('1'), {
      mediaType: 'application/vnd.oci.image.index.v1+json',
      tags: ['latest', 'v1'],
      imageSizeBytes: '2400',
      imageManifests: [
        { digest: d('a'), os: 'linux', architecture: 'amd64' },
        { digest: d('b'), os: 'linux', architecture: 'arm64', variant: 'v8' },
      ],
    }),
    docker('app', d('a'), { imageSizeBytes: '1100' }),
    docker('app', d('b'), { imageSizeBytes: '1200' }),
    docker('team/svc', d('c'), { tags: ['v2'] }),
    docker('team/svc', d('e')),
  ]
  server.use(
    http.get('/_emu/v1/hostmode', () => HttpResponse.json({ enabled: false })),
    http.all(/\/artifactregistry\/v1\/(.*)$/, async ({ request }) => {
      const url = new URL(request.url)
      const text = request.method === 'GET' ? '' : await request.text()
      const body: unknown = text ? JSON.parse(text) : undefined
      const path = decodeURIComponent(url.pathname.replace(/^.*\/artifactregistry\/v1\//, ''))
      requests.push({ method: request.method, path, search: url.searchParams, body })
      if (path === `projects/${P}/locations`) {
        return HttpResponse.json({
          locations: [
            { name: US, locationId: 'us-central1', displayName: 'Iowa' },
            { name: `projects/${P}/locations/europe-west1`, locationId: 'europe-west1' },
          ],
        })
      }
      const m = /^(projects\/[^/]+\/locations\/[^/]+)\/repositories(?:\/([^/]+)(\/.*)?)?$/.exec(
        path,
      )
      if (!m) return err(400, 'INVALID_ARGUMENT', `bad path ${path}`)
      const [, parent, id, rest = ''] = m
      if (!id) {
        if (request.method === 'GET') {
          return HttpResponse.json({
            repositories: repos.filter((r) => r.name.startsWith(parent!)),
          })
        }
        const rid = url.searchParams.get('repositoryId') ?? ''
        if (repos.some((r) => r.name === `${parent}/repositories/${rid}`)) {
          return err(409, 'ALREADY_EXISTS', 'the repository already exists')
        }
        const r = { ...(body as Repository), name: `${parent}/repositories/${rid}` }
        repos.push(r)
        return done(r)
      }
      const r = repos.find((x) => x.name === `${parent}/repositories/${id}`)
      if (!r) return err(404, 'NOT_FOUND', 'Requested entity was not found.')
      if (rest === '') {
        if (request.method === 'DELETE') {
          repos = repos.filter((x) => x !== r)
          return done()
        }
        if (request.method === 'PATCH') return HttpResponse.json({ ...r, ...(body as Repository) })
        return HttpResponse.json(r)
      }
      if (rest === '/dockerImages') return HttpResponse.json({ dockerImages: images })
      if (rest === '/packages') {
        const names = [
          ...new Set(images.map((i) => i.name.split('/dockerImages/')[1]!.split('@')[0])),
        ]
        return HttpResponse.json({
          packages: names.map((n) => ({ name: `${r.name}/packages/${n}` })),
        })
      }
      if (request.method === 'DELETE' && /^\/packages\/[^/]+(\/versions\/[^/]+)?$/.test(rest)) {
        return done()
      }
      if (/^\/packages\/[^/]+\/tags(\/[^/]+)?$/.test(rest)) {
        if (request.method === 'DELETE') return HttpResponse.json({})
        const tag = url.searchParams.get('tagId') ?? rest.split('/').pop()
        if (request.method === 'POST' && images.some((i) => i.tags?.includes(tag!))) {
          return err(409, 'ALREADY_EXISTS', `Tag ${tag} already exists.`)
        }
        return HttpResponse.json({ name: `${r.name}${rest}`, ...(body as object) })
      }
      return err(400, 'INVALID_ARGUMENT', `bad path ${path}`)
    }),
  )
})

const sent = (method: string, path: RegExp) =>
  requests.filter((r) => r.method === method && path.test(r.path))

it('says how to enable Artifact Registry when it is off', async () => {
  fake.info.services = ['iam']
  renderApp(`/ar?project=${P}`)
  expect(
    await screen.findByRole('heading', { name: /Service ar is not enabled/ }),
  ).toBeInTheDocument()
})

it('asks for a Project before listing repositories', async () => {
  renderApp('/ar')
  expect(await screen.findByText(/Choose a Project/)).toBeInTheDocument()
})

it('lists repositories in every location, then one, and filters by name in the URL', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/ar?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Repositories' })
  await waitFor(() => expect(within(table).getAllByTestId('repository')).toHaveLength(2))
  const [images, hub] = within(table).getAllByTestId('repository')
  expect(images).toHaveTextContent('us-central1')
  expect(images).toHaveTextContent('team=web')
  expect(images).toHaveTextContent('2.9 KiB')
  expect(hub).toHaveTextContent('Remote')
  expect(sent('GET', /repositories$/).map((r) => r.path)).toEqual([
    `${US}/repositories`,
    `projects/${P}/locations/europe-west1/repositories`,
  ])

  await user.type(screen.getByLabelText('Filter'), 'hub')
  expect(router.state.location.search).toContain('q=hub')
  expect(within(table).getAllByTestId('repository')).toHaveLength(1)

  await user.clear(screen.getByLabelText('Filter'))
  await user.selectOptions(screen.getByLabelText('Location'), 'us-central1')
  expect(router.state.location.search).toContain('location=us-central1')
  await waitFor(() =>
    expect(screen.getAllByTestId('repository').map((r) => r.textContent)).toEqual([
      expect.stringContaining('images'),
    ]),
  )
})

it('creates a repository with the API’s validation messages and waits for its Operation', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/ar/create?project=${P}`)
  await user.type(await screen.findByLabelText('Name'), 'Bad_ID')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(
    await screen.findByText(
      'Invalid repository ID "Bad_ID": repository IDs must start with a lowercase letter, contain only lowercase letters, numbers and hyphens, and be at most 63 characters.',
    ),
  ).toBeInTheDocument()
  expect(sent('POST', /repositories$/)).toHaveLength(0)

  await user.clear(screen.getByLabelText('Name'))
  await user.type(screen.getByLabelText('Name'), 'mirror')
  await user.selectOptions(await screen.findByLabelText('Location'), 'europe-west1')
  await user.selectOptions(screen.getByLabelText('Mode'), 'REMOTE_REPOSITORY')
  await user.selectOptions(screen.getByLabelText('Remote source'), 'custom')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(
    await screen.findByText(
      'remote_repository_config.docker_repository must set public_repository or custom_repository.',
    ),
  ).toBeInTheDocument()
  await user.type(screen.getByLabelText('Registry URL'), 'https://registry.example.com')
  await user.type(screen.getByLabelText('Labels'), 'env=dev')
  await user.click(screen.getByLabelText(/Immutable tags/))
  await user.click(screen.getByRole('tab', { name: 'JSON' }))
  const json: unknown = JSON.parse(screen.getByLabelText<HTMLTextAreaElement>('Request body').value)
  expect(json).toEqual({
    format: 'DOCKER',
    mode: 'REMOTE_REPOSITORY',
    labels: { env: 'dev' },
    dockerConfig: { immutableTags: true },
    remoteRepositoryConfig: {
      dockerRepository: { customRepository: { uri: 'https://registry.example.com' } },
    },
  })
  await user.click(screen.getByRole('button', { name: 'Create' }))

  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/ar/locations/europe-west1/repositories/mirror'),
  )
  const [create] = sent('POST', /europe-west1\/repositories$/)
  expect(create!.search.get('repositoryId')).toBe('mirror')
})

it('creates a virtual repository from its upstreams', async () => {
  const user = userEvent.setup()
  renderApp(`/ar/create?project=${P}&location=us-central1`)
  await user.type(await screen.findByLabelText('Name'), 'all')
  await user.selectOptions(screen.getByLabelText('Mode'), 'VIRTUAL_REPOSITORY')
  await user.type(screen.getByLabelText('Upstream repositories'), 'images')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(
    await screen.findByText('Invalid upstream "images": use repository-id=priority.'),
  ).toBeInTheDocument()
  await user.type(screen.getByLabelText('Upstream repositories'), '=10')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() => expect(sent('POST', /repositories$/)).toHaveLength(1))
  expect(sent('POST', /repositories$/)[0]!.body).toEqual({
    format: 'DOCKER',
    mode: 'VIRTUAL_REPOSITORY',
    virtualRepositoryConfig: {
      upstreamPolicies: [{ id: 'images', repository: REPO, priority: 10 }],
    },
  })
})

it('shows a create error the API returns', async () => {
  const user = userEvent.setup()
  renderApp(`/ar/create?project=${P}&location=us-central1`)
  await user.type(await screen.findByLabelText('Name'), 'images')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect((await screen.findAllByText('the repository already exists')).length).toBeGreaterThan(0)
})

it('lists a repository’s images with digests, tags and sizes, and deletes one', async () => {
  const user = userEvent.setup()
  renderApp(`/ar/locations/us-central1/repositories/images?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Images' })
  await waitFor(() => expect(within(table).getAllByTestId('image')).toHaveLength(2))
  const [app, svc] = within(table).getAllByTestId('image')
  expect(app).toHaveTextContent('app')
  expect(app).toHaveTextContent('latest, v1')
  expect(app).toHaveTextContent('3')
  expect(svc).toHaveTextContent('team/svc')
  expect(within(svc!).getByRole('link', { name: 'team/svc' })).toHaveAttribute(
    'href',
    `/ar/locations/us-central1/repositories/images/images/team%2Fsvc?project=${P}`,
  )

  await user.click(within(svc!).getByRole('button', { name: 'Delete image team/svc' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete image' }),
  )
  await waitFor(() => expect(sent('DELETE', /packages/)).toHaveLength(1))
  expect(sent('DELETE', /packages/)[0]!.path).toBe(`${REPO}/packages/team%2Fsvc`)
})

it('shows an index with its per-arch manifests, copies pull commands and deletes a tag', async () => {
  const user = userEvent.setup()
  renderApp(`/ar/locations/us-central1/repositories/images/images/app?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Digests' })
  const rows = await within(table).findAllByTestId('digest')
  expect(rows).toHaveLength(1)
  expect(rows[0]).toHaveTextContent('Index')
  const platforms = within(table).getAllByTestId('platform')
  expect(platforms.map((p) => p.textContent)).toEqual([
    expect.stringMatching(/linux\/amd64.*1\.1 KiB/),
    expect.stringMatching(/linux\/arm64\/v8.*1\.2 KiB/),
  ])

  await user.click(screen.getByRole('button', { name: `Copy pull command of 111111111111` }))
  expect(await navigator.clipboard.readText()).toBe(
    `docker pull 127.0.0.1:5000/us-central1-docker.pkg.dev/${P}/images/app:latest`,
  )
  await user.click(screen.getByRole('button', { name: 'Copy pull command of linux/arm64/v8' }))
  expect(await navigator.clipboard.readText()).toBe(
    `docker pull 127.0.0.1:5000/us-central1-docker.pkg.dev/${P}/images/app@${d('b')}`,
  )

  await user.click(screen.getByRole('button', { name: 'Delete tag v1' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete tag' }),
  )
  await waitFor(() => expect(sent('DELETE', /tags/)).toHaveLength(1))
  expect(sent('DELETE', /tags/)[0]!.path).toBe(`${REPO}/packages/app/tags/v1`)
})

it('pulls by the real registry host in host mode', async () => {
  server.use(
    http.get('/_emu/v1/hostmode', () => HttpResponse.json({ enabled: true, state: 'ready' })),
  )
  const user = userEvent.setup()
  renderApp(`/ar/locations/us-central1/repositories/images/images/team%2Fsvc?project=${P}`)
  const rows = await screen.findAllByTestId('digest')
  expect(rows).toHaveLength(2)
  await waitFor(async () => {
    await user.click(screen.getByRole('button', { name: 'Copy pull command of cccccccccccc' }))
    expect(await navigator.clipboard.readText()).toBe(
      `docker pull us-central1-docker.pkg.dev/${P}/images/team/svc:v2`,
    )
  })
})

it('tags a digest: a new tag is created, an existing one moved', async () => {
  const user = userEvent.setup()
  renderApp(`/ar/locations/us-central1/repositories/images/images/team%2Fsvc?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Add tag to eeeeeeeeeeee' }))
  let dialog = await screen.findByRole('dialog')
  await user.type(within(dialog).getByLabelText('Tag'), 'bad tag')
  await user.click(within(dialog).getByRole('button', { name: 'Add tag' }))
  expect(within(dialog).getByRole('alert')).toHaveTextContent('Invalid tag "bad tag".')
  await user.clear(within(dialog).getByLabelText('Tag'))
  await user.type(within(dialog).getByLabelText('Tag'), 'v3')
  await user.click(within(dialog).getByRole('button', { name: 'Add tag' }))
  await waitFor(() => expect(sent('POST', /tags$/)).toHaveLength(1))
  const [create] = sent('POST', /tags$/)
  expect(create!.path).toBe(`${REPO}/packages/team%2Fsvc/tags`)
  expect(create!.search.get('tagId')).toBe('v3')
  expect(create!.body).toEqual({ version: `${REPO}/packages/team%2Fsvc/versions/${d('e')}` })

  await user.click(screen.getByRole('button', { name: 'Add tag to eeeeeeeeeeee' }))
  dialog = await screen.findByRole('dialog')
  await user.type(within(dialog).getByLabelText('Tag'), 'v2')
  await user.click(within(dialog).getByRole('button', { name: 'Move tag' }))
  await waitFor(() => expect(sent('PATCH', /tags\/v2$/)).toHaveLength(1))
  const [move] = sent('PATCH', /tags\/v2$/)
  expect(move!.search.get('updateMask')).toBe('version')
})

it('deletes a tagged digest with its tags', async () => {
  const user = userEvent.setup()
  renderApp(`/ar/locations/us-central1/repositories/images/images/team%2Fsvc?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Delete digest cccccccccccc' }))
  const dialog = await screen.findByRole('dialog')
  expect(dialog).toHaveTextContent('Its tags (v2) are deleted with it.')
  await user.click(within(dialog).getByRole('button', { name: 'Delete digest' }))
  await waitFor(() => expect(sent('DELETE', /versions/)).toHaveLength(1))
  const [del] = sent('DELETE', /versions/)
  expect(del!.path).toBe(`${REPO}/packages/team%2Fsvc/versions/${d('c')}`)
  expect(del!.search.get('force')).toBe('true')
})

it('shows a digest’s manifest, platforms and pull commands', async () => {
  renderApp(`/ar/locations/us-central1/repositories/images/images/app/${d('1')}?project=${P}`)
  const details = await screen.findByLabelText('Digest details')
  expect(details).toHaveTextContent('application/vnd.oci.image.index.v1+json')
  expect(details).toHaveTextContent('latest')
  expect(screen.getAllByTestId('platform')).toHaveLength(2)
  expect(
    await screen.findByText(
      `docker pull 127.0.0.1:5000/us-central1-docker.pkg.dev/${P}/images/app:v1`,
    ),
  ).toBeInTheDocument()
})

it('shows a repository’s configuration and deletes it', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(
    `/ar/locations/us-central1/repositories/images/overview?project=${P}`,
  )
  const details = await screen.findByLabelText('Repository details')
  expect(details).toHaveTextContent('App images')
  expect(details).toHaveTextContent('team=web')
  expect(
    screen.getByText(`docker push 127.0.0.1:5000/us-central1-docker.pkg.dev/${P}/images/IMAGE:TAG`),
  ).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Delete repository' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete repository' }),
  )
  await waitFor(() => expect(router.state.location.pathname).toBe('/ar'))
  expect(sent('DELETE', /repositories\/images$/)).toHaveLength(1)
})

it('edits only what changed, with an updateMask', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/ar/locations/us-central1/repositories/images/edit?project=${P}`)
  await user.clear(await screen.findByLabelText('Description'))
  await user.type(screen.getByLabelText('Description'), 'Web images')
  await user.click(screen.getByLabelText(/Immutable tags/))
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe(
      '/ar/locations/us-central1/repositories/images/overview',
    ),
  )
  const [patch] = sent('PATCH', /repositories\/images$/)
  expect(patch!.search.get('updateMask')).toBe('description,docker_config')
  expect(patch!.body).toEqual({ description: 'Web images', dockerConfig: { immutableTags: true } })
})

it('builds patch bodies and masks for every field', () => {
  const r: Repository = {
    name: `${US}/repositories/v`,
    mode: 'VIRTUAL_REPOSITORY',
    labels: { a: 'b' },
    virtualRepositoryConfig: { upstreamPolicies: [{ repository: REPO, priority: 1 }] },
  }
  const body = patchBody(
    r,
    { project: P, location: 'us-central1', repo: 'v' },
    { description: '', labels: '', immutableTags: false, upstreams: 'images=5\nhub=1' },
    {},
  )
  expect(body).toEqual({
    labels: {},
    virtualRepositoryConfig: {
      upstreamPolicies: [
        { id: 'images', repository: REPO, priority: 5 },
        { id: 'hub', repository: `${US}/repositories/hub`, priority: 1 },
      ],
    },
  })
  expect(updateMask(body)).toEqual(['labels', 'virtual_repository_config'])
})

it('nests an index’s manifests under it unless they are tagged', () => {
  const { top } = imageDigests(images, 'app')
  expect(top.map((i) => i.name.slice(-1))).toEqual(['1'])
  images[1]!.tags = ['amd64']
  expect(imageDigests(images, 'app').top).toHaveLength(2)
})
