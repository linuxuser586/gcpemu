import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, it, vi } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'

import { snapshotBody, snapshotValues } from './SaveSnapshot'

const rows = () => screen.getAllByTestId('snapshot')

afterEach(() => vi.restoreAllMocks())

it('lists the State snapshots with their time, size and Services', async () => {
  renderApp('/state')
  expect(await screen.findByRole('heading', { name: 'State' })).toBeInTheDocument()
  await screen.findByRole('table', { name: 'State snapshots' })
  expect(rows().map((r) => r.dataset.name)).toEqual(['clean'])
  const clean = rows()[0]!
  expect(within(clean).getByText('1.5 KiB')).toBeInTheDocument()
  expect(
    within(clean).getByText('Cloud Storage, IAM & Resource Manager, Pub/Sub'),
  ).toBeInTheDocument()
  expect(
    within(clean).getByText(new Date('2026-10-08T11:00:00Z').toLocaleString()),
  ).toBeInTheDocument()
})

it('is reached from the sidebar', async () => {
  const user = userEvent.setup()
  renderApp('/?project=alpha-project')
  await user.click(
    within(await screen.findByRole('navigation', { name: 'Services' })).getByRole('link', {
      name: 'State',
    }),
  )
  expect(await screen.findByRole('heading', { name: 'State' })).toBeInTheDocument()
})

it('says when no snapshot is saved', async () => {
  fake.snapshots = []
  renderApp('/state')
  expect(await screen.findByText('No State snapshots saved.')).toBeInTheDocument()
})

it('restores a snapshot after confirming', async () => {
  const user = userEvent.setup()
  renderApp('/state')
  await screen.findByRole('table', { name: 'State snapshots' })
  await user.click(screen.getByRole('button', { name: 'Restore clean' }))
  expect(
    await screen.findByRole('dialog', { name: 'Restore State snapshot clean?' }),
  ).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Restore' }))
  expect(await screen.findByText('Restored State snapshot clean.')).toBeInTheDocument()
})

it('deletes a snapshot after confirming', async () => {
  const user = userEvent.setup()
  renderApp('/state')
  await screen.findByRole('table', { name: 'State snapshots' })
  await user.click(screen.getByRole('button', { name: 'Delete clean' }))
  await user.click(await screen.findByRole('button', { name: 'Delete' }))
  expect(await screen.findByText('Deleted State snapshot clean.')).toBeInTheDocument()
  expect(await screen.findByText('No State snapshots saved.')).toBeInTheDocument()
  expect(fake.snapshots).toEqual([])
})

it('saves a snapshot through the form', async () => {
  const user = userEvent.setup()
  renderApp('/state')
  await user.click(await screen.findByRole('link', { name: 'Save snapshot' }))
  expect(await screen.findByRole('heading', { name: 'Save State snapshot' })).toBeInTheDocument()
  const name = screen.getByRole('textbox', { name: 'Name' })

  await user.type(name, '../escape')
  await user.click(screen.getByRole('button', { name: 'Save snapshot' }))
  expect(await screen.findByText(/Use up to 63 letters/)).toBeInTheDocument()

  // A taken name replaces that snapshot, and the form says so.
  await user.clear(name)
  await user.type(name, 'clean')
  expect(await screen.findByText('Replaces the State snapshot clean.')).toBeInTheDocument()
  expect(screen.getByRole('button', { name: 'Replace snapshot' })).toBeInTheDocument()

  await user.clear(name)
  await user.type(name, 'after-seed')
  await user.click(screen.getByRole('button', { name: 'Save snapshot' }))
  expect(await screen.findByText('Saved State snapshot after-seed.')).toBeInTheDocument()
  await screen.findByRole('table', { name: 'State snapshots' })
  await waitFor(() => expect(rows().map((r) => r.dataset.name)).toEqual(['after-seed', 'clean']))
})

it('shows why the Instance cannot be snapshotted', async () => {
  const user = userEvent.setup()
  renderApp('/state/snapshots/save')
  await user.type(await screen.findByRole('textbox', { name: 'Name' }), 'busy')
  await user.click(screen.getByRole('button', { name: 'Save snapshot' }))
  expect(await screen.findByText(/Operation operation-1 of sql is running/)).toBeInTheDocument()
  expect(screen.getByRole('heading', { name: 'Save State snapshot' })).toBeInTheDocument()
})

it('reads and writes the request body', () => {
  expect(snapshotBody({ name: ' nightly ' }, { extra: 1 })).toEqual({ extra: 1, name: 'nightly' })
  expect(snapshotValues({ name: 'nightly' })).toEqual({ name: 'nightly' })
  expect(snapshotValues({})).toEqual({ name: '' })
})

it('resets the Instance after confirming', async () => {
  const user = userEvent.setup()
  renderApp('/state')
  await user.click(await screen.findByRole('button', { name: 'Reset Instance' }))
  expect(await screen.findByRole('dialog', { name: 'Reset the Instance?' })).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Reset' }))
  expect(await screen.findByText('Reset the Instance.')).toBeInTheDocument()
  expect(fake.resets).toBe(1)
})

it('applies a seed file chosen or pasted, showing the API’s error', async () => {
  const user = userEvent.setup()
  renderApp('/state')
  const form = await screen.findByRole('form', { name: 'Import seed file' })
  const apply = within(form).getByRole('button', { name: 'Apply seed file' })
  expect(apply).toBeDisabled()

  const file = new File(['projects: [from-file]\n'], 'seed.yaml', { type: 'application/yaml' })
  await user.upload(within(form).getByLabelText('Seed file'), file)
  const yaml = within(form).getByRole('textbox', { name: 'YAML' })
  await waitFor(() => expect(yaml).toHaveValue('projects: [from-file]\n'))
  await user.click(apply)
  expect(await screen.findByText('Applied the seed file.')).toBeInTheDocument()
  expect(fake.seeds).toEqual(['projects: [from-file]\n'])

  await user.clear(yaml)
  await user.type(yaml, 'nope: {{}}')
  await user.click(apply)
  expect(await within(form).findByRole('alert')).toHaveTextContent(
    'seed section "nope": service not running',
  )
  expect(fake.seeds).toHaveLength(1)
})

it('exports the seed file as a download', async () => {
  const user = userEvent.setup()
  const blobs: Blob[] = []
  URL.createObjectURL = vi.fn((b: Blob) => {
    blobs.push(b)
    return 'blob:seed'
  })
  URL.revokeObjectURL = vi.fn()
  const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (
    this: HTMLAnchorElement,
  ) {
    expect(this.download).toBe('seed.yaml')
    expect(this.href).toBe('blob:seed')
  })
  renderApp('/state')
  await user.click(await screen.findByRole('button', { name: 'Export seed file' }))
  await waitFor(() => expect(click).toHaveBeenCalledOnce())
  expect(await blobs[0]!.text()).toBe('projects:\n  - alpha-project\n')
})
