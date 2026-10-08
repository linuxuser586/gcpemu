import { useQueryClient } from '@tanstack/react-query'
import { isRouteErrorResponse, useRevalidator, useRouteError } from 'react-router'

import { errorMessage } from '@/api/errors'
import { Button } from '@/components/ui/button'

/** RouteError is shown when a route's loader fails, e.g. the Instance stopped. */
export function RouteError() {
  const error = useRouteError()
  const revalidator = useRevalidator()
  const queryClient = useQueryClient()
  const message = isRouteErrorResponse(error)
    ? `${error.status} ${error.statusText}`
    : errorMessage(error)
  return (
    <div role="alert" className="mx-auto mt-24 flex max-w-lg flex-col gap-4 p-6">
      <h1 className="text-xl font-semibold">The console can&apos;t load this page</h1>
      <p className="text-sm text-muted-foreground">{message}</p>
      <div>
        <Button
          onClick={() => {
            queryClient.clear()
            void revalidator.revalidate()
          }}
        >
          Try again
        </Button>
      </div>
    </div>
  )
}
