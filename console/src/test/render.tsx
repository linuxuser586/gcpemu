import { QueryClientProvider } from '@tanstack/react-query'
import { render } from '@testing-library/react'
import { createMemoryRouter, RouterProvider } from 'react-router'

import { createQueryClient } from '@/api/queryClient'
import { routes } from '@/router'

/** renderApp renders the whole console at a path under /console. */
export function renderApp(path = '/') {
  const queryClient = createQueryClient()
  // Tests assert on what a failure shows, not on how often it is retried.
  queryClient.setDefaultOptions({
    queries: { ...queryClient.getDefaultOptions().queries, retry: false },
  })
  const router = createMemoryRouter(routes(queryClient), { initialEntries: [path] })
  const utils = render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  )
  return { ...utils, router, queryClient }
}
