import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { expect, it, vi } from 'vitest'

import type { Operation } from '@/api/admin'
import { POLL_MS } from '@/api/queries'
import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import { durationMs, formatDuration, matches, operationState } from './format'

const rows = () => screen.getAllByTestId('operation')

it('lists every Service’s Operations with status, duration and resource or error', async () => {
  renderApp('/operations')
  expect(await screen.findByRole('heading', { name: 'Operations' })).toBeInTheDocument()
  await screen.findByRole('table', { name: 'Operations' })
  const [running, failed, done] = rows()

  expect(within(running!).getByText('Running')).toBeInTheDocument()
  expect(within(running!).getByText('VPC network')).toBeInTheDocument()
  expect(
    within(running!).getByText('projects/alpha-project/global/networks/vpc-b'),
  ).toBeInTheDocument()

  expect(within(failed!).getByText('Failed')).toBeInTheDocument()
  expect(within(failed!).getByText(/The resource 'vpc-a' already exists/)).toBeInTheDocument()
  expect(within(failed!).getByText(/RESOURCE_ALREADY_EXISTS/)).toBeInTheDocument()
  expect(within(failed!).getByText('1.5 s')).toBeInTheDocument()

  expect(within(done!).getByText('Done')).toBeInTheDocument()
  expect(within(done!).getByText('Cloud DNS')).toBeInTheDocument()
  expect(within(done!).getByText('change')).toBeInTheDocument()
  expect(within(done!).getByText('250 ms')).toBeInTheDocument()
})

it('is reached from the sidebar', async () => {
  const user = userEvent.setup()
  renderApp('/?project=alpha-project')
  await user.click(
    within(await screen.findByRole('navigation', { name: 'Services' })).getByRole('link', {
      name: 'Operations',
    }),
  )
  expect(await screen.findByRole('heading', { name: 'Operations' })).toBeInTheDocument()
})

it('expands an Operation to its details and the Operation as the API returns it', async () => {
  const user = userEvent.setup()
  renderApp('/operations')
  await screen.findByRole('table', { name: 'Operations' })
  const failed = rows()[1]!
  await user.click(within(failed).getByRole('button', { name: 'Show details' }))
  expect(within(failed).getByRole('button', { name: 'Hide details' })).toHaveAttribute(
    'aria-expanded',
    'true',
  )
  const json = screen.getByLabelText('Operation JSON')
  expect(JSON.parse(json.textContent ?? '')).toEqual(fake.operations[1]!.operation)
  expect(screen.getByText('projects/alpha-project/global/operations/operation-2')).toBeVisible()
})

it('scopes to the chosen Project', async () => {
  let asked: string | null = ''
  server.use(
    http.get('*/_emu/v1/operations', ({ request }) => {
      asked = new URL(request.url).searchParams.get('project')
      return HttpResponse.json({ operations: fake.operations.filter((op) => op.project === asked) })
    }),
  )
  renderApp('/operations?project=beta-project')
  await screen.findByRole('table', { name: 'Operations' })
  expect(asked).toBe('beta-project')
  expect(rows()).toHaveLength(1)
  expect(screen.getByText(/Operations of Project/)).toHaveTextContent('beta-project')
})

it('filters by Service, status and text, keeping the filters in the URL', async () => {
  const user = userEvent.setup()
  const { router } = renderApp('/operations?status=failed')
  await screen.findByRole('table', { name: 'Operations' })
  expect(rows()).toHaveLength(1)
  expect(screen.getByLabelText('Status')).toHaveValue('failed')

  await user.selectOptions(screen.getByLabelText('Status'), '')
  expect(rows()).toHaveLength(3)
  await user.selectOptions(screen.getByLabelText('Service'), 'dns')
  expect(rows()).toHaveLength(1)
  expect(router.state.location.search).toBe('?service=dns')

  await user.selectOptions(screen.getByLabelText('Service'), '')
  await user.type(screen.getByLabelText('Filter'), 'vpc-')
  expect(rows()).toHaveLength(2)
  expect(router.state.location.search).toBe('?q=vpc-')
  await user.type(screen.getByLabelText('Filter'), 'zzz')
  expect(screen.getByText('No Operations match the filters.')).toBeInTheDocument()
})

it('says when there are no Operations', async () => {
  fake.operations = []
  renderApp('/operations')
  expect(await screen.findByText('No Operations yet.')).toBeInTheDocument()
})

it('shows a failure inline', async () => {
  server.use(
    http.get('*/_emu/v1/operations', () =>
      HttpResponse.json({ error: 'store closed' }, { status: 500 }),
    ),
  )
  renderApp('/operations')
  expect(await screen.findByRole('alert')).toHaveTextContent('store closed')
})

it('advances running durations live and polls only while an Operation runs', async () => {
  let fetched = 0
  server.use(
    http.get('*/_emu/v1/operations', () => {
      fetched++
      return HttpResponse.json({ operations: fake.operations })
    }),
  )
  vi.useFakeTimers({ shouldAdvanceTime: true, now: new Date('2026-10-08T12:00:05Z') })
  try {
    renderApp('/operations')
    await screen.findByRole('table', { name: 'Operations' })
    const running = () => rows()[0]!
    // Seconds shown as the running Operation's duration. Real time
    // advances too (shouldAdvanceTime), so only bounds are exact.
    const shown = () => parseFloat(within(running()).getByText(/^\d+\.\d s$/).textContent ?? '')
    expect(await within(running()).findByText(/^\d+\.\d s$/)).toBeInTheDocument()
    const before = shown()
    expect(before).toBeGreaterThanOrEqual(2)
    await vi.advanceTimersByTimeAsync(3000)
    expect(shown() - before).toBeGreaterThanOrEqual(2)
    expect(fetched).toBeGreaterThan(1)

    // Once it is done the view stops polling: Operation events report changes.
    fake.operations[0] = {
      ...fake.operations[0]!,
      done: true,
      status: 'DONE',
      endTime: '2026-10-08T12:00:09Z',
    }
    await vi.advanceTimersByTimeAsync(POLL_MS)
    expect(await within(running()).findByText('Done')).toBeInTheDocument()
    expect(within(running()).getByText('6.0 s')).toBeInTheDocument()
    const after = fetched
    await vi.advanceTimersByTimeAsync(10 * POLL_MS)
    expect(fetched).toBe(after)
  } finally {
    vi.useRealTimers()
  }
})

const op = (o: Partial<Operation>): Operation => ({ service: 'gke', name: 'n', done: true, ...o })

it('derives state and duration', () => {
  expect(operationState(op({ done: false }))).toBe('running')
  expect(operationState(op({ error: { message: 'x' } }))).toBe('failed')
  expect(operationState(op({}))).toBe('succeeded')

  const now = Date.parse('2026-10-08T12:01:00Z')
  expect(durationMs(op({ done: false, startTime: '2026-10-08T12:00:00Z' }), now)).toBe(60_000)
  expect(
    durationMs(op({ startTime: '2026-10-08T12:00:00Z', endTime: '2026-10-08T12:00:01Z' }), now),
  ).toBe(1000)
  // Not recorded: Operations persisted before start times were kept.
  expect(durationMs(op({}), now)).toBeUndefined()
  expect(durationMs(op({ startTime: '2026-10-08T12:00:00Z' }), now)).toBeUndefined()
})

it('formats durations', () => {
  expect(formatDuration(0)).toBe('0 ms')
  expect(formatDuration(999)).toBe('999 ms')
  expect(formatDuration(2400)).toBe('2.4 s')
  expect(formatDuration(185_000)).toBe('3 m 05 s')
  expect(formatDuration(3_720_000)).toBe('1 h 02 m')
})

it('matches text in the name, type, resource or error', () => {
  const o = op({
    type: 'CREATE_CLUSTER',
    target: 'projects/p/clusters/c',
    error: { message: 'No Quota' },
  })
  expect(matches(o, '')).toBe(true)
  expect(matches(o, 'create_')).toBe(true)
  expect(matches(o, 'clusters/c')).toBe(true)
  expect(matches(o, 'quota')).toBe(true)
  expect(matches(o, 'bucket')).toBe(false)
})
