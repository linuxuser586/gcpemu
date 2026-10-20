import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, expect, it } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import type { Certificate, CertificateMap, CertificateMapEntry } from './api'
import { entryRecipe, mapRecipe } from './copy'

const P = 'alpha-project'
const G = `projects/${P}/locations/global`
const MAIN = `${G}/certificateMaps/main`

// A small in-memory Certificate Manager REST API for certificate maps and
// their entries, with the certificates an entry can serve; operations are
// done at once. The compute API answers with the forwarding rule of the
// map's target proxy.
let certs: Certificate[] = []
let maps: CertificateMap[] = []
let entries: CertificateMapEntry[] = []
let requests: { method: string; path: string; search: URLSearchParams; body: unknown }[] = []

const err = (code: number, status: string, message: string) =>
  HttpResponse.json({ error: { code, status, message } }, { status: code })

const done = (response: unknown) =>
  HttpResponse.json({ name: `${G}/operations/op-1`, done: true, response })

beforeEach(() => {
  requests = []
  fake.info.services = ['iam', 'certs', 'lb']
  certs = [
    { name: `${G}/certificates/self`, sanDnsnames: ['www.example.com'] },
    { name: `${G}/certificates/other`, sanDnsnames: ['api.example.com'] },
    { name: `projects/${P}/locations/us-central1/certificates/regional` },
  ]
  maps = [
    {
      name: MAIN,
      description: 'Production',
      labels: { team: 'web' },
      createTime: '2026-10-01T10:00:00Z',
      gclbTargets: [
        {
          targetHttpsProxy: `projects/${P}/global/targetHttpsProxies/web-https`,
          ipConfigs: [{ ipAddress: '34.120.0.1', ports: [443] }],
        },
      ],
    },
    { name: `${G}/certificateMaps/spare` },
  ]
  entries = [
    {
      name: `${MAIN}/certificateMapEntries/www`,
      hostname: 'www.example.com',
      certificates: [`${G}/certificates/self`],
      state: 'ACTIVE',
      labels: { tier: 'edge' },
    },
    {
      name: `${MAIN}/certificateMapEntries/fallback`,
      matcher: 'PRIMARY',
      certificates: [`${G}/certificates/self`, `${G}/certificates/other`],
      state: 'PENDING',
    },
  ]
  server.use(
    http.all(/\/certificatemanager\/v1\/(.*)$/, async ({ request }) => {
      const url = new URL(request.url)
      const text = request.method === 'GET' ? '' : await request.text()
      const body: unknown = text ? JSON.parse(text) : undefined
      const path = decodeURIComponent(url.pathname.replace(/^.*\/certificatemanager\/v1\//, ''))
      requests.push({ method: request.method, path, search: url.searchParams, body })
      if (/^projects\/[^/]+\/locations\/-\/certificates$/.test(path)) {
        return HttpResponse.json({ certificates: certs })
      }
      let m = /^(projects\/[^/]+\/locations\/global)\/certificateMaps(?:\/([^/]+))?$/.exec(path)
      if (m) {
        const [, parent, id] = m
        if (!id) {
          if (request.method === 'GET')
            return HttpResponse.json(maps.length ? { certificateMaps: maps } : {})
          const name = `${parent}/certificateMaps/${url.searchParams.get('certificateMapId')}`
          if (maps.some((x) => x.name === name)) {
            return err(409, 'ALREADY_EXISTS', `Certificate map ${name} already exists.`)
          }
          const cm = { ...(body as CertificateMap), name }
          maps.push(cm)
          return done(cm)
        }
        const cm = maps.find((x) => x.name === `${parent}/certificateMaps/${id}`)
        if (!cm) return err(404, 'NOT_FOUND', `Certificate map ${id} not found.`)
        if (request.method === 'DELETE') {
          maps = maps.filter((x) => x !== cm)
          return done({})
        }
        if (request.method === 'PATCH') {
          Object.assign(cm, body)
          return done(cm)
        }
        return HttpResponse.json(cm)
      }
      m =
        /^(projects\/[^/]+\/locations\/global\/certificateMaps\/[^/]+)\/certificateMapEntries(?:\/([^/]+))?$/.exec(
          path,
        )
      if (!m) return err(404, 'NOT_FOUND', `bad path ${path}`)
      const [, parent, id] = m
      if (!id) {
        if (request.method === 'GET') {
          const list = entries.filter((e) => e.name.startsWith(`${parent}/`))
          return HttpResponse.json(list.length ? { certificateMapEntries: list } : {})
        }
        const name = `${parent}/certificateMapEntries/${url.searchParams.get('certificateMapEntryId')}`
        const e = { ...(body as CertificateMapEntry), name, state: 'ACTIVE' }
        entries.push(e)
        return done(e)
      }
      const e = entries.find((x) => x.name === `${parent}/certificateMapEntries/${id}`)
      if (!e) return err(404, 'NOT_FOUND', `Certificate map entry ${id} not found.`)
      if (request.method === 'DELETE') {
        entries = entries.filter((x) => x !== e)
        return done({})
      }
      if (request.method === 'PATCH') {
        Object.assign(e, body)
        return done(e)
      }
      return HttpResponse.json(e)
    }),
    http.get(`*/compute/v1/projects/${P}/aggregated/forwardingRules`, () =>
      HttpResponse.json({
        items: {
          global: {
            forwardingRules: [
              {
                name: 'web-443',
                selfLink: `https://www.googleapis.com/compute/v1/projects/${P}/global/forwardingRules/web-443`,
                target: `https://www.googleapis.com/compute/v1/projects/${P}/global/targetHttpsProxies/web-https`,
                IPAddress: '34.120.0.1',
                portRange: '443-443',
              },
            ],
          },
        },
      }),
    ),
  )
})

const sent = (method: string, path: RegExp) =>
  requests.filter((r) => r.method === method && path.test(r.path))

it('lists the Project’s certificate maps with their targets', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs?project=${P}`)
  await user.click(await screen.findByRole('link', { name: 'Certificate maps' }))
  const table = await screen.findByRole('table', { name: 'Certificate maps' })
  const rows = within(table).getAllByTestId('certificate-map')
  expect(rows).toHaveLength(2)
  expect(rows[0]).toHaveTextContent(/main.*Production.*1/)
  expect(rows[1]).toHaveTextContent(/spare.*0/)
  await user.click(within(table).getByRole('link', { name: 'main' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/certs/locations/global/certificateMaps/main'),
  )
})

it('shows a map’s entries and the target proxies and forwarding rules that serve it', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/locations/global/certificateMaps/main?project=${P}`)
  expect(await screen.findByRole('heading', { name: 'main' })).toBeInTheDocument()
  expect(screen.getByLabelText('Certificate map details')).toHaveTextContent('team=web')

  const table = await screen.findByRole('table', { name: 'Certificate map entries' })
  const [www, fallback] = within(table).getAllByTestId('certificate-map-entry')
  expect(www).toHaveTextContent(/www.*www\.example\.com.*self.*ACTIVE/)
  expect(fallback).toHaveTextContent(/fallback.*Primary.*self.*other.*PENDING/)
  expect(within(www!).getByRole('link', { name: 'self' })).toHaveAttribute(
    'href',
    `/certs/locations/global/certificates/self?project=${P}`,
  )

  const targets = screen.getByRole('table', { name: 'Target proxies' })
  const [row] = within(targets).getAllByTestId('gclb-target')
  expect(row).toHaveTextContent(/web-https.*34\.120\.0\.1:443/)
  expect(within(row!).getByRole('link', { name: 'web-https' })).toHaveAttribute(
    'href',
    `/lb/global/targetHttpsProxies/web-https?project=${P}`,
  )
  await user.click(await within(row!).findByRole('link', { name: 'web-443' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/lb/global/forwardingRules/web-443'),
  )
})

it('says when no load balancer serves a map', async () => {
  renderApp(`/certs/locations/global/certificateMaps/spare?project=${P}`)
  expect(
    await screen.findByText('No target proxy serves this certificate map yet.'),
  ).toBeInTheDocument()
  expect(screen.getByText('No entries in this certificate map yet.')).toBeInTheDocument()
})

it('deletes an entry from the map’s page, and then the map', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/locations/global/certificateMaps/main?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Certificate map entries' })
  await user.click(within(table).getByRole('button', { name: 'Delete entry fallback' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete entry' }),
  )
  await waitFor(() => expect(within(table).getAllByTestId('certificate-map-entry')).toHaveLength(1))
  expect(sent('DELETE', /certificateMapEntries\/fallback$/)).toHaveLength(1)

  await user.click(screen.getByRole('button', { name: 'Delete certificate map' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', {
      name: 'Delete certificate map',
    }),
  )
  await waitFor(() => expect(router.state.location.pathname).toBe('/certs/maps'))
  expect(sent('DELETE', /certificateMaps\/main$/)).toHaveLength(1)
})

it('creates a certificate map, as the API checks its ID', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/maps?project=${P}`)
  await user.click(await screen.findByRole('link', { name: 'Create certificate map' }))
  await user.type(await screen.findByLabelText('Name'), 'Bad_ID')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(
    await screen.findByText(/Invalid certificateMapId "Bad_ID": must be 1-63 characters/),
  ).toBeInTheDocument()
  await user.clear(screen.getByLabelText('Name'))
  await user.type(screen.getByLabelText('Name'), 'edge')
  await user.type(screen.getByLabelText('Description'), 'Edge sites')
  await user.type(screen.getByLabelText('Labels'), 'team=edge')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/certs/locations/global/certificateMaps/edge'),
  )
  const [post] = sent('POST', /global\/certificateMaps$/)
  expect(post!.search.get('certificateMapId')).toBe('edge')
  expect(post!.body).toEqual({ description: 'Edge sites', labels: { team: 'edge' } })
})

it('edits a map’s labels and description, with an updateMask', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/locations/global/certificateMaps/main?project=${P}`)
  await user.click(await screen.findByRole('link', { name: 'Edit certificate map' }))
  const labels = await screen.findByLabelText('Labels')
  expect(labels).toHaveValue('team=web')
  await user.clear(labels)
  await user.type(labels, 'team=platform')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/certs/locations/global/certificateMaps/main'),
  )
  const [patch] = sent('PATCH', /certificateMaps\/main$/)
  expect(patch!.search.get('updateMask')).toBe('labels')
  expect(patch!.body).toEqual({ labels: { team: 'platform' } })
})

it('adds a hostname entry with global certificates, as the API checks it', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/locations/global/certificateMaps/main?project=${P}`)
  await user.click(await screen.findByRole('link', { name: 'Add entry' }))
  await user.type(await screen.findByLabelText('Name'), 'api')
  await user.type(screen.getByLabelText('Hostname'), 'not a host')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(await screen.findByText('Invalid hostname "not a host".')).toBeInTheDocument()
  expect(screen.getByText('Field certificates is required.')).toBeInTheDocument()

  // Only global certificates can serve a map.
  const certificates = screen.getByRole('group', { name: 'Certificates' })
  expect(within(certificates).getAllByRole('checkbox')).toHaveLength(2)
  await user.click(within(certificates).getByRole('checkbox', { name: /other/ }))
  await user.clear(screen.getByLabelText('Hostname'))
  await user.type(screen.getByLabelText('Hostname'), 'API.example.com.')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/certs/locations/global/certificateMaps/main'),
  )
  const [post] = sent('POST', /certificateMaps\/main\/certificateMapEntries$/)
  expect(post!.search.get('certificateMapEntryId')).toBe('api')
  expect(post!.body).toEqual({
    hostname: 'api.example.com',
    certificates: [`${G}/certificates/other`],
  })
})

it('adds the primary entry, which serves every other hostname', async () => {
  const user = userEvent.setup()
  renderApp(`/certs/locations/global/certificateMaps/spare/create-entry?project=${P}`)
  await user.type(await screen.findByLabelText('Name'), 'default')
  await user.selectOptions(screen.getByLabelText('Serves'), 'PRIMARY')
  expect(screen.queryByLabelText('Hostname')).not.toBeInTheDocument()
  await user.click(await screen.findByRole('checkbox', { name: /self/ }))
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() =>
    expect(sent('POST', /certificateMaps\/spare\/certificateMapEntries$/)).toHaveLength(1),
  )
  expect(sent('POST', /certificateMapEntries$/)[0]!.body).toEqual({
    matcher: 'PRIMARY',
    certificates: [`${G}/certificates/self`],
  })
})

it('edits an entry’s certificates and labels from the map’s page, with an updateMask', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/certs/locations/global/certificateMaps/main?project=${P}`)
  await user.click(await screen.findByRole('link', { name: 'Edit entry www' }))
  expect(await screen.findByText('www.example.com')).toBeInTheDocument()
  expect(screen.queryByLabelText('Hostname')).not.toBeInTheDocument()
  const certificates = screen.getByRole('group', { name: 'Certificates' })
  expect(within(certificates).getByRole('checkbox', { name: /self/ })).toBeChecked()
  await user.click(within(certificates).getByRole('checkbox', { name: /other/ }))
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe('/certs/locations/global/certificateMaps/main'),
  )
  const [patch] = sent('PATCH', /certificateMapEntries\/www$/)
  expect(patch!.search.get('updateMask')).toBe('certificates')
  expect(patch!.body).toEqual({
    certificates: [`${G}/certificates/self`, `${G}/certificates/other`],
  })
})

it('copies a map as gcloud, curl and OpenTofu', async () => {
  const user = userEvent.setup()
  renderApp(`/certs/locations/global/certificateMaps/main?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Copy certificate map as' }))
  const dialog = await screen.findByRole('dialog', { name: 'Copy certificate map as' })
  expect(within(dialog).getByLabelText('gcloud for the certificate map')).toHaveTextContent(
    [
      'gcloud certificate-manager maps create main \\',
      `    --project=${P} \\`,
      '    --description=Production \\',
      '    --labels=team=web',
    ].join('\n'),
    { normalizeWhitespace: false },
  )
  await user.click(within(dialog).getByRole('tab', { name: 'curl' }))
  const curl = within(dialog).getByLabelText('curl for the certificate map')
  expect(curl).toHaveTextContent(
    `curl '${location.origin}/certificatemanager/v1/projects/${P}/locations/global/certificateMaps?certificateMapId=main' \\ -X POST`,
  )
  expect(curl).not.toHaveTextContent('gclbTargets')
})

it('copies each entry of the map’s page', async () => {
  const user = userEvent.setup()
  renderApp(`/certs/locations/global/certificateMaps/main?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Certificate map entries' })
  await user.click(within(table).getByRole('button', { name: 'Copy certificate map entry www as' }))
  const dialog = await screen.findByRole('dialog', { name: 'Copy certificate map entry as' })
  expect(within(dialog).getByLabelText('gcloud for the certificate map entry')).toHaveTextContent(
    'gcloud certificate-manager maps entries create www',
  )
})

it('copies hostname and primary entries', () => {
  const [www, fallback] = entries
  const host = entryRecipe(www!, P)
  expect(host.gcloud).toBe(
    [
      'gcloud certificate-manager maps entries create www \\',
      `    --project=${P} \\`,
      '    --map=main \\',
      '    --labels=tier=edge \\',
      '    --hostname=www.example.com \\',
      '    --certificates=self',
    ].join('\n'),
  )
  expect(host.rest).toEqual({
    method: 'POST',
    path: `/certificatemanager/v1/${MAIN}/certificateMapEntries?certificateMapEntryId=www`,
    body: {
      labels: { tier: 'edge' },
      hostname: 'www.example.com',
      certificates: [`${G}/certificates/self`],
    },
  })
  expect(host.tofu).toBe(
    [
      'resource "google_certificate_manager_certificate_map_entry" "www" {',
      `  project      = "${P}"`,
      '  name         = "www"',
      '  map          = "main"',
      '  labels       = { tier = "edge" }',
      '  hostname     = "www.example.com"',
      `  certificates = ["${G}/certificates/self"]`,
      '}',
    ].join('\n'),
  )

  const primary = entryRecipe(fallback!, P)
  expect(primary.gcloud).toContain('    --set-primary \\\n    --certificates=self,other')
  expect(primary.tofu).toContain('matcher      = "PRIMARY"')

  expect(mapRecipe(maps[1]!, P).tofu).toBe(
    [
      'resource "google_certificate_manager_certificate_map" "spare" {',
      `  project = "${P}"`,
      '  name    = "spare"',
      '}',
    ].join('\n'),
  )
})
