import { createHash } from 'node:crypto'

import type { APIRequestContext, Page } from '@playwright/test'

import { expect, projectId, test } from './fixtures'

// The Artifact Registry view (SRS 4.8.3, FR-UI-011) against the
// Instance's ar Service: images are pushed to its registry with the OCI
// Distribution API, as docker would. Each attempt has its own Project,
// so a retry does not meet the repositories of the one before.

// The console's paths: its bundle, the admin API and public GCP API paths.
const allowed = /^\/(console\/|_emu\/v1\/|[a-z]+\/v\d[a-z0-9]*\/)/

const sha256 = (b: string) => `sha256:${createHash('sha256').update(b).digest('hex')}`
const short = (digest: string) => digest.slice('sha256:'.length, 'sha256:'.length + 12)

/** Registry pushes blobs and manifests to the Instance's registry. */
class Registry {
  constructor(
    private request: APIRequestContext,
    private base: string,
  ) {}

  static async open(request: APIRequestContext) {
    const endpoints = (await (await request.get('/_emu/v1/endpoints')).json()) as {
      ar: string
    }
    return new Registry(request, `http://${endpoints.ar}`)
  }

  async blob(name: string, content: string) {
    const digest = sha256(content)
    const start = await this.request.post(`${this.base}/v2/${name}/blobs/uploads/`)
    expect(start.status()).toBe(202)
    const loc = new URL(start.headers().location!, this.base)
    loc.searchParams.set('digest', digest)
    const put = await this.request.put(loc.toString(), {
      data: Buffer.from(content),
      headers: { 'Content-Type': 'application/octet-stream' },
    })
    expect(put.status()).toBe(201)
    return { digest, size: Buffer.byteLength(content) }
  }

  /** image pushes a one-layer image for a platform and returns its manifest. */
  async image(name: string, arch: string, ref?: string) {
    const config = await this.blob(name, JSON.stringify({ architecture: arch, os: 'linux' }))
    const layer = await this.blob(name, `layer for ${arch}`)
    const manifest = JSON.stringify({
      schemaVersion: 2,
      mediaType: 'application/vnd.oci.image.manifest.v1+json',
      config: { mediaType: 'application/vnd.oci.image.config.v1+json', ...config },
      layers: [{ mediaType: 'application/vnd.oci.image.layer.v1.tar', ...layer }],
    })
    return this.manifest(name, ref, manifest, 'application/vnd.oci.image.manifest.v1+json')
  }

  /** index pushes a multi-arch index of manifests, tagged. */
  async index(
    name: string,
    tag: string,
    manifests: { digest: string; size: number; arch: string }[],
  ) {
    const index = JSON.stringify({
      schemaVersion: 2,
      mediaType: 'application/vnd.oci.image.index.v1+json',
      manifests: manifests.map((m) => ({
        mediaType: 'application/vnd.oci.image.manifest.v1+json',
        digest: m.digest,
        size: m.size,
        platform: { os: 'linux', architecture: m.arch },
      })),
    })
    return this.manifest(name, tag, index, 'application/vnd.oci.image.index.v1+json')
  }

  async manifest(name: string, ref: string | undefined, body: string, mediaType: string) {
    const digest = sha256(body)
    const put = await this.request.put(`${this.base}/v2/${name}/manifests/${ref ?? digest}`, {
      data: Buffer.from(body),
      headers: { 'Content-Type': mediaType },
    })
    expect(put.status()).toBe(201)
    return { digest, size: Buffer.byteLength(body) }
  }

  /** pullStatus is the registry's answer to a pull of "HOST/PATH:TAG" or "HOST/PATH@DIGEST". */
  async pullStatus(ref: string) {
    const path = ref.slice(ref.indexOf('/') + 1)
    const at = path.includes('@') ? path.lastIndexOf('@') : path.lastIndexOf(':')
    const res = await this.request.head(
      `${this.base}/v2/${path.slice(0, at)}/manifests/${path.slice(at + 1)}`,
      {
        headers: {
          Accept:
            'application/vnd.oci.image.index.v1+json,application/vnd.oci.image.manifest.v1+json',
        },
      },
    )
    return res.status()
  }
}

const digestRow = (page: Page, digest: string) =>
  page.getByTestId('digest').filter({ has: page.getByRole('link', { name: short(digest) }) })

test('Artifact Registry: repositories, images, tags, digests and per-arch manifests', async ({
  page,
  browserName,
  context,
  request,
  requests,
}, testInfo) => {
  const id = projectId(`e2e-ar${testInfo.retry}`, browserName)
  await page.goto(`/console/ar?project=${id}`)
  await expect(page.getByText('No repositories in this Project yet.')).toBeVisible()
  await page.getByRole('link', { name: 'Create repository' }).click()

  // The form checks the ID as the API does; the API rejects a bad remote URL.
  await page.getByLabel('Name').fill('Images')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page.getByText('Invalid repository ID "Images"')).toBeVisible()
  await page.getByLabel('Name').fill('images')
  await page.getByLabel('Location').selectOption('us-central1')
  await page.getByLabel('Mode').selectOption('REMOTE_REPOSITORY')
  await page.getByLabel('Remote source').selectOption('custom')
  await page.getByLabel('Registry URL').fill('not a url')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(
    page.getByRole('alert').filter({
      hasText: 'Invalid remote_repository_config.docker_repository.custom_repository.uri',
    }),
  ).toBeVisible()
  await page.getByLabel('Mode').selectOption('STANDARD_REPOSITORY')
  await page.getByLabel('Labels').fill('team=e2e')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page).toHaveURL(
    new RegExp(`/console/ar/locations/us-central1/repositories/images\\?project=${id}$`),
  )
  await expect(page.getByText('No images yet')).toBeVisible()

  // Push a multi-arch image and a nested single-arch one; the view updates live.
  const reg = await Registry.open(request)
  const name = `us-central1-docker.pkg.dev/${id}/images`
  const amd64 = await reg.image(`${name}/app`, 'amd64')
  const arm64 = await reg.image(`${name}/app`, 'arm64')
  const index = await reg.index(`${name}/app`, 'v1', [
    { ...amd64, arch: 'amd64' },
    { ...arm64, arch: 'arm64' },
  ])
  const svc = await reg.image(`${name}/team/svc`, 'amd64', 'stable')
  const images = page.getByRole('table', { name: 'Images' })
  await expect(images.getByTestId('image')).toHaveCount(2)
  await expect(images.getByTestId('image').filter({ hasText: 'team/svc' })).toContainText('stable')

  // The image: the index with a row per platform, and pull commands that work.
  await images.getByRole('link', { name: 'app', exact: true }).click()
  await expect(page.getByTestId('digest')).toHaveCount(1)
  await expect(digestRow(page, index.digest)).toContainText('Index')
  await expect(digestRow(page, index.digest)).toContainText('v1')
  const platforms = page.getByRole('table', { name: 'Digests' }).getByTestId('platform')
  await expect(platforms).toHaveCount(2)
  await expect(platforms.nth(0)).toContainText('linux/amd64')
  await expect(platforms.nth(1)).toContainText('linux/arm64')
  if (browserName === 'chromium') {
    await context.grantPermissions(['clipboard-read', 'clipboard-write'])
    await page.getByRole('button', { name: `Copy pull command of ${short(index.digest)}` }).click()
    const command = await page.evaluate(() => navigator.clipboard.readText())
    expect(command).toMatch(new RegExp(`^docker pull \\S+/${name}/app:v1$`))
    expect(await reg.pullStatus(command.slice('docker pull '.length))).toBe(200)
  }

  // Tag the arm64 manifest from its digest page; its pull command works.
  await page.getByRole('link', { name: short(index.digest) }).click()
  await expect(page.getByLabel('Digest details')).toContainText(index.digest)
  await page.getByRole('link', { name: arm64.digest }).click()
  await page.getByRole('button', { name: `Add tag to ${short(arm64.digest)}` }).click()
  await page.getByRole('dialog').getByLabel('Tag').fill('arm')
  await page.getByRole('dialog').getByRole('button', { name: 'Add tag' }).click()
  await expect(page.getByLabel('Digest details')).toContainText('arm')
  const pull = page.getByText(/^docker pull .*:arm$/)
  await expect(pull).toBeVisible()
  expect(await reg.pullStatus((await pull.textContent())!.slice('docker pull '.length))).toBe(200)

  // Move v1 to it, then delete the tag.
  await page.getByRole('button', { name: `Add tag to ${short(arm64.digest)}` }).click()
  await page.getByRole('dialog').getByLabel('Tag').fill('v1')
  await page.getByRole('dialog').getByRole('button', { name: 'Move tag' }).click()
  await expect(page.getByLabel('Digest details').getByTestId('tag')).toHaveText(['arm', 'v1'])
  await page.getByRole('button', { name: 'Delete tag arm' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete tag' }).click()
  await expect(page.getByLabel('Digest details').getByTestId('tag')).toHaveText(['v1'])
  expect(await reg.pullStatus(`registry/${name}/app:arm`)).toBe(404)

  // Delete a digest: the tagged team/svc manifest, with its tag.
  await page.getByRole('link', { name: 'images', exact: true }).click()
  await page.getByRole('link', { name: 'team/svc' }).click()
  await digestRow(page, svc.digest)
    .getByRole('button', { name: `Delete digest ${short(svc.digest)}` })
    .click()
  await expect(page.getByRole('dialog')).toContainText('Its tags (stable) are deleted with it.')
  await page.getByRole('dialog').getByRole('button', { name: 'Delete digest' }).click()
  await expect(page.getByText('The image has no digests')).toBeVisible()

  // Edit the repository, then delete it.
  await page.getByRole('link', { name: 'images', exact: true }).click()
  await page.getByRole('link', { name: 'Edit repository' }).click()
  await page.getByLabel('Description').fill('E2E images')
  await page.getByLabel(/Immutable tags/).check()
  await page.getByRole('button', { name: 'Save' }).click()
  const details = page.getByLabel('Repository details')
  await expect(details).toContainText('E2E images')
  await expect(details).toContainText('team=e2e')
  await page.getByRole('button', { name: 'Delete repository' }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Delete repository' }).click()
  await expect(page).toHaveURL(new RegExp(`/console/ar\\?project=${id}$`))
  await expect(page.getByText('No repositories in this Project yet.')).toBeVisible()

  // FR-UI-003: only the console's own paths and public APIs on the gateway.
  const gateway = new URL(page.url()).origin
  for (const r of requests) {
    const u = new URL(r.url())
    if (u.origin === gateway) expect(u.pathname).toMatch(allowed)
  }
})
