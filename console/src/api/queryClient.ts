import { MutationCache, QueryClient } from '@tanstack/react-query'

import { toast } from '@/lib/toast'

import { errorMessage, shouldRetry } from './errors'

declare module '@tanstack/react-query' {
  interface Register {
    mutationMeta: {
      /** false when the caller shows the error itself, e.g. as field errors. */
      toast?: boolean
    }
  }
}

/**
 * createQueryClient: queries retry only server and network failures and
 * show their errors inline; failed mutations raise a toast with the API's
 * message unless they opt out.
 */
export function createQueryClient() {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: shouldRetry, staleTime: 1000 },
      mutations: { retry: false },
    },
    mutationCache: new MutationCache({
      onError(error, _vars, _ctx, mutation) {
        if (mutation.meta?.toast !== false) toast.error(errorMessage(error))
      },
    }),
  })
}
