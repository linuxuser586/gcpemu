import { zodResolver } from '@hookform/resolvers/zod'
import { queryOptions, useQuery, useQueryClient } from '@tanstack/react-query'
import { Bot, Check, ChevronsUpDown, UserRound } from 'lucide-react'
import { useId, useState } from 'react'
import { useForm } from 'react-hook-form'
import { z } from 'zod'

import { errorMessage } from '@/api/errors'
import { gcpFetch } from '@/api/fetch'
import { infoQuery } from '@/api/queries'
import { PRINCIPAL } from '@/api/tokens'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { cn } from '@/lib/utils'
import { useViewState } from '@/lib/viewState'
import { usePrefs } from '@/stores/prefs'

/**
 * toPrincipal reads what the user typed as a Principal: "user:EMAIL" and
 * "serviceAccount:EMAIL" as they are, and a bare email as a service
 * account when it has a service-account domain, otherwise as a user.
 */
export function toPrincipal(typed: string): string {
  const s = typed.trim()
  if (s.includes(':') || !s.includes('@')) return s
  return s.endsWith('.gserviceaccount.com') ? `serviceAccount:${s}` : `user:${s}`
}

const schema = z.object({
  principal: z
    .string()
    .trim()
    .transform(toPrincipal)
    .refine((p) => PRINCIPAL.test(p), {
      message: 'Enter an email, or user:EMAIL or serviceAccount:EMAIL.',
    }),
})
type FormIn = z.input<typeof schema>
type FormOut = z.output<typeof schema>

interface Account {
  email: string
  displayName?: string
  disabled?: boolean
}

/**
 * accountsQuery lists the Project's service accounts as the Default
 * principal, so that a Principal IAM keeps from listing them can still
 * switch to another.
 */
const accountsQuery = (project: string) =>
  queryOptions({
    queryKey: ['iam', 'serviceAccounts', project, 'switcher'],
    queryFn: async () =>
      (
        await gcpFetch<{ accounts?: Account[] }>(
          `/iam/v1/projects/${encodeURIComponent(project)}/serviceAccounts?pageSize=100`,
          { principal: '' },
        )
      ).accounts ?? [],
  })

/**
 * PrincipalSwitcher chooses who the console acts as (FR-UI-004): the
 * Default principal, or any user or service account, so that what IAM
 * allows them shows in every view. The choice is a per-browser preference
 * and never travels in a link.
 */
export function PrincipalSwitcher() {
  const queryClient = useQueryClient()
  const principal = usePrefs((s) => s.principal)
  const setPrincipal = usePrefs((s) => s.setPrincipal)
  const [view] = useViewState()
  const [open, setOpen] = useState(false)
  const { data: info } = useQuery(infoQuery())
  const project = view.project
  const listable = !!project && (info?.services.includes('iam') ?? false)
  const accounts = useQuery({ ...accountsQuery(project ?? ''), enabled: listable && open })

  const form = useForm<FormIn, unknown, FormOut>({
    resolver: zodResolver(schema),
    defaultValues: { principal: '' },
  })
  const inputId = useId()
  const errorId = useId()

  const choose = (p: string) => {
    setOpen(false)
    form.reset()
    if (p === principal) return
    setPrincipal(p)
    // What the console showed was what the last Principal may see.
    void queryClient.resetQueries({ predicate: (q) => q.queryKey[0] !== 'admin' })
  }
  const onSubmit = form.handleSubmit(({ principal: p }) => choose(p))
  const fieldError = form.formState.errors.principal?.message

  const isSA = principal.startsWith('serviceAccount:')
  const Icon = isSA ? Bot : UserRound
  const label = principal ? principal.slice(principal.indexOf(':') + 1) : 'Default principal'

  const option = (p: string, text: string, hint?: string) => (
    <li key={p || 'default'}>
      <button
        type="button"
        onClick={() => choose(p)}
        className="flex w-full items-center gap-2 rounded-sm px-2 py-1.5 text-left text-sm outline-none hover:bg-accent focus-visible:bg-accent"
      >
        <Check className={cn('size-4 shrink-0', p !== principal && 'invisible')} aria-hidden />
        <span className="truncate">{text}</span>
        {hint && <span className="ml-auto shrink-0 text-xs text-muted-foreground">{hint}</span>}
      </button>
    </li>
  )

  return (
    <Popover
      open={open}
      onOpenChange={(o) => {
        setOpen(o)
        if (!o) form.reset()
      }}
    >
      <PopoverTrigger asChild>
        <Button
          variant="outline"
          size="sm"
          className={cn('max-w-72', principal && 'border-warning/60')}
          aria-label="Acting as"
          title={principal ? `Acting as ${principal}` : 'Acting as the Default principal'}
        >
          <Icon aria-hidden />
          <span className="truncate">{label}</span>
          <ChevronsUpDown className="opacity-50" aria-hidden />
        </Button>
      </PopoverTrigger>
      <PopoverContent className="w-96">
        <form onSubmit={(e) => void onSubmit(e)} noValidate className="flex flex-col gap-2">
          <label htmlFor={inputId} className="text-sm font-medium">
            Act as
          </label>
          <div className="flex gap-2">
            <Input
              id={inputId}
              autoComplete="off"
              spellCheck={false}
              placeholder="user:alice@example.com"
              aria-invalid={!!fieldError}
              aria-describedby={fieldError ? errorId : undefined}
              {...form.register('principal')}
            />
            <Button type="submit" size="sm" className="h-9">
              Switch
            </Button>
          </div>
          {fieldError && (
            <p id={errorId} role="alert" className="text-sm text-destructive">
              {fieldError}
            </p>
          )}
        </form>

        <ul className="mt-3 flex flex-col border-t pt-3" aria-label="Principals">
          {option('', 'Default principal', 'no credentials')}
          {principal && !accounts.data?.some((a) => `serviceAccount:${a.email}` === principal)
            ? option(principal, principal)
            : null}
        </ul>

        <div className="mt-3 border-t pt-3">
          <h3 className="mb-1 text-xs font-medium text-muted-foreground">
            Service accounts{project ? ` in ${project}` : ''}
          </h3>
          {!project && (
            <p className="text-sm text-muted-foreground">
              Choose a Project to list its service accounts.
            </p>
          )}
          {project && !listable && (
            <p className="text-sm text-muted-foreground">
              Service accounts can&apos;t be listed: the iam Service is not enabled.
            </p>
          )}
          {accounts.isPending && listable && (
            <p className="text-sm text-muted-foreground">Loading…</p>
          )}
          {accounts.isError && (
            <p role="alert" className="text-sm text-destructive">
              {errorMessage(accounts.error)}
            </p>
          )}
          {accounts.isSuccess && accounts.data.length === 0 && (
            <p className="text-sm text-muted-foreground">No service accounts in this Project.</p>
          )}
          {accounts.isSuccess && accounts.data.length > 0 && (
            <ul className="flex max-h-64 flex-col overflow-y-auto" aria-label="Service accounts">
              {accounts.data.map((a) =>
                option(`serviceAccount:${a.email}`, a.email, a.disabled ? 'disabled' : undefined),
              )}
            </ul>
          )}
        </div>
      </PopoverContent>
    </Popover>
  )
}
