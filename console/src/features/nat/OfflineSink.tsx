import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useId } from 'react'

import { QueryStatus } from '@/components/QueryStatus'
import { Card, CardTitle } from '@/components/ui/card'
import { toast } from '@/lib/toast'
import { cn } from '@/lib/utils'

import { setSink, sinkQuery } from './api'

/**
 * OfflineSink shows and toggles Cloud NAT's offline sink (FR-NAT-005)
 * through the admin API: while it is on, TCP egress a NAT allows is
 * answered by the sink instead of reaching the internet. It is the
 * Instance's, for every Project, and lasts until the Instance stops.
 */
export function OfflineSink() {
  const id = useId()
  const qc = useQueryClient()
  const query = useQuery(sinkQuery())
  const change = useMutation({
    mutationFn: setSink,
    onSuccess: (s) => {
      qc.setQueryData(sinkQuery().queryKey, s)
      toast.success(
        s.enabled
          ? 'Offline sink on: NAT egress is answered by the sink.'
          : 'Offline sink off: NAT egress reaches the internet.',
      )
    },
  })
  const s = query.data
  const on = change.isPending ? change.variables : !!s?.enabled
  return (
    <Card aria-labelledby={`${id}-title`}>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <CardTitle id={`${id}-title`}>Offline sink</CardTitle>
        {s && (
          <button
            type="button"
            role="switch"
            aria-checked={on}
            aria-labelledby={`${id}-title`}
            aria-describedby={`${id}-hint`}
            disabled={change.isPending}
            onClick={() => change.mutate(!s.enabled)}
            className={cn(
              'relative inline-flex h-6 w-11 shrink-0 items-center rounded-full border-2 border-transparent transition-colors outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50 disabled:opacity-50',
              on ? 'bg-primary' : 'bg-input',
            )}
          >
            <span
              aria-hidden
              className={cn(
                'inline-block size-5 rounded-full bg-background shadow-sm transition-transform',
                on ? 'translate-x-5' : 'translate-x-0',
              )}
            />
          </button>
        )}
      </div>
      <QueryStatus query={query} />
      {s && (
        <p id={`${id}-hint`} className="text-sm text-muted-foreground">
          {s.enabled ? (
            <>
              <strong className="font-medium text-foreground">On.</strong> TCP egress a Cloud NAT
              allows is recorded and answered with{' '}
              <span className="font-mono">
                {s.status} {s.body}
              </span>{' '}
              instead of reaching the internet; it shows as OFFLINE_SINK in the translation log.
            </>
          ) : (
            <>
              <strong className="font-medium text-foreground">Off.</strong> Egress a Cloud NAT
              allows reaches the internet. Turn the sink on to keep tests offline.
            </>
          )}{' '}
          It applies to every Project of this Instance until it stops; <code>--offline</code> turns
          it on at start.
        </p>
      )}
    </Card>
  )
}
