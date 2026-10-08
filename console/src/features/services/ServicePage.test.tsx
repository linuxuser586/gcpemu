import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { expect, it } from 'vitest'

import { renderApp } from '@/test/render'

it('explains how to enable a disabled Service a deep link points at', async () => {
  renderApp('/sql/instances?project=alpha-project')
  expect(
    await screen.findByRole('heading', { name: 'Service sql is not enabled on this Instance' }),
  ).toBeInTheDocument()
  expect(screen.getByText('gcpemu start --services iam,gcs,pubsub,sql')).toBeInTheDocument()
})

it('lists only enabled Services in the sidebar, carrying the view state', async () => {
  const user = userEvent.setup()
  const { router } = renderApp('/?project=alpha-project&location=us-east1&q=logs&other=1')
  const nav = await screen.findByRole('navigation', { name: 'Services' })
  const links = within(nav)
    .getAllByRole('link')
    .map((a) => a.textContent)
  expect(links).toEqual([
    'Dashboard',
    'Operations',
    'IAM & Resource Manager',
    'Pub/Sub',
    'Cloud Storage',
  ])
  await user.click(within(nav).getByRole('link', { name: 'Cloud Storage' }))
  expect(router.state.location.pathname).toBe('/gcs')
  expect(router.state.location.search).toBe('?project=alpha-project&location=us-east1&q=logs')
  expect(await screen.findByRole('heading', { name: 'Cloud Storage' })).toBeInTheDocument()
})

it('says when no Service has that ID', async () => {
  renderApp('/nope')
  expect(await screen.findByText('Page not found')).toBeInTheDocument()
})
