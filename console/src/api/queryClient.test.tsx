import { QueryClientProvider, useMutation } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { expect, it } from 'vitest'

import { Toaster } from '@/components/shell/Toaster'
import { server } from '@/test/setup'

import { gcpFetch } from './fetch'
import { createQueryClient } from './queryClient'

function CreateBucket() {
  const m = useMutation({
    mutationFn: () =>
      gcpFetch('/storage/v1/b?project=alpha-project', {
        method: 'POST',
        body: JSON.stringify({ name: 'taken' }),
      }),
  })
  return <button onClick={() => m.mutate()}>Create</button>
}

it('toasts a failed mutation with the API’s message', async () => {
  server.use(
    http.post('*/storage/v1/b', () =>
      HttpResponse.json(
        {
          error: {
            code: 409,
            message: 'The bucket name is already taken.',
            status: 'ALREADY_EXISTS',
          },
        },
        { status: 409 },
      ),
    ),
  )
  const user = userEvent.setup()
  render(
    <QueryClientProvider client={createQueryClient()}>
      <CreateBucket />
      <Toaster />
    </QueryClientProvider>,
  )
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('The bucket name is already taken.')
})
