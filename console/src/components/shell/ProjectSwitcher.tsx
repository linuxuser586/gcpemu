import { zodResolver } from '@hookform/resolvers/zod'
import { useQuery } from '@tanstack/react-query'
import { Check, ChevronsUpDown, FolderOpen } from 'lucide-react'
import { useId, useState } from 'react'
import { useForm, useWatch } from 'react-hook-form'
import { z } from 'zod'

import { errorMessage } from '@/api/errors'
import { infoQuery, projectsQuery } from '@/api/queries'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { PROJECT_ID } from '@/lib/services'
import { useViewState } from '@/lib/viewState'
import { cn } from '@/lib/utils'

const schema = z.object({
  projectId: z
    .string()
    .trim()
    .regex(
      PROJECT_ID,
      'A Project ID is 6 to 30 lowercase letters, digits or hyphens, starting with a letter and not ending with a hyphen.',
    ),
})
type Form = z.infer<typeof schema>

export function strictMessage(id: string) {
  return `Project ${id} does not exist, and this Instance runs with Strict projects. Declare it under "projects:" in the Seed file and apply it.`
}

/**
 * ProjectSwitcher scopes the views to a Project through ?project=.
 * Choosing a Project only changes the URL: it never creates one (a Project
 * exists once a Service request references it, CONTEXT.md).
 */
export function ProjectSwitcher() {
  const [view, setView] = useViewState()
  const [open, setOpen] = useState(false)
  const { data: info } = useQuery(infoQuery())
  const listable = info?.services.includes('iam') ?? false
  const strict = info?.strictProjects ?? false
  const projects = useQuery({ ...projectsQuery(), enabled: listable })
  const known = new Set(projects.data?.map((p) => p.projectId))
  const isKnown = (id: string) => !projects.isSuccess || known.has(id)

  const form = useForm<Form>({
    resolver: zodResolver(schema),
    defaultValues: { projectId: '' },
  })
  const typed = useWatch({ control: form.control, name: 'projectId' }).trim()
  const inputId = useId()
  const errorId = useId()

  const choose = (id: string) => {
    setView({ project: id })
    setOpen(false)
    form.reset()
  }
  const onSubmit = form.handleSubmit(({ projectId }) => {
    if (strict && !isKnown(projectId)) {
      form.setError('projectId', { message: strictMessage(projectId) })
      return
    }
    choose(projectId)
  })

  const current = view.project
  const filtered = (projects.data ?? []).filter((p) => p.projectId.includes(typed))
  const typedUnknown = PROJECT_ID.test(typed) && projects.isSuccess && !known.has(typed)
  const fieldError = form.formState.errors.projectId?.message

  return (
    <div className="flex items-center gap-2">
      <Popover
        open={open}
        onOpenChange={(o) => {
          setOpen(o)
          if (!o) form.reset()
        }}
      >
        <PopoverTrigger asChild>
          <Button variant="outline" size="sm" className="max-w-72" aria-label="Project">
            <FolderOpen aria-hidden />
            <span className="truncate">{current ?? 'Choose a project'}</span>
            <ChevronsUpDown className="opacity-50" aria-hidden />
          </Button>
        </PopoverTrigger>
        <PopoverContent className="w-96">
          <form onSubmit={(e) => void onSubmit(e)} noValidate className="flex flex-col gap-2">
            <label htmlFor={inputId} className="text-sm font-medium">
              Project ID
            </label>
            <div className="flex gap-2">
              <Input
                id={inputId}
                autoComplete="off"
                spellCheck={false}
                placeholder="my-project"
                aria-invalid={!!fieldError}
                aria-describedby={fieldError ? errorId : undefined}
                {...form.register('projectId')}
              />
              <Button type="submit" size="sm" className="h-9">
                Open
              </Button>
            </div>
            {fieldError && (
              <p id={errorId} role="alert" className="text-sm text-destructive">
                {fieldError}
              </p>
            )}
            {!fieldError && typedUnknown && (
              <p className="text-sm text-muted-foreground">
                {strict ? (
                  strictMessage(typed)
                ) : (
                  <>
                    <span className="font-mono">{typed}</span> is not yet created. Opening it
                    creates nothing; it is created by the first Service request that references it.
                  </>
                )}
              </p>
            )}
          </form>

          <div className="mt-3 border-t pt-3">
            <h3 className="mb-1 text-xs font-medium text-muted-foreground">
              Projects on this Instance
            </h3>
            {!listable && (
              <p className="text-sm text-muted-foreground">
                Projects can&apos;t be listed: Resource Manager is part of the iam Service, which is
                not enabled.
              </p>
            )}
            {projects.isPending && listable && (
              <p className="text-sm text-muted-foreground">Loading…</p>
            )}
            {projects.isError && (
              <p role="alert" className="text-sm text-destructive">
                {errorMessage(projects.error)}
              </p>
            )}
            {projects.isSuccess && projects.data.length === 0 && (
              <p className="text-sm text-muted-foreground">
                No Projects yet. A Project is created the first time a Service request references
                it.
              </p>
            )}
            {filtered.length > 0 && (
              <ul className="flex max-h-64 flex-col overflow-y-auto" aria-label="Projects">
                {filtered.map((p) => (
                  <li key={p.projectId}>
                    <button
                      type="button"
                      onClick={() => choose(p.projectId)}
                      className="flex w-full items-center gap-2 rounded-sm px-2 py-1.5 text-left font-mono text-sm outline-none hover:bg-accent focus-visible:bg-accent"
                    >
                      <Check
                        className={cn('size-4', p.projectId !== current && 'invisible')}
                        aria-hidden
                      />
                      {p.projectId}
                    </button>
                  </li>
                ))}
              </ul>
            )}
            {current && (
              <Button
                variant="link"
                size="sm"
                className="mt-2 px-0"
                onClick={() => {
                  setView({ project: undefined })
                  setOpen(false)
                }}
              >
                Clear project
              </Button>
            )}
          </div>
        </PopoverContent>
      </Popover>
      {current && projects.isSuccess && !known.has(current) && (
        <Badge
          variant={strict ? 'destructive' : 'warning'}
          title={strict ? strictMessage(current) : undefined}
        >
          {strict ? 'does not exist' : 'not yet created'}
        </Badge>
      )}
    </div>
  )
}
