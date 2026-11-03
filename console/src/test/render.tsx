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
  // A navigation lands later than whatever started it. The delay makes a test
  // that reads too early fail every run, not once in a while on a slow runner.
  const navigate = router.navigate.bind(router)
  router.navigate = ((...args: Parameters<typeof navigate>) =>
    new Promise((resolve) => setTimeout(() => resolve(navigate(...args)), 30))) as typeof navigate
  const utils = render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  )
  return { ...utils, router, queryClient }
}
