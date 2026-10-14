import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Ban, CircleCheck, Download, FlaskConical, Pencil, Trash2, Upload } from 'lucide-react'
import { useId, useState } from 'react'
import { Outlet, useLocation, useOutletContext } from 'react-router'

import { errorMessage } from '@/api/errors'
import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { CopyAs } from '@/components/resource/CopyAs'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogTitle,
  DialogTrigger,
} from '@/components/ui/dialog'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { Textarea } from '@/components/ui/textarea'
import { toast } from '@/lib/toast'
import { cn } from '@/lib/utils'

import { time } from '../gcs/format'
import { downloadText } from '../gke/kubeconfig'
import {
  createKey,
  decodeBase64,
  deleteKey,
  deleteServiceAccount,
  keysQuery,
  lastSegment,
  serviceAccountQuery,
  setAccountDisabled,
  setKeyDisabled,
  uploadKey,
  type ServiceAccount,
} from './api'
import { serviceAccountRecipe } from './copy'
import { accountPath, ChooseProject, simulatorPath, useAccount } from './IamLayout'
import { PolicyEditor } from './PolicyEditor'
import { AccountStatus } from './ServiceAccounts'

const TABS = [
  ['', 'Details'],
  ['keys', 'Keys'],
  ['permissions', 'Permissions'],
] as const

/**
 * ServiceAccountPage is one service account (FR-IAM-001): edit, disable or
 * enable and delete it above tabs for its details, its keys and the
 * policy on it.
 */
export function ServiceAccountPage() {
  const { project, email } = useAccount()
  const query = useQuery({ ...serviceAccountQuery(project, email), enabled: !!project })
  const sa = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const { pathname } = useLocation()
  const del = useMutation({
    mutationFn: () => deleteServiceAccount(project, email),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['iam'] })
      toast.success(
        `Deleted service account ${email}. Undelete it with its unique ID ${sa?.uniqueId ?? ''}.`,
      )
      navigate('/iam/serviceAccounts')
    },
  })
  const toggle = useMutation({
    mutationFn: (disabled: boolean) => setAccountDisabled(project, email, disabled),
    onSuccess: (_, disabled) => {
      void qc.invalidateQueries({ queryKey: ['iam'] })
      toast.success(`${disabled ? 'Disabled' : 'Enabled'} service account ${email}.`)
    },
  })
  if (!project) return <ChooseProject title="Service account" what="see its service accounts" />
  const base = accountPath(email)
  const rest = pathname.slice(pathname.indexOf(base) + base.length).split('/')[1] ?? ''
  const active = TABS.some(([p]) => p === rest) ? rest : ''

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to="/iam/serviceAccounts" className="underline-offset-4 hover:underline">
              Service accounts
            </Link>{' '}
            / <span className="font-mono">{project}</span>
          </p>
          <h1 className="flex flex-wrap items-center gap-2 text-2xl font-semibold">
            <span className="font-mono break-all">{email}</span>
            {sa && <AccountStatus disabled={sa.disabled} />}
          </h1>
        </div>
        {sa && (
          <div className="flex flex-wrap gap-2">
            <CopyAs what="service account" recipe={() => serviceAccountRecipe(sa)} />
            <Button variant="outline" asChild>
              <Link to={accountPath(email, 'edit')}>
                <Pencil aria-hidden />
                Edit service account
              </Link>
            </Button>
            {sa.disabled ? (
              <Button
                variant="outline"
                disabled={toggle.isPending}
                onClick={() => toggle.mutate(false)}
              >
                <CircleCheck aria-hidden />
                Enable
              </Button>
            ) : (
              <ConfirmDialog
                trigger={
                  <Button variant="outline">
                    <Ban aria-hidden />
                    Disable
                  </Button>
                }
                title={`Disable service account ${email}?`}
                description="Its tokens and keys stop working until it is enabled again."
                confirmLabel="Disable"
                onConfirm={() => toggle.mutateAsync(true)}
              />
            )}
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Delete service account
                </Button>
              }
              title={`Delete service account ${email}?`}
              description={`Its keys and the policy on it are deleted. It can be undeleted with its unique ID ${sa.uniqueId ?? ''}.`}
              confirmLabel="Delete service account"
              onConfirm={() => del.mutateAsync()}
            />
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {sa && (
        <>
          <nav aria-label="Service account" className="flex flex-wrap gap-1 border-b">
            {TABS.map(([path, label]) => (
              <Link
                key={path}
                to={path ? `${base}/${path}` : base}
                aria-current={active === path ? 'page' : undefined}
                className={cn(
                  '-mb-px border-b-2 px-3 py-2 text-sm font-medium outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50',
                  active === path
                    ? 'border-primary text-foreground'
                    : 'border-transparent text-muted-foreground hover:text-foreground',
                )}
              >
                {label}
              </Link>
            ))}
          </nav>
          <Outlet context={sa} />
        </>
      )}
    </div>
  )
}

/** useServiceAccount is the account a ServiceAccountPage tab belongs to. */
const useServiceAccount = () => useOutletContext<ServiceAccount>()

/** AccountDetails shows the account's fields and the resource as JSON. */
export function AccountDetails() {
  const sa = useServiceAccount()
  return (
    <div className="flex flex-col gap-6">
      <Card>
        <CardTitle>Details</CardTitle>
        <DetailList
          label="Service account details"
          rows={[
            ['Name', <Mono key="n">{sa.name}</Mono>],
            ['Email', <Mono key="e">{sa.email}</Mono>],
            ['Display name', sa.displayName],
            ['Description', sa.description],
            ['Unique ID', <Mono key="u">{sa.uniqueId}</Mono>],
            ['OAuth 2 client ID', sa.oauth2ClientId && <Mono key="o">{sa.oauth2ClientId}</Mono>],
            ['Status', sa.disabled ? 'Disabled' : 'Enabled'],
          ]}
        />
      </Card>
      <Card>
        <CardTitle>Service account resource</CardTitle>
        <JsonView value={sa} label="Service account JSON" />
      </Card>
    </div>
  )
}

/**
 * Keys lists the account's keys; creates a JSON key and downloads its key
 * file, uploads a public key, and disables, enables and deletes keys.
 */
export function Keys() {
  const sa = useServiceAccount()
  const { project } = useAccount()
  const query = useQuery(keysQuery(project, sa.email))
  const qc = useQueryClient()
  const done = () => void qc.invalidateQueries({ queryKey: ['iam', 'keys'] })
  const create = useMutation({
    mutationFn: () => createKey(project, sa.email),
    onSuccess: (k) => {
      done()
      const id = lastSegment(k.name)
      downloadText(
        `${project}-${id.slice(0, 12)}.json`,
        decodeBase64(k.privateKeyData ?? ''),
        'application/json',
      )
      toast.success(`Created key ${id} and downloaded its key file. It cannot be downloaded again.`)
    },
  })
  const toggle = useMutation({
    mutationFn: ({ key, disabled }: { key: string; disabled: boolean }) =>
      setKeyDisabled(project, sa.email, key, disabled),
    onSuccess: (_, { key, disabled }) => {
      done()
      toast.success(`${disabled ? 'Disabled' : 'Enabled'} key ${key}.`)
    },
  })
  const del = useMutation({
    mutationFn: (key: string) => deleteKey(project, sa.email, key),
    onSuccess: (_, key) => {
      done()
      toast.success(`Deleted key ${key}.`)
    },
  })
  const keys = query.data ?? []
  return (
    <Card>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <CardTitle>Keys</CardTitle>
        <div className="flex flex-wrap gap-2">
          <UploadKey project={project} email={sa.email} />
          <Button disabled={create.isPending} onClick={() => create.mutate()}>
            <Download aria-hidden />
            Create JSON key
          </Button>
        </div>
      </div>
      <QueryStatus query={query} />
      {query.data && keys.length === 0 && <p className="text-sm text-muted-foreground">No keys.</p>}
      {keys.length > 0 && (
        <Table aria-label="Keys">
          <TableHeader>
            <TableRow>
              <TableHead>Key ID</TableHead>
              <TableHead>Type</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Created</TableHead>
              <TableHead>Expires</TableHead>
              <TableHead>
                <span className="sr-only">Actions</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {keys.map((k) => {
              const id = lastSegment(k.name)
              const user = k.keyType !== 'SYSTEM_MANAGED'
              return (
                <TableRow key={k.name} data-testid="key">
                  <TableCell className="font-mono text-xs break-all">{id}</TableCell>
                  <TableCell>
                    {user ? 'User-managed' : 'Google-managed'}
                    {k.keyOrigin === 'USER_PROVIDED' && ' (uploaded)'}
                  </TableCell>
                  <TableCell>
                    {k.disabled ? (
                      <Badge variant="warning">Disabled</Badge>
                    ) : (
                      <Badge variant="success">Enabled</Badge>
                    )}
                  </TableCell>
                  <TableCell>{time(k.validAfterTime)}</TableCell>
                  <TableCell>{time(k.validBeforeTime)}</TableCell>
                  <TableCell>
                    {user && (
                      <div className="flex flex-wrap justify-end gap-2">
                        <Button
                          size="sm"
                          variant="outline"
                          aria-label={`${k.disabled ? 'Enable' : 'Disable'} key ${id}`}
                          onClick={() => toggle.mutate({ key: id, disabled: !k.disabled })}
                        >
                          {k.disabled ? 'Enable' : 'Disable'}
                        </Button>
                        <ConfirmDialog
                          trigger={
                            <Button size="sm" variant="outline" aria-label={`Delete key ${id}`}>
                              Delete
                            </Button>
                          }
                          title={`Delete key ${id}?`}
                          description="Credentials that use it stop working. This cannot be undone."
                          confirmLabel="Delete key"
                          onConfirm={() => del.mutateAsync(id)}
                        />
                      </div>
                    )}
                  </TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}

/** UploadKey uploads a public key certificate (PEM) as a key of the account. */
function UploadKey({ project, email }: { project: string; email: string }) {
  const [open, setOpen] = useState(false)
  const [pem, setPem] = useState('')
  const [error, setError] = useState<string>()
  const id = useId()
  const qc = useQueryClient()
  const upload = useMutation({
    meta: { toast: false },
    mutationFn: () => uploadKey(project, email, pem),
    onSuccess: (k) => {
      void qc.invalidateQueries({ queryKey: ['iam', 'keys'] })
      toast.success(`Uploaded key ${lastSegment(k.name)}.`)
      setOpen(false)
      setPem('')
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
          <Upload aria-hidden />
          Upload key
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogTitle>Upload a public key</DialogTitle>
        <DialogDescription>
          An X.509 certificate of an RSA key, in PEM. JWTs signed with its private key then
          authenticate as the service account.
        </DialogDescription>
        <form
          className="flex flex-col gap-3"
          onSubmit={(e) => {
            e.preventDefault()
            upload.mutate()
          }}
        >
          <label htmlFor={id} className="text-sm font-medium">
            Certificate (PEM)
          </label>
          <Textarea
            id={id}
            rows={8}
            spellCheck={false}
            className="font-mono text-xs"
            placeholder="-----BEGIN CERTIFICATE-----"
            value={pem}
            onChange={(e) => {
              setPem(e.target.value)
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
            <Button type="submit" disabled={upload.isPending}>
              Upload
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/**
 * AccountPermissions is the policy on the account (who may act as it or
 * manage it), and a link to test what the account itself may do.
 */
export function AccountPermissions() {
  const sa = useServiceAccount()
  const { project } = useAccount()
  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="text-sm text-muted-foreground">
          Who may use or manage this service account, e.g. with{' '}
          <span className="font-mono">roles/iam.serviceAccountTokenCreator</span>.
        </p>
        <Button variant="outline" asChild>
          <Link to={simulatorPath(`serviceAccount:${sa.email}`)}>
            <FlaskConical aria-hidden />
            Test what it may do
          </Link>
        </Button>
      </div>
      <PolicyEditor r={{ project, kind: 'serviceAccount', name: sa.email }} />
    </div>
  )
}
