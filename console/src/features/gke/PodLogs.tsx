import { useQuery } from '@tanstack/react-query'
import { useEffect, useRef } from 'react'
import { useParams, useSearchParams } from 'react-router'

import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Card } from '@/components/ui/card'
import { NativeSelect } from '@/components/ui/native-select'

import { kubeQuery, podLogQuery, type KubePod } from './api'
import { podStatus, StatusBadge } from './format'
import { clusterPath, useClusterRef } from './GkeLayout'

export const TAIL_CHOICES = [100, 500, 2000] as const

/**
 * PodLogs shows a pod's log through the Connect gateway: the last
 * ?tail= lines of ?container= (or the previous instance's with
 * ?previous=1), refreshed while following, which is the default.
 */
export function PodLogs() {
  const ref = useClusterRef()
  const { namespace = '', pod = '' } = useParams()
  const [search, setSearch] = useSearchParams()
  const podQuery = useQuery(kubeQuery<KubePod>(ref, `/api/v1/namespaces/${namespace}/pods/${pod}`))
  const containers = podQuery.data?.spec?.containers.map((c) => c.name) ?? []
  const container = search.get('container') ?? containers[0] ?? ''
  const tail = Number(search.get('tail')) || TAIL_CHOICES[0]
  const previous = search.get('previous') === '1'
  const follow = search.get('follow') !== '0' && !previous
  const log = useQuery({
    ...podLogQuery(ref, namespace, pod, { container, tailLines: tail, previous }, follow),
    enabled: !!container,
  })
  const pre = useRef<HTMLPreElement>(null)
  useEffect(() => {
    if (follow && pre.current) pre.current.scrollTop = pre.current.scrollHeight
  }, [log.data, follow])

  const set = (k: string, v: string) =>
    setSearch((prev) => {
      const next = new URLSearchParams(prev)
      if (v) next.set(k, v)
      else next.delete(k)
      return next
    })

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center gap-3">
        <h2 className="text-lg font-semibold">
          Logs of pod <span className="font-mono">{pod}</span>
        </h2>
        {podQuery.data && <StatusBadge {...podStatus(podQuery.data)} />}
        <Link
          to={`${clusterPath(ref.location, ref.cluster)}/pods?namespace=${encodeURIComponent(namespace)}`}
          className="text-sm text-primary underline-offset-4 hover:underline"
        >
          All pods in <span className="font-mono">{namespace}</span>
        </Link>
      </div>
      <QueryStatus query={podQuery} rows={1} />
      <Card>
        <form
          aria-label="Log options"
          className="flex flex-wrap items-end gap-4"
          onSubmit={(e) => e.preventDefault()}
        >
          <label htmlFor="log-container" className="flex flex-col gap-1 text-sm">
            Container
            <NativeSelect
              id="log-container"
              value={container}
              onChange={(e) => set('container', e.target.value)}
            >
              {containers.map((c) => (
                <option key={c} value={c}>
                  {c}
                </option>
              ))}
            </NativeSelect>
          </label>
          <label htmlFor="log-tail" className="flex flex-col gap-1 text-sm">
            Lines
            <NativeSelect id="log-tail" value={tail} onChange={(e) => set('tail', e.target.value)}>
              {TAIL_CHOICES.map((n) => (
                <option key={n} value={n}>
                  Last {n}
                </option>
              ))}
            </NativeSelect>
          </label>
          <label className="flex h-9 items-center gap-2 text-sm">
            <input
              type="checkbox"
              className="size-4"
              checked={previous}
              onChange={(e) => set('previous', e.target.checked ? '1' : '')}
            />
            Previous instance
          </label>
          <label className="flex h-9 items-center gap-2 text-sm">
            <input
              type="checkbox"
              className="size-4"
              checked={follow}
              disabled={previous}
              onChange={(e) => set('follow', e.target.checked ? '' : '0')}
            />
            Follow
          </label>
        </form>
        <QueryStatus query={log} rows={6} />
        {log.data !== undefined && (
          <pre
            ref={pre}
            aria-label="Log"
            // Focusable so that the log scrolls from the keyboard.
            // eslint-disable-next-line jsx-a11y/no-noninteractive-tabindex
            tabIndex={0}
            className="max-h-[36rem] min-h-48 overflow-auto rounded-md bg-muted p-3 font-mono text-xs whitespace-pre-wrap"
          >
            {log.data || 'The log is empty.'}
          </pre>
        )}
      </Card>
    </div>
  )
}
