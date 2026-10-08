import type { UseQueryResult } from '@tanstack/react-query'

import { errorMessage } from '@/api/errors'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'

/**
 * QueryStatus shows a query's loading or error state inline, or nothing
 * once it has data. Queries already retried server and network failures.
 */
export function QueryStatus({ query, rows = 3 }: { query: UseQueryResult; rows?: number }) {
  if (query.isPending) {
    return (
      <div className="flex flex-col gap-2" aria-busy="true" aria-label="Loading">
        {Array.from({ length: rows }, (_, i) => (
          <Skeleton key={i} className="h-5" />
        ))}
      </div>
    )
  }
  if (query.isError) {
    return (
      <div role="alert" className="flex items-start justify-between gap-2 text-sm text-destructive">
        <p>{errorMessage(query.error)}</p>
        <Button variant="outline" size="sm" onClick={() => void query.refetch()}>
          Retry
        </Button>
      </div>
    )
  }
  return null
}
