import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useId } from 'react'

import { QueryStatus } from '@/components/QueryStatus'
import { Card, CardTitle } from '@/components/ui/card'
import { Switch } from '@/components/ui/switch'
import { toast } from '@/lib/toast'

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
          <Switch
            checked={on}
            aria-labelledby={`${id}-title`}
            aria-describedby={`${id}-hint`}
            disabled={change.isPending}
            onClick={() => change.mutate(!s.enabled)}
          />
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
