import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Tags, Trash2 } from 'lucide-react'
import { useState } from 'react'

import { errorMessage } from '@/api/errors'
import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { CopyAs } from '@/components/resource/CopyAs'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Field, fieldAria } from '@/components/resource/Field'
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
import { Textarea } from '@/components/ui/textarea'
import { formatLabels, labelsError, parseLabels } from '@/lib/labels'
import { toast } from '@/lib/toast'

import { lastSeg } from '../lb/api'
import { addressQuery, deleteAddress, setAddressLabels, type Address, type AddressRef } from './api'
import { ipOf, scopeLabel } from './Addresses'
import { addressRecipe } from './copy'
import { AddressUsers, NetworkLink, SubnetLink } from './links'
import { useAddressRef } from './VpcLayout'

/**
 * AddressPage is one regional or global address: what it reserves, the
 * resources that use it, its labels, and the address as the API returns
 * it. Addresses cannot be changed otherwise.
 */
export function AddressPage() {
  const ref = useAddressRef()
  const query = useQuery(addressQuery(ref))
  const a = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const remove = useMutation({
    mutationFn: () => deleteAddress(ref),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(`Released address ${ref.address}.`)
      navigate('/compute/addresses')
    },
  })
  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to="/compute/addresses" className="underline-offset-4 hover:underline">
              VPC network
            </Link>{' '}
            / IP address
          </p>
          <h1 className="text-2xl font-semibold">
            <span className="font-mono">{ref.address}</span>{' '}
            <span className="text-sm font-normal text-muted-foreground">
              {ref.region || 'global'}
            </span>
          </h1>
        </div>
        {a && (
          <div className="flex flex-wrap gap-2">
            <CopyAs what="address" recipe={() => addressRecipe(a, ref.project)} />
            <SetLabels r={ref} address={a} />
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Release address
                </Button>
              }
              title={`Release address ${ref.address}?`}
              description="The API refuses while a forwarding rule, NAT, VM instance or private services access connection still uses it."
              confirmLabel="Release address"
              onConfirm={() => remove.mutateAsync()}
            />
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {a && (
        <>
          <Card aria-labelledby="address-title">
            <CardTitle id="address-title">IP address</CardTitle>
            <DetailList
              label="Address details"
              rows={[
                ['Address', <Mono key="a">{ipOf(a)}</Mono>],
                ['Scope', scopeLabel(a)],
                ['Type', <Mono key="t">{a.addressType ?? 'EXTERNAL'}</Mono>],
                ['Purpose', a.purpose && <Mono key="p">{a.purpose}</Mono>],
                ['Status', a.status && <Mono key="s">{a.status}</Mono>],
                ['In use by', <AddressUsers key="u" users={a.users} />],
                [
                  'Subnetwork',
                  a.subnetwork && (
                    <SubnetLink key="sn" region={lastSeg(a.region)} name={lastSeg(a.subnetwork)} />
                  ),
                ],
                ['Network', a.network && <NetworkLink key="n" link={a.network} />],
                ['Network tier', a.networkTier && <Mono key="nt">{a.networkTier}</Mono>],
                [
                  'Labels',
                  Object.keys(a.labels ?? {}).length ? (
                    <Mono key="l">{formatLabels(a.labels).replaceAll('\n', ', ')}</Mono>
                  ) : (
                    'None'
                  ),
                ],
                ['Description', a.description],
                ['Created', a.creationTimestamp && <Mono key="c">{a.creationTimestamp}</Mono>],
              ]}
            />
          </Card>
          <Card>
            <CardTitle>Resource</CardTitle>
            <JsonView value={a} label="Address JSON" />
          </Card>
        </>
      )}
    </div>
  )
}

/** SetLabels replaces the address's labels with setLabels. */
function SetLabels({ r, address }: { r: AddressRef; address: Address }) {
  const [open, setOpen] = useState(false)
  const [text, setText] = useState('')
  const [error, setError] = useState<string>()
  const qc = useQueryClient()
  const save = useMutation({
    meta: { toast: false },
    mutationFn: (labels: Record<string, string>) =>
      setAddressLabels(r, labels, address.labelFingerprint),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(`Set the labels of ${r.address}.`)
      setOpen(false)
    },
    onError: (e) => setError(errorMessage(e)),
  })
  const submit = () => {
    const message = labelsError(text)
    setError(message)
    if (!message) save.mutate(parseLabels(text))
  }
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        setOpen(o)
        if (o) {
          setText(formatLabels(address.labels))
          setError(undefined)
        }
      }}
    >
      <DialogTrigger asChild>
        <Button variant="outline">
          <Tags aria-hidden />
          Set labels
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogTitle>Labels of {r.address}</DialogTitle>
        <DialogDescription>
          The labels replace the address&apos;s current ones; none removes them all.
        </DialogDescription>
        <form
          className="flex flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault()
            submit()
          }}
        >
          <Field id="address-labels" label="Labels" error={error} hint="key=value, one per line.">
            <Textarea
              rows={3}
              spellCheck={false}
              className="font-mono"
              value={text}
              onChange={(e) => setText(e.target.value)}
              {...fieldAria('address-labels', error, 'key=value')}
            />
          </Field>
          <DialogFooter>
            <DialogClose asChild>
              <Button type="button" variant="outline">
                Cancel
              </Button>
            </DialogClose>
            <Button type="submit" disabled={save.isPending}>
              Save
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
