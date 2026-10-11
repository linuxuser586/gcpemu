import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ArchiveRestore, Plus } from 'lucide-react'
import { useId, useState } from 'react'

import { errorMessage } from '@/api/errors'
import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogTitle,
  DialogTrigger,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
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

import { serviceAccountsQuery, undeleteServiceAccount } from './api'
import { accountPath, ChooseProject } from './IamLayout'

/**
 * ServiceAccounts lists the chosen Project's service accounts (FR-IAM-001);
 * ?q= keeps those whose email or display name contains it.
 */
export function ServiceAccounts() {
  const [view, setView] = useViewState()
  const project = view.project ?? ''
  const [text, setText] = useState(view.q ?? '')
  const query = useQuery({ ...serviceAccountsQuery(project), enabled: !!project })
  if (!project) return <ChooseProject title="Service accounts" what="see its service accounts" />
  const all = query.data ?? []
  const q = text.toLowerCase()
  const shown = all.filter(
    (a) => a.email.toLowerCase().includes(q) || (a.displayName ?? '').toLowerCase().includes(q),
  )

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Service accounts</h1>
          <p className="text-sm text-muted-foreground">
            Of Project <span className="font-mono">{project}</span>
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          <Undelete />
          <Button asChild>
            <Link to="/iam/create-service-account">
              <Plus aria-hidden />
              Create service account
            </Link>
          </Button>
        </div>
      </div>
      <Card>
        <form
          role="search"
          aria-label="Filter service accounts"
          onSubmit={(e) => e.preventDefault()}
        >
          <label htmlFor="sa-filter" className="flex flex-col gap-1 text-sm">
            Filter
            <Input
              id="sa-filter"
              type="search"
              placeholder="Email or display name"
              value={text}
              onChange={(e) => {
                setText(e.target.value)
                setView({ q: e.target.value })
              }}
            />
          </label>
        </form>
        <QueryStatus query={query} />
        {query.data && all.length === 0 && (
          <p className="text-sm text-muted-foreground">No service accounts in this Project yet.</p>
        )}
        {query.data && all.length > 0 && shown.length === 0 && (
          <p className="text-sm text-muted-foreground">No service accounts match the filter.</p>
        )}
        {shown.length > 0 && (
          <Table aria-label="Service accounts">
            <TableHeader>
              <TableRow>
                <TableHead>Email</TableHead>
                <TableHead>Display name</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Unique ID</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((a) => (
                <TableRow key={a.email} data-testid="service-account">
                  <TableCell>
                    <Link
                      className="font-mono text-primary underline-offset-4 hover:underline"
                      to={accountPath(a.email)}
                    >
                      {a.email}
                    </Link>
                  </TableCell>
                  <TableCell>{a.displayName}</TableCell>
                  <TableCell>
                    <AccountStatus disabled={a.disabled} />
                  </TableCell>
                  <TableCell className="font-mono text-xs">{a.uniqueId}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  )
}

export function AccountStatus({ disabled }: { disabled?: boolean }) {
  return disabled ? (
    <Badge variant="warning">Disabled</Badge>
  ) : (
    <Badge variant="success">Enabled</Badge>
  )
}

/** Undelete restores a deleted service account by its unique ID. */
function Undelete() {
  const [open, setOpen] = useState(false)
  const [uid, setUid] = useState('')
  const [error, setError] = useState<string>()
  const id = useId()
  const qc = useQueryClient()
  const navigate = useCarriedNavigate()
  const undelete = useMutation({
    meta: { toast: false },
    mutationFn: () => undeleteServiceAccount(uid.trim()),
    onSuccess: (res) => {
      void qc.invalidateQueries({ queryKey: ['iam'] })
      const email = res.restoredAccount?.email ?? ''
      toast.success(`Restored service account ${email}.`)
      setOpen(false)
      if (email) navigate(accountPath(email))
    },
    onError: (e) => setError(errorMessage(e)),
  })
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        setOpen(o)
        setError(undefined)
      }}
    >
      <DialogTrigger asChild>
        <Button variant="outline">
          <ArchiveRestore aria-hidden />
          Undelete
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogTitle>Undelete a service account</DialogTitle>
        <DialogDescription>
          A deleted service account is restored with its unique ID, which the console showed when it
          was deleted. Its keys and its own policy are not restored.
        </DialogDescription>
        <form
          className="flex flex-col gap-3"
          onSubmit={(e) => {
            e.preventDefault()
            if (!uid.trim()) {
              setError('Enter the unique ID of the deleted service account.')
              return
            }
            undelete.mutate()
          }}
        >
          <label htmlFor={id} className="text-sm font-medium">
            Unique ID
          </label>
          <Input
            id={id}
            autoComplete="off"
            spellCheck={false}
            className="font-mono"
            value={uid}
            aria-invalid={!!error}
            onChange={(e) => {
              setUid(e.target.value)
              setError(undefined)
            }}
          />
          {error && (
            <p role="alert" className="text-sm text-destructive">
              {error}
            </p>
          )}
          <DialogFooter>
            <DialogClose asChild>
              <Button type="button" variant="outline">
                Cancel
              </Button>
            </DialogClose>
            <Button type="submit" disabled={undelete.isPending}>
              Undelete
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
