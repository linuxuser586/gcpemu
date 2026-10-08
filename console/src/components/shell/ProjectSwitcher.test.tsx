import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { expect, it } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

it('lists Projects and puts the chosen one in the URL', async () => {
  const user = userEvent.setup()
  const { router } = renderApp('/')
  await user.click(await screen.findByRole('button', { name: 'Project' }))
  const list = await screen.findByRole('list', { name: 'Projects' })
  await user.click(within(list).getByRole('button', { name: 'beta-project' }))
  expect(router.state.location.search).toBe('?project=beta-project')
  expect(screen.getByRole('button', { name: 'Project' })).toHaveTextContent('beta-project')
})

it('accepts an unknown Project ID as "not yet created" and creates nothing', async () => {
  const user = userEvent.setup()
  const writes: string[] = []
  server.events.on('request:start', ({ request }) => {
    if (request.method !== 'GET') writes.push(`${request.method} ${request.url}`)
  })
  const { router } = renderApp('/')
  await user.click(await screen.findByRole('button', { name: 'Project' }))
  await screen.findByRole('list', { name: 'Projects' })
  await user.type(screen.getByLabelText('Project ID'), 'new-project')
  expect(screen.getByText(/is not yet created/)).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Open' }))
  expect(router.state.location.search).toBe('?project=new-project')
  expect(await screen.findByText('not yet created')).toBeInTheDocument()
  expect(writes).toEqual([])
})

it('rejects an invalid Project ID', async () => {
  const user = userEvent.setup()
  const { router } = renderApp('/')
  await user.click(await screen.findByRole('button', { name: 'Project' }))
  await user.type(screen.getByLabelText('Project ID'), 'Bad_ID')
  await user.click(screen.getByRole('button', { name: 'Open' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('A Project ID is 6 to 30')
  expect(router.state.location.search).toBe('')
})

it('rejects an unknown Project under Strict projects, pointing at the Seed file', async () => {
  fake.info.strictProjects = true
  const user = userEvent.setup()
  const { router } = renderApp('/')
  await user.click(await screen.findByRole('button', { name: 'Project' }))
  await screen.findByRole('list', { name: 'Projects' })
  await user.type(screen.getByLabelText('Project ID'), 'new-project')
  await user.click(screen.getByRole('button', { name: 'Open' }))
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Project new-project does not exist, and this Instance runs with Strict projects. Declare it under "projects:" in the Seed file',
  )
  expect(router.state.location.search).toBe('')
})

it('keeps the Project of a deep link', async () => {
  renderApp('/?project=alpha-project')
  expect(await screen.findByRole('button', { name: 'Project' })).toHaveTextContent('alpha-project')
})

it('says why Projects cannot be listed without the iam Service', async () => {
  fake.info.services = ['gcs']
  server.use(
    http.get('*/cloudresourcemanager/v3/projects\\:search', () =>
      HttpResponse.json(
        { error: { code: 404, message: 'not found', status: 'NOT_FOUND' } },
        { status: 404 },
      ),
    ),
  )
  const user = userEvent.setup()
  renderApp('/')
  await user.click(await screen.findByRole('button', { name: 'Project' }))
  expect(await screen.findByText(/Resource Manager is part of the iam Service/)).toBeInTheDocument()
})
