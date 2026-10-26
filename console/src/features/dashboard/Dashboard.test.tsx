import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { expect, it, vi } from 'vitest'

import { POLL_MS } from '@/api/queries'
import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import { envScript } from './EnvCard'
import { humanize } from './ResourceCounts'

it('shows readiness with reasons, endpoints, runtime and resource counts', async () => {
  renderApp('/')
  expect(await screen.findByRole('heading', { name: 'Dashboard' })).toBeInTheDocument()

  const pubsub = await screen.findByTestId('ready-pubsub')
  expect(within(pubsub).getByText('Not ready')).toBeInTheDocument()
  expect(within(pubsub).getByText('loading persisted messages')).toBeInTheDocument()
  expect(within(screen.getByTestId('ready-gcs')).getByText('Ready')).toBeInTheDocument()

  const endpoints = screen.getByRole('table', { name: 'Service endpoints' })
  expect(within(endpoints).getByText('127.0.0.1:4443')).toBeInTheDocument()

  expect(screen.getByText('Reachable')).toBeInTheDocument()
  expect(screen.getByText('27.3.1', { exact: false })).toBeInTheDocument()

  const storage = screen.getByRole('region', { name: 'Cloud Storage' })
  expect(within(storage).getByText('Buckets').nextSibling).toHaveTextContent('3')
  expect(screen.getByText('This Instance runs no containers.')).toBeInTheDocument()
})

it('shows a failing card inline and keeps the rest', async () => {
  fake.containers = { status: 500, error: 'runtime API 500: daemon gone' }
  renderApp('/')
  expect(await screen.findByRole('alert')).toHaveTextContent('runtime API 500: daemon gone')
  expect(screen.getByRole('table', { name: 'Service endpoints' })).toBeInTheDocument()
})

it('lists Managed containers', async () => {
  fake.containers = [
    {
      name: 'gcpemu-a1b2-sql-main',
      service: 'sql',
      resource: 'projects/p/instances/main',
      role: 'postgres',
      image: 'postgres:17',
      state: 'running',
    },
  ]
  renderApp('/')
  expect(await screen.findByText('gcpemu-a1b2-sql-main')).toBeInTheDocument()
})

it('lists Delegated APIs and whether their External emulators answer', async () => {
  fake.external = [
    {
      api: 'firestore',
      address: 'localhost:8080',
      hosts: ['firestore.googleapis.com'],
      reachable: true,
    },
    {
      api: 'spanner',
      address: 'localhost:9010',
      hosts: ['spanner.googleapis.com'],
      reachable: false,
      error: 'connection refused',
    },
  ]
  renderApp('/')
  const table = await screen.findByRole('table', { name: 'Delegated APIs' })
  const firestore = within(table).getByText('firestore').closest('tr')!
  expect(within(firestore).getByText('localhost:8080')).toBeInTheDocument()
  expect(within(firestore).getByText('Reachable')).toBeInTheDocument()
  const spanner = within(table).getByText('spanner').closest('tr')!
  expect(within(spanner).getByText('Unreachable')).toBeInTheDocument()
  expect(within(spanner).getByText('connection refused')).toBeInTheDocument()
})

it('marks Managed emulators, with no address', async () => {
  fake.external = [
    {
      api: 'bigtable',
      address: '',
      managed: true,
      hosts: ['bigtable.googleapis.com', 'bigtableadmin.googleapis.com'],
      reachable: true,
    },
    {
      api: 'firestore',
      address: '',
      managed: true,
      hosts: ['firestore.googleapis.com'],
      reachable: false,
      error: 'it needs a container runtime, and none is usable',
    },
  ]
  renderApp('/')
  const table = await screen.findByRole('table', { name: 'Delegated APIs' })
  const bigtable = within(table).getByText('bigtable').closest('tr')!
  expect(within(bigtable).getByText('Managed')).toBeInTheDocument()
  expect(within(bigtable).getByText('Reachable')).toBeInTheDocument()
  const firestore = within(table).getByText('firestore').closest('tr')!
  expect(within(firestore).getByText('Managed')).toBeInTheDocument()
  expect(within(firestore).getByText('Unreachable')).toBeInTheDocument()
})

it('shows no Delegated APIs card without --external', async () => {
  renderApp('/')
  await screen.findByRole('table', { name: 'Service endpoints' })
  expect(screen.queryByRole('table', { name: 'Delegated APIs' })).not.toBeInTheDocument()
})

it('says when no container runtime is reachable', async () => {
  fake.info.runtime = { kind: 'none', reachable: false, error: 'no container runtime found' }
  renderApp('/')
  expect(await screen.findByText('No container runtime found')).toBeInTheDocument()
  expect(screen.getByText('no container runtime found')).toBeInTheDocument()
})

it('copies the gcpemu env output', async () => {
  const user = userEvent.setup()
  const write = vi.spyOn(navigator.clipboard, 'writeText')
  renderApp('/')
  const out = await screen.findByTestId('env-output')
  expect(out).toHaveTextContent("export PUBSUB_EMULATOR_HOST='127.0.0.1:8085'\\''x'")
  await user.click(screen.getByRole('button', { name: 'Copy' }))
  expect(write).toHaveBeenCalledWith(envScript(fake.env))
  expect(await screen.findByRole('button', { name: 'Copied' })).toBeInTheDocument()
})

it('envScript matches `gcpemu env` for bash', () => {
  expect(envScript({ B: "it's", A: '1' })).toBe("export A='1'\nexport B='it'\\''s'\n")
})

it('shows the error page when the Instance cannot be reached', async () => {
  server.use(http.get('*/_emu/v1/info', () => HttpResponse.error()))
  renderApp('/')
  expect(await screen.findByText(/Cannot reach the emulator/)).toBeInTheDocument()
})

it('humanizes resource types', () => {
  expect(humanize('resourceRecordSets')).toBe('Resource record sets')
  expect(humanize('hmacKeys')).toBe('HMAC keys')
  expect(humanize('sslCertificates')).toBe('SSL certificates')
  expect(humanize('targetHttpsProxies')).toBe('Target HTTPS proxies')
})

it('does not poll resource counts: the event stream keeps them fresh (FR-UI-006)', async () => {
  let fetched = 0
  server.use(
    http.get('*/_emu/v1/resources/counts', () => {
      fetched++
      return HttpResponse.json(fake.counts)
    }),
  )
  vi.useFakeTimers({ shouldAdvanceTime: true })
  try {
    renderApp('/')
    await screen.findByRole('region', { name: 'Cloud Storage' })
    expect(fetched).toBe(1)
    await vi.advanceTimersByTimeAsync(10 * POLL_MS)
    expect(fetched).toBe(1)
  } finally {
    vi.useRealTimers()
  }
})
