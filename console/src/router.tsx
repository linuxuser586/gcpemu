import type { QueryClient } from '@tanstack/react-query'
import { createBrowserRouter, type RouteObject } from 'react-router'

import {
  containersQuery,
  endpointsQuery,
  envQuery,
  infoQuery,
  readinessQuery,
  resourceCountsQuery,
} from '@/api/queries'
import { AppShell } from '@/components/shell/AppShell'
import { RouteError } from '@/components/shell/RouteError'
import { Dashboard } from '@/features/dashboard/Dashboard'
import { ServicePage } from '@/features/services/ServicePage'

export const BASENAME = '/console'

// Loaders only warm the query cache with ensureQueryData; components read
// it with useQuery. Only /_emu/v1/info is required to render: a failure of
// any other query is shown inline by the card that uses it.
export function routes(queryClient: QueryClient): RouteObject[] {
  return [
    {
      path: '/',
      element: <AppShell />,
      errorElement: <RouteError />,
      hydrateFallbackElement: <p className="p-6 text-sm text-muted-foreground">Loading…</p>,
      loader: async () => {
        await queryClient.ensureQueryData(infoQuery())
        return null
      },
      children: [
        {
          index: true,
          element: <Dashboard />,
          loader: async () => {
            await Promise.allSettled([
              queryClient.ensureQueryData(readinessQuery()),
              queryClient.ensureQueryData(endpointsQuery()),
              queryClient.ensureQueryData(containersQuery()),
              queryClient.ensureQueryData(resourceCountsQuery()),
              queryClient.ensureQueryData(envQuery()),
            ])
            return null
          },
        },
        {
          path: ':service/*',
          element: <ServicePage />,
          loader: async () => {
            await Promise.allSettled([queryClient.ensureQueryData(resourceCountsQuery())])
            return null
          },
        },
      ],
    },
  ]
}

export function createRouter(queryClient: QueryClient) {
  return createBrowserRouter(routes(queryClient), { basename: BASENAME })
}
