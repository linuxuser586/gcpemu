import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { expect, it } from 'vitest'

import { fake, linkedFake } from '@/test/handlers'
import { renderApp } from '@/test/render'

const link = (name: string) => screen.findByRole('region', { name: `Link ${name}` })

/** detail returns the value DetailList shows for label within el. */
const detail = (el: HTMLElement, label: string) =>
  within(el).getByText(label, { selector: 'dt' }).nextElementSibling as HTMLElement

it('is reached from the sidebar and lists each link with its state', async () => {
  linkedFake()
  const user = userEvent.setup()
  renderApp('/')
  await user.click(
    within(await screen.findByRole('navigation', { name: 'Services' })).getByRole('link', {
      name: 'Links',
    }),
  )
  expect(await screen.findByRole('heading', { name: 'Links', level: 1 })).toBeInTheDocument()
  expect(screen.getByText('sha256:LAPTOP')).toBeInTheDocument()
  const office = await link('office')
  expect(within(office).getByText('192.168.1.20:4511')).toBeInTheDocument()
  expect(within(office).getByText('sha256:OFFICE')).toBeInTheDocument()
  expect(await within(office).findByText('reachable')).toBeInTheDocument()
  expect(within(office).getByText('paired')).toBeInTheDocument()
  expect(detail(office, 'Protocol version')).toHaveTextContent('1')
})

it('says when this Instance has no links', async () => {
  renderApp('/links')
  expect(await screen.findByText(/No links yet/)).toBeInTheDocument()
})

it('shows the Projects shared each way, and delegates one the link shares', async () => {
  linkedFake()
  const user = userEvent.setup()
  renderApp('/links')
  const office = await link('office')
  const byIt = await within(office).findByRole('list', { name: 'Shared by office' })
  expect(
    within(byIt)
      .getAllByRole('listitem')
      .map((li) => li.dataset.project),
  ).toEqual(['for-laptop', 'team-shared'])
  const withIt = within(office).getByRole('list', { name: 'Shared with office' })
  expect(within(withIt).getByText('laptop-own')).toBeInTheDocument()

  // team-shared is delegated already; for-laptop can be.
  expect(within(byIt).getByText('delegated')).toBeInTheDocument()
  await user.click(within(byIt).getByRole('button', { name: 'Delegate for-laptop' }))
  expect(await screen.findByText('Delegated for-laptop to office.')).toBeInTheDocument()
  expect(fake.delegated).toContainEqual({ project: 'for-laptop', link: 'office' })
})

it('undelegates a Project after confirming', async () => {
  linkedFake()
  const user = userEvent.setup()
  renderApp('/links')
  const delegated = await screen.findByRole('table', { name: 'Delegated Projects' })
  const row = within(delegated).getByRole('row', { name: /team-shared/ })
  expect(row).toHaveTextContent('office')
  await user.click(within(row).getByRole('button', { name: 'Undelegate team-shared' }))
  await user.click(await screen.findByRole('button', { name: 'Undelegate' }))
  expect(await screen.findByText('Stopped delegating team-shared.')).toBeInTheDocument()
  expect(fake.delegated).toEqual([])
})

it('shares a Project with every link or one, and stops sharing it', async () => {
  linkedFake()
  const user = userEvent.setup()
  renderApp('/links')
  const form = await screen.findByRole('form', { name: 'Share a Project' })
  await user.type(within(form).getByRole('textbox', { name: 'Project ID' }), 'Bad_ID')
  await user.click(within(form).getByRole('button', { name: 'Share' }))
  expect(await within(form).findByRole('alert')).toHaveTextContent('A Project ID is 6 to 30')

  await user.clear(within(form).getByRole('textbox', { name: 'Project ID' }))
  await user.type(within(form).getByRole('textbox', { name: 'Project ID' }), 'office-only')
  await user.selectOptions(within(form).getByRole('combobox', { name: 'With' }), 'office')
  await user.click(within(form).getByRole('button', { name: 'Share' }))
  expect(await screen.findByText('Shared office-only with office.')).toBeInTheDocument()
  expect(fake.shared).toContainEqual({ project: 'office-only', with: 'office' })

  const shares = await screen.findByRole('table', { name: 'Shared Projects' })
  await waitFor(() =>
    expect(within(shares).getByRole('row', { name: /office-only/ })).toHaveTextContent('office'),
  )
  expect(within(shares).getByRole('row', { name: /laptop-own/ })).toHaveTextContent('every link')
  await user.click(within(shares).getByRole('button', { name: 'Unshare laptop-own' }))
  await user.click(await screen.findByRole('button', { name: 'Unshare' }))
  expect(await screen.findByText('Stopped sharing laptop-own.')).toBeInTheDocument()
  expect(fake.shared).toEqual([{ project: 'office-only', with: 'office' }])
})

const request = (id: string, name: string) => ({
  id,
  name,
  instanceId: `${name}-id`,
  fingerprint: `sha256:${name.toUpperCase()}`,
  address: '192.168.1.30:51234',
  version: 1,
  received: '2026-10-08T12:00:00Z',
})

it('approves a Pairing request after comparing its fingerprint, naming the link', async () => {
  fake.pairingRequests = [request('req-1', 'laptop')]
  const user = userEvent.setup()
  renderApp('/links')
  const requests = await screen.findByRole('table', { name: 'Pairing requests' })
  const row = within(requests).getByRole('row', { name: /laptop/ })
  expect(row).toHaveTextContent('sha256:LAPTOP')
  await user.click(within(row).getByRole('button', { name: 'Approve laptop' }))
  const dialog = await screen.findByRole('dialog', {
    name: 'Approve the Pairing request from laptop?',
  })
  expect(dialog).toHaveTextContent('sha256:LAPTOP')
  await user.type(within(dialog).getByRole('textbox', { name: 'Link name' }), 'barrys-laptop')
  await user.click(within(dialog).getByRole('button', { name: 'Approve' }))
  expect(await screen.findByText('Paired with laptop as barrys-laptop.')).toBeInTheDocument()
  expect(await link('barrys-laptop')).toBeInTheDocument()
  expect(fake.pairingRequests).toEqual([])
})

it('rejects a Pairing request', async () => {
  fake.pairingRequests = [request('req-2', 'stranger')]
  const user = userEvent.setup()
  renderApp('/links')
  const requests = await screen.findByRole('table', { name: 'Pairing requests' })
  await user.click(within(requests).getByRole('button', { name: 'Reject stranger' }))
  await user.click(await screen.findByRole('button', { name: 'Reject' }))
  expect(await screen.findByText('Rejected the Pairing request from stranger.')).toBeInTheDocument()
  expect(await screen.findByText('No Pairing requests waiting.')).toBeInTheDocument()
  expect(fake.links).toEqual([])
})

const discovered = (name: string, address: string, more: object = {}) => ({
  name: `${name}@desk`,
  instance: name,
  instanceId: `${name}-id`,
  address,
  fingerprint: `sha256:${name.toUpperCase()}`,
  version: 1,
  minVersion: 1,
  source: 'mdns' as const,
  ...more,
})

it('pairs with a Discovered Instance, once it approves', async () => {
  fake.discovered = [
    discovered('office', '192.168.1.20:4511'),
    discovered('old', '192.168.1.21:4511', { link: 'old' }),
    discovered('future', '192.168.1.22:4511', {
      problem: 'it speaks link protocol version 3 only; this Instance speaks 1',
    }),
  ]
  const user = userEvent.setup()
  renderApp('/links')
  const table = await screen.findByRole('table', { name: 'Discovered Instances' })
  expect(within(table).getByRole('row', { name: /old@desk/ })).toHaveTextContent('paired as old')
  const future = within(table).getByRole('row', { name: /future@desk/ })
  expect(future).toHaveTextContent('version 3 only')
  expect(within(future).queryByRole('button', { name: /Pair/ })).not.toBeInTheDocument()

  await user.click(within(table).getByRole('button', { name: 'Pair office@desk' }))
  const dialog = await screen.findByRole('dialog', { name: 'Pair with office@desk?' })
  expect(dialog).toHaveTextContent('sha256:OFFICE')
  await user.click(within(dialog).getByRole('button', { name: 'Send Pairing request' }))
  expect(await screen.findByText('Paired with office.')).toBeInTheDocument()
  expect(fake.pairs).toEqual([{ address: '192.168.1.20:4511', fingerprint: 'sha256:OFFICE' }])
  expect(await link('office')).toBeInTheDocument()
})

it('pairs by address, and says why the other Instance refused', async () => {
  fake.discovered = [discovered('office', 'refusing.example:4511')]
  const user = userEvent.setup()
  renderApp('/links')
  const form = await screen.findByRole('form', { name: 'Pair by address' })
  await user.type(within(form).getByRole('textbox', { name: 'Link address' }), 'nobody:4511')
  await user.click(within(form).getByRole('button', { name: 'Identify' }))
  expect(await within(form).findByRole('alert')).toHaveTextContent('connection refused')

  await user.clear(within(form).getByRole('textbox', { name: 'Link address' }))
  await user.type(
    within(form).getByRole('textbox', { name: 'Link address' }),
    'refusing.example:4511',
  )
  await user.click(within(form).getByRole('button', { name: 'Identify' }))
  const dialog = await screen.findByRole('dialog', { name: 'Pair with office?' })
  await user.click(within(dialog).getByRole('button', { name: 'Send Pairing request' }))
  expect(await within(dialog).findByRole('alert')).toHaveTextContent(
    'Instance "office" rejected the Pairing request',
  )
  expect(fake.links).toEqual([])
})

it('lists the Delegated APIs with their External emulators', async () => {
  fake.external = [
    {
      api: 'firestore',
      address: '127.0.0.1:8080',
      hosts: ['firestore.googleapis.com'],
      reachable: true,
    },
    {
      api: 'spanner',
      address: '127.0.0.1:9010',
      hosts: ['spanner.googleapis.com'],
      reachable: false,
      error: 'dial tcp 127.0.0.1:9010: connection refused',
    },
  ]
  renderApp('/links')
  const table = await screen.findByRole('table', { name: 'Delegated APIs' })
  expect(within(table).getByRole('row', { name: /firestore/ })).toHaveTextContent('127.0.0.1:8080')
  expect(within(table).getByRole('row', { name: /firestore/ })).toHaveTextContent('reachable')
  expect(within(table).getByRole('row', { name: /spanner/ })).toHaveTextContent('unreachable')
})

it('shows an unreachable link, and the Projects delegated to it as unavailable', async () => {
  linkedFake(false)
  renderApp('/links')
  const office = await link('office')
  expect(await within(office).findByText('unreachable')).toBeInTheDocument()
  expect(within(office).getByRole('status')).toHaveTextContent('connection refused')
  expect(within(office).getByText('Known once office is reachable.')).toBeInTheDocument()
  const delegated = await screen.findByRole('table', { name: 'Delegated Projects' })
  expect(
    await within(within(delegated).getByRole('row', { name: /team-shared/ })).findByText(
      'unavailable',
    ),
  ).toBeInTheDocument()
})

it('unpairs a link after confirming', async () => {
  linkedFake()
  const user = userEvent.setup()
  renderApp('/links')
  const office = await link('office')
  await user.click(within(office).getByRole('button', { name: 'Unpair office' }))
  const dialog = await screen.findByRole('dialog', { name: 'Unpair office?' })
  expect(dialog).toHaveTextContent('team-shared')
  await user.click(within(dialog).getByRole('button', { name: 'Unpair' }))
  expect(await screen.findByText('Unpaired office.')).toBeInTheDocument()
  expect(await screen.findByText(/No links yet/)).toBeInTheDocument()
})

it('does not call a Delegated Project available before its link is dialled', async () => {
  linkedFake()
  fake.linkStatus = []
  renderApp('/links')
  const delegated = await screen.findByRole('table', { name: 'Delegated Projects' })
  const row = within(delegated).getByRole('row', { name: /team-shared/ })
  expect(await within(row).findByText('checking…')).toBeInTheDocument()
  expect(within(row).queryByText('available')).not.toBeInTheDocument()
})
