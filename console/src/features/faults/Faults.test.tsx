import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { expect, it } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import type { FaultRule } from './api'
import { ruleBody, ruleValues, type Values } from './FaultForm'
import { effect, matches, when } from './Faults'

const rows = () => screen.getAllByTestId('fault')
const row = (id: string) => rows().find((r) => r.dataset.id === id)!

it('lists the rules with their effect, frequency and hit count', async () => {
  renderApp('/faults')
  expect(await screen.findByRole('heading', { name: 'Fault rules' })).toBeInTheDocument()
  await screen.findByRole('table', { name: 'Fault rules' })
  expect(rows().map((r) => r.dataset.id)).toEqual(['flaky-publish', 'slow-uploads', 'drop-all'])

  const flaky = row('flaky-publish')
  expect(within(flaky).getByRole('switch', { name: 'Rule flaky-publish enabled' })).toBeChecked()
  expect(within(flaky).getByText('Pub/Sub')).toBeInTheDocument()
  expect(within(flaky).getByText('/google.pubsub.v1.Publisher/Publish')).toBeInTheDocument()
  expect(within(flaky).getByText('UNAVAILABLE')).toBeInTheDocument()
  expect(within(flaky).getByText('25% of matches')).toBeInTheDocument()
  expect(within(flaky).getByLabelText('7 hits')).toHaveTextContent('7')

  const slow = row('slow-uploads')
  expect(within(slow).getByText('Delay 2s')).toBeInTheDocument()
  expect(within(slow).getByLabelText('3 hits')).toHaveTextContent('3 / 3')
  expect(within(slow).getByText('Used up')).toBeInTheDocument()

  const drop = row('drop-all')
  expect(within(drop).getByRole('switch')).not.toBeChecked()
  expect(within(drop).getByText('Any')).toBeInTheDocument()
  expect(within(drop).getByText('Drop the connection')).toBeInTheDocument()

  expect(screen.getByText('1 of 3 rules can fire.')).toBeInTheDocument()
})

it('is reached from the sidebar', async () => {
  const user = userEvent.setup()
  renderApp('/?project=alpha-project')
  await user.click(
    within(await screen.findByRole('navigation', { name: 'Services' })).getByRole('link', {
      name: 'Fault rules',
    }),
  )
  expect(await screen.findByRole('heading', { name: 'Fault rules' })).toBeInTheDocument()
})

it('says when there are no rules', async () => {
  fake.faults = []
  renderApp('/faults')
  expect(
    await screen.findByText('No fault rules: every request is handled normally.'),
  ).toBeInTheDocument()
  expect(screen.queryByRole('button', { name: 'Remove all' })).not.toBeInTheDocument()
})

it('disables and enables a rule', async () => {
  const user = userEvent.setup()
  const sent: unknown[] = []
  server.events.on('request:start', ({ request }) => {
    if (request.method === 'PATCH')
      void request
        .clone()
        .json()
        .then((b) => sent.push(b))
  })
  renderApp('/faults')
  await screen.findByRole('table', { name: 'Fault rules' })
  const toggle = within(row('flaky-publish')).getByRole('switch')

  await user.click(toggle)
  await waitFor(() => expect(toggle).not.toBeChecked())
  expect(await screen.findByText('Disabled rule flaky-publish.')).toBeInTheDocument()
  expect(fake.faults[0]!.disabled).toBe(true)
  // Disabling keeps the hit count.
  expect(within(row('flaky-publish')).getByLabelText('7 hits')).toBeInTheDocument()

  await user.click(within(row('flaky-publish')).getByRole('switch'))
  expect(await screen.findByText('Enabled rule flaky-publish.')).toBeInTheDocument()
  await waitFor(() => expect(within(row('flaky-publish')).getByRole('switch')).toBeChecked())
  expect(sent).toEqual([{ disabled: true }, { disabled: false }])
  server.events.removeAllListeners()
})

it('shows the API’s error when a toggle fails', async () => {
  const user = userEvent.setup()
  server.use(
    http.patch('*/_emu/v1/faults/:id', () =>
      HttpResponse.json({ error: 'no such fault' }, { status: 404 }),
    ),
  )
  renderApp('/faults')
  await screen.findByRole('table', { name: 'Fault rules' })
  await user.click(within(row('drop-all')).getByRole('switch'))
  expect(await screen.findByText('no such fault')).toBeInTheDocument()
  expect(within(row('drop-all')).getByRole('switch')).not.toBeChecked()
})

it('removes one rule, or all of them', async () => {
  const user = userEvent.setup()
  renderApp('/faults')
  await screen.findByRole('table', { name: 'Fault rules' })

  await user.click(screen.getByRole('button', { name: 'Remove rule slow-uploads' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Remove' }),
  )
  await waitFor(() => expect(rows()).toHaveLength(2))
  expect(fake.faults.map((r) => r.id)).toEqual(['flaky-publish', 'drop-all'])

  await user.click(screen.getByRole('button', { name: 'Remove all' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Remove all' }),
  )
  expect(
    await screen.findByText('No fault rules: every request is handled normally.'),
  ).toBeInTheDocument()
  expect(fake.faults).toEqual([])
})

it('filters by Service and text, keeping the filters in the URL', async () => {
  const user = userEvent.setup()
  const { router } = renderApp('/faults?service=gcs')
  await screen.findByRole('table', { name: 'Fault rules' })
  // A rule for any Service applies to Cloud Storage too.
  expect(rows().map((r) => r.dataset.id)).toEqual(['slow-uploads', 'drop-all'])

  await user.selectOptions(screen.getByLabelText('Service'), '')
  await waitFor(() => expect(rows()).toHaveLength(3))
  await user.type(screen.getByLabelText('Filter'), 'publish')
  await waitFor(() => expect(rows().map((r) => r.dataset.id)).toEqual(['flaky-publish']))
  expect(router.state.location.search).toBe('?q=publish')
})

it('adds a rule from the form', async () => {
  const user = userEvent.setup()
  let sent: unknown
  server.events.on('request:start', ({ request }) => {
    if (request.method === 'POST')
      void request
        .clone()
        .json()
        .then((b: unknown) => {
          sent = b
        })
  })
  const { router } = renderApp('/faults?project=alpha-project')
  await user.click(await screen.findByRole('link', { name: 'Add rule' }))
  expect(await screen.findByRole('heading', { name: 'Add fault rule' })).toBeInTheDocument()

  await user.selectOptions(screen.getByLabelText('Service'), 'gcs')
  await user.type(screen.getByLabelText('ID'), 'no-buckets')
  await user.type(screen.getByLabelText('Method'), '^GET /storage/v1/b')
  await user.selectOptions(screen.getByLabelText('Error code'), 'NOT_FOUND')
  await user.type(screen.getByLabelText('HTTP status'), '418')
  await user.type(screen.getByLabelText('Count'), '2')
  await user.click(screen.getByLabelText('Add disabled'))
  await user.click(screen.getByRole('button', { name: 'Add rule' }))

  expect(await screen.findByText('Added rule no-buckets, disabled.')).toBeInTheDocument()
  expect(sent).toEqual({
    id: 'no-buckets',
    service: 'gcs',
    method: '^GET /storage/v1/b',
    code: 'NOT_FOUND',
    httpStatus: 418,
    count: 2,
    disabled: true,
  })
  await waitFor(() => expect(rows()).toHaveLength(4))
  expect(router.state.location.pathname).toBe('/faults')
  expect(router.state.location.search).toBe('?project=alpha-project')
  expect(within(row('no-buckets')).getByRole('switch')).not.toBeChecked()
  server.events.removeAllListeners()
})

it('checks the form as the API does', async () => {
  const user = userEvent.setup()
  renderApp('/faults/create')
  await screen.findByRole('heading', { name: 'Add fault rule' })
  await user.selectOptions(screen.getByLabelText('Error code'), '')
  await user.type(screen.getByLabelText('Latency'), '2 seconds')
  await user.type(screen.getByLabelText('Probability'), '1.5')
  await user.type(screen.getByLabelText('HTTP status'), '99')
  await user.click(screen.getByRole('button', { name: 'Add rule' }))
  expect(
    await screen.findByText('Latency must be a duration, e.g. 250ms or 2s.'),
  ).toBeInTheDocument()
  expect(screen.getByText('Probability must be in [0,1]; 0 means always.')).toBeInTheDocument()
  expect(screen.getByText('HTTP status must be between 100 and 599.')).toBeInTheDocument()

  await user.clear(screen.getByLabelText('Latency'))
  await user.clear(screen.getByLabelText('Probability'))
  await user.clear(screen.getByLabelText('HTTP status'))
  await user.click(screen.getByRole('button', { name: 'Add rule' }))
  expect(
    await screen.findByText('A rule needs an error code, a latency or a dropped connection.'),
  ).toBeInTheDocument()
})

it('adds a rule from the JSON editor and shows the API’s error', async () => {
  const user = userEvent.setup()
  renderApp('/faults/create')
  await screen.findByRole('heading', { name: 'Add fault rule' })
  await user.click(screen.getByRole('tab', { name: 'JSON' }))
  const json = screen.getByLabelText('Request body')
  expect(JSON.parse((json as HTMLTextAreaElement).value)).toEqual({ code: 'UNAVAILABLE' })

  await user.clear(json)
  await user.paste('{"service": "pubsub"}')
  await user.click(screen.getByRole('button', { name: 'Add rule' }))
  expect(await screen.findByText('rule needs code, latency or drop')).toBeInTheDocument()

  await user.clear(json)
  await user.paste('{"service": "pubsub", "latency": "500ms", "probability": 0.5}')
  await user.click(screen.getByRole('tab', { name: 'Form' }))
  expect(screen.getByLabelText('Latency')).toHaveValue('500ms')
  expect(screen.getByLabelText('Probability')).toHaveValue('0.5')
  expect(screen.getByLabelText('Service')).toHaveValue('pubsub')
  await user.click(screen.getByRole('tab', { name: 'JSON' }))
  await user.click(screen.getByRole('button', { name: 'Add rule' }))
  expect(await screen.findByText('Added rule fault-4.')).toBeInTheDocument()
  expect(fake.faults.at(-1)).toMatchObject({
    service: 'pubsub',
    latency: '500ms',
    probability: 0.5,
  })
})

it('describes rules', () => {
  const r: FaultRule = { code: 'ABORTED', httpStatus: 418, latency: '1s', count: 1 }
  expect(effect(r)).toBe('Delay 1s, then ABORTED (HTTP 418)')
  expect(when(r)).toBe('Every match, at most 1 time')
  expect(when({ probability: 0.125, count: 2 })).toBe('12.5% of matches, at most 2 times')
  expect(effect({ drop: true, code: 'INTERNAL' })).toBe('Drop the connection')
  expect(matches({ id: 'a', resource: '/b/Photos' }, 'photos')).toBe(true)
  expect(matches({ id: 'a' }, 'z')).toBe(false)
})

it('round-trips a rule between the form and the JSON body', () => {
  const v: Values = {
    id: 'x',
    service: 'gcs',
    method: '',
    resource: 'bucket-a',
    code: '',
    httpStatus: '',
    latency: '0s',
    drop: true,
    probability: '0.5',
    count: '',
    disabled: false,
  }
  const body = ruleBody(v, { extra: 1 })
  expect(body).toEqual({
    extra: 1,
    id: 'x',
    service: 'gcs',
    resource: 'bucket-a',
    drop: true,
    probability: 0.5,
  })
  expect(ruleValues(body)).toEqual({ ...v, latency: '' })
})
