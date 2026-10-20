import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Play } from 'lucide-react'

import { useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { toast } from '@/lib/toast'
import { useViewState } from '@/lib/viewState'

import { time } from '../gcs/format'
import {
  buildRef,
  isUnimplemented,
  runTrigger,
  triggerKind,
  triggersQuery,
  type BuildTrigger,
} from './api'
import { BuildHeader, buildPath, ChooseProject } from './BuildLayout'
import { LocationFilter, useBuildLocation } from './LocationFilter'

/** triggerSource is what a Build trigger listens to or builds. */
function triggerSource(t: BuildTrigger): string {
  if (t.pubsubConfig?.topic) return t.pubsubConfig.topic
  if (t.github) return `github.com/${t.github.owner ?? ''}/${t.github.name ?? ''}`
  if (t.repositoryEventConfig?.repository) return t.repositoryEventConfig.repository
  if (t.triggerTemplate?.repoName) return t.triggerTemplate.repoName
  if (t.sourceToBuild?.uri)
    return `${t.sourceToBuild.uri}${t.sourceToBuild.ref ? `@${t.sourceToBuild.ref}` : ''}`
  return ''
}

/**
 * Triggers lists the chosen Project's Build triggers in one location and
 * runs them. Creating and editing them is left to the API's clients.
 */
export function Triggers() {
  const [view] = useViewState()
  const project = view.project ?? ''
  const location = useBuildLocation()
  const query = useQuery({ ...triggersQuery(project, location), enabled: !!project })
  const qc = useQueryClient()
  const navigate = useCarriedNavigate()
  const run = useMutation({
    mutationFn: (id: string) => runTrigger(project, location, id),
    onSuccess: (op, id) => {
      void qc.invalidateQueries({ queryKey: ['build'] })
      const b = op.metadata?.build
      toast.success(`Ran Build trigger ${id}${b ? `: Build ${b.id}` : ''}.`)
      if (b) navigate(buildPath(buildRef(b)))
    },
  })
  if (!project) return <ChooseProject what="see its Build triggers" />
  const unimplemented = isUnimplemented(query.error)
  const all = query.data ?? []

  return (
    <div className="flex flex-col gap-6">
      <BuildHeader>
        <p className="text-sm text-muted-foreground">
          Build triggers of Project <span className="font-mono">{project}</span> in{' '}
          <span className="font-mono">{location}</span>
        </p>
      </BuildHeader>
      <Card>
        <LocationFilter what="Build triggers" />
        {unimplemented ? (
          <p role="status" className="text-sm text-muted-foreground">
            Build triggers are not available on this Instance yet.
          </p>
        ) : (
          <QueryStatus query={query} />
        )}
        {query.data && all.length === 0 && (
          <p className="text-sm text-muted-foreground">No Build triggers in {location} yet.</p>
        )}
        {all.length > 0 && (
          <Table aria-label="Build triggers">
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Event</TableHead>
                <TableHead>Source</TableHead>
                <TableHead>Description</TableHead>
                <TableHead>Created</TableHead>
                <TableHead>
                  <span className="sr-only">Actions</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {all.map((t) => {
                const name = t.name || t.id
                return (
                  <TableRow key={t.id} data-testid="trigger">
                    <TableCell className="font-mono">
                      {name}
                      {t.disabled && (
                        <Badge variant="warning" className="ml-2">
                          Disabled
                        </Badge>
                      )}
                    </TableCell>
                    <TableCell>{triggerKind(t)}</TableCell>
                    <TableCell className="font-mono text-xs break-all">
                      {triggerSource(t)}
                    </TableCell>
                    <TableCell>{t.description}</TableCell>
                    <TableCell>{time(t.createTime)}</TableCell>
                    <TableCell className="text-right">
                      <Button
                        size="sm"
                        variant="outline"
                        aria-label={`Run Build trigger ${name}`}
                        disabled={run.isPending}
                        onClick={() => run.mutate(t.id)}
                      >
                        <Play aria-hidden />
                        Run
                      </Button>
                    </TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  )
}
