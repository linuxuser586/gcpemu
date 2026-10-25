import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Trash2 } from 'lucide-react'

import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { DetailList } from '@/components/resource/DetailList'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import { toast } from '@/lib/toast'

import { formatBytes } from '../gcs/format'
import { daemonQuery, pruneDaemon } from './api'
import { BuildHeader } from './BuildLayout'

/**
 * Daemon is the Build daemon (ADR 0016) every Build of the Instance runs
 * on: whether it runs, its image and layer cache's size, and pruning that
 * cache.
 */
export function Daemon() {
  const query = useQuery(daemonQuery())
  const d = query.data
  const qc = useQueryClient()
  const prune = useMutation({
    mutationFn: pruneDaemon,
    onSuccess: (res) => {
      qc.setQueryData(daemonQuery().queryKey, (old) =>
        old ? { ...old, cacheBytes: res.cacheBytes } : old,
      )
      void qc.invalidateQueries({ queryKey: daemonQuery().queryKey })
      toast.success(`Pruned the Build daemon: freed ${formatBytes(res.reclaimedBytes)}.`)
    },
  })

  return (
    <div className="flex flex-col gap-6">
      <BuildHeader>
        <p className="text-sm text-muted-foreground">Shared by every Build of this Instance</p>
      </BuildHeader>
      <Card>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <CardTitle>Build daemon</CardTitle>
          <ConfirmDialog
            trigger={
              <Button variant="outline">
                <Trash2 aria-hidden />
                Prune cache
              </Button>
            }
            title="Prune the Build daemon’s cache?"
            description="Its build history, stopped containers, unused images and volumes, and build cache are removed. The next Build of each image pulls and builds its layers again."
            confirmLabel="Prune cache"
            onConfirm={() => prune.mutateAsync()}
          />
        </div>
        <QueryStatus query={query} />
        {d && (
          <DetailList
            label="Build daemon details"
            rows={[
              ['State', d.running ? 'Running' : 'Not running: it starts with the next Build'],
              ['Cache size', formatBytes(d.cacheBytes)],
              [
                'Problem',
                d.privilegedRefused
                  ? 'The container runtime refused the Build daemon’s privileged container, so every Build fails until it allows privileged containers.'
                  : d.error,
              ],
            ]}
          />
        )}
      </Card>
    </div>
  )
}
