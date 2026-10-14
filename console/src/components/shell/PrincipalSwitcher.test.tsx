import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { expect, it } from 'vitest'

import { usePrefs } from '@/stores/prefs'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import { toPrincipal } from './PrincipalSwitcher'

const SA = 'reader@alpha-project.iam.gserviceaccount.com'

/** bucketsAs answers the bucket list, denying it to every token but none. */
function bucketsAs(auth: (string | null)[]) {
  server.use(
    http.get('*/iam/v1/projects/:project/serviceAccounts', () =>
      HttpResponse.json({ accounts: [{ email: SA }] }),
    ),
    http.get('*/storage/v1/b', ({ request }) => {
      const a = request.headers.get('Authorization')
      auth.push(a)
      if (a)
        return HttpResponse.json(
          {
            error: {
              code: 403,
              status: 'PERMISSION_DENIED',
              message: `Permission 'storage.buckets.list' denied on resource 'projects/alpha-project'.`,
            },
          },
          { status: 403 },
        )
      return HttpResponse.json({ items: [{ name: 'visible-bucket' }] })
    }),
  )
}

it('reads a typed Principal', () => {
  expect(toPrincipal('alice@example.com')).toBe('user:alice@example.com')
  expect(toPrincipal(SA)).toBe(`serviceAccount:${SA}`)
  expect(toPrincipal(` user:bob@example.com `)).toBe('user:bob@example.com')
})

it('acts as a chosen service account, showing what IAM denies it', async () => {
  const auth: (string | null)[] = []
  bucketsAs(auth)
  const user = userEvent.setup()
  const { router } = renderApp('/gcs?project=alpha-project')
  expect(await screen.findByText('visible-bucket')).toBeInTheDocument()
  expect(auth.at(-1)).toBeNull()

  await user.click(screen.getByRole('button', { name: 'Acting as' }))
  const list = await screen.findByRole('list', { name: 'Service accounts' })
  await user.click(within(list).getByRole('button', { name: SA }))

  expect(await screen.findByText(/storage\.buckets\.list' denied/)).toBeInTheDocument()
  expect(screen.queryByText('visible-bucket')).not.toBeInTheDocument()
  expect(auth.at(-1)).toBe(`Bearer tok-serviceAccount:${SA}`)
  expect(screen.getByRole('button', { name: 'Acting as' })).toHaveTextContent(SA)
  // A per-browser preference, not part of a shared link.
  expect(router.state.location.search).toBe('?project=alpha-project')
  expect(localStorage.getItem('gcpemu-console-prefs')).toContain(`serviceAccount:${SA}`)

  await user.click(screen.getByRole('button', { name: 'Acting as' }))
  const principals = await screen.findByRole('list', { name: 'Principals' })
  await user.click(within(principals).getByRole('button', { name: /Default principal/ }))
  expect(await screen.findByText('visible-bucket')).toBeInTheDocument()
  expect(auth.at(-1)).toBeNull()
})

it('takes a typed email as a user', async () => {
  const user = userEvent.setup()
  renderApp('/')
  await user.click(await screen.findByRole('button', { name: 'Acting as' }))
  await user.type(screen.getByLabelText('Act as'), 'alice@example.com')
  await user.click(screen.getByRole('button', { name: 'Switch' }))
  expect(usePrefs.getState().principal).toBe('user:alice@example.com')
  expect(screen.getByRole('button', { name: 'Acting as' })).toHaveTextContent('alice@example.com')
})

it('rejects what is not a Principal', async () => {
  const user = userEvent.setup()
  renderApp('/')
  await user.click(await screen.findByRole('button', { name: 'Acting as' }))
  await user.type(screen.getByLabelText('Act as'), 'group:devs@example.com')
  await user.click(screen.getByRole('button', { name: 'Switch' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('Enter an email')
  expect(usePrefs.getState().principal).toBe('')
})

it('mints a fresh token when the gateway refuses the cached one', async () => {
  usePrefs.setState({ principal: 'user:alice@example.com' })
  let mints = 0
  const auth: (string | null)[] = []
  server.use(
    http.post('*/oauth2/token', () =>
      HttpResponse.json({ access_token: `tok-${++mints}`, expires_in: 3600 }),
    ),
    http.get('*/storage/v1/b', ({ request }) => {
      const a = request.headers.get('Authorization')
      auth.push(a)
      if (a === 'Bearer tok-1')
        return HttpResponse.json(
          { error: { code: 401, status: 'UNAUTHENTICATED', message: 'expired' } },
          { status: 401 },
        )
      return HttpResponse.json({ items: [{ name: 'visible-bucket' }] })
    }),
  )
  renderApp('/gcs?project=alpha-project')
  expect(await screen.findByText('visible-bucket')).toBeInTheDocument()
  expect(auth).toEqual(['Bearer tok-1', 'Bearer tok-2'])
})
