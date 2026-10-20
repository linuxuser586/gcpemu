import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Expand, Pencil, Trash2 } from 'lucide-react'
import { useId, useState } from 'react'

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
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { toast } from '@/lib/toast'

import { lastSeg } from '../lb/api'
import {
  deleteSubnetwork,
  expandIpCidrRange,
  setPrivateIpGoogleAccess,
  subnetQuery,
  type Subnetwork,
  type SubnetRef,
} from './api'
import { subnetRecipe } from './copy'
import { CIDR_INVALID, parseCidr } from './form'
import { networkPage, NetworkLink, subnetPage, useSubnetRef } from './VpcLayout'

/**
 * SubnetworkPage is one subnetwork: its ranges and settings, the custom
 * methods that change its primary range and Private Google Access, and
 * the subnetwork as the API returns it.
 */
export function SubnetworkPage() {
  const ref = useSubnetRef()
  const query = useQuery(subnetQuery(ref))
  const s = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const remove = useMutation({
    mutationFn: () => deleteSubnetwork(ref),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(`Deleted subnetwork ${ref.subnetwork}.`)
      navigate(s?.network ? networkPage(lastSeg(s.network)) : '/compute/subnetworks')
    },
  })
  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to="/compute/subnetworks" className="underline-offset-4 hover:underline">
              VPC network
            </Link>{' '}
            / subnetwork
          </p>
          <h1 className="text-2xl font-semibold">
            <span className="font-mono">{ref.subnetwork}</span>{' '}
            <span className="text-sm font-normal text-muted-foreground">{ref.region}</span>
          </h1>
        </div>
        {s && (
          <div className="flex flex-wrap gap-2">
            <CopyAs what="subnetwork" recipe={() => subnetRecipe(s, ref.project)} />
            <Button variant="outline" asChild>
              <Link to={subnetPage(ref, 'edit')}>
                <Pencil aria-hidden />
                Edit subnetwork
              </Link>
            </Button>
            <ExpandRange r={ref} subnet={s} />
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Delete subnetwork
                </Button>
              }
              title={`Delete subnetwork ${ref.subnetwork}?`}
              description="The API refuses while a cluster, instance, address or other resource still uses it."
              confirmLabel="Delete subnetwork"
              onConfirm={() => remove.mutateAsync()}
            />
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {s && (
        <>
          <Card aria-labelledby="subnet-title">
            <CardTitle id="subnet-title">Subnetwork</CardTitle>
            <DetailList
              label="Subnetwork details"
              rows={[
                ['Network', <NetworkLink key="n" link={s.network} />],
                ['Primary range', <Mono key="r">{s.ipCidrRange}</Mono>],
                ['Gateway', s.gatewayAddress && <Mono key="g">{s.gatewayAddress}</Mono>],
                [
                  'Secondary ranges',
                  s.secondaryIpRanges?.length ? (
                    <span key="s" className="flex flex-col">
                      {s.secondaryIpRanges.map((x) => (
                        <span key={x.rangeName}>
                          <span className="text-xs text-muted-foreground">{x.rangeName}</span>{' '}
                          <span className="font-mono">{x.ipCidrRange}</span>
                        </span>
                      ))}
                    </span>
                  ) : (
                    'None'
                  ),
                ],
                ['Purpose', s.purpose && <Mono key="p">{s.purpose}</Mono>],
                ['Stack type', s.stackType && <Mono key="t">{s.stackType}</Mono>],
                ['Description', s.description],
                ['Created', s.creationTimestamp && <Mono key="c">{s.creationTimestamp}</Mono>],
              ]}
            />
          </Card>
          <GoogleAccess r={ref} subnet={s} />
          <Card>
            <CardTitle>Resource</CardTitle>
            <JsonView value={s} label="Subnetwork JSON" />
          </Card>
        </>
      )}
    </div>
  )
}

/** GoogleAccess switches Private Google Access with setPrivateIpGoogleAccess. */
function GoogleAccess({ r, subnet }: { r: SubnetRef; subnet: Subnetwork }) {
  const id = useId()
  const qc = useQueryClient()
  const change = useMutation({
    mutationFn: (on: boolean) => setPrivateIpGoogleAccess(r, on),
    onSuccess: (_, on) => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(`Private Google Access ${on ? 'on' : 'off'} for ${r.subnetwork}.`)
    },
  })
  const on = change.isPending ? !!change.variables : !!subnet.privateIpGoogleAccess
  return (
    <Card aria-labelledby={`${id}-title`}>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <CardTitle id={`${id}-title`}>Private Google Access</CardTitle>
        <Switch
          checked={on}
          aria-labelledby={`${id}-title`}
          aria-describedby={`${id}-hint`}
          disabled={change.isPending}
          onClick={() => change.mutate(!subnet.privateIpGoogleAccess)}
        />
      </div>
      <p id={`${id}-hint`} className="text-sm text-muted-foreground">
        {on
          ? 'Workloads without an external IP can reach Google APIs and services.'
          : 'Workloads without an external IP cannot reach Google APIs and services.'}
      </p>
    </Card>
  )
}

/** ExpandRange widens the primary range with expandIpCidrRange. */
function ExpandRange({ r, subnet }: { r: SubnetRef; subnet: Subnetwork }) {
  const [open, setOpen] = useState(false)
  const [range, setRange] = useState('')
  const [error, setError] = useState<string>()
  const qc = useQueryClient()
  const expand = useMutation({
    meta: { toast: false },
    mutationFn: (v: string) => expandIpCidrRange(r, v),
    onSuccess: (_, v) => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(`Expanded ${r.subnetwork} to ${v}.`)
      setOpen(false)
    },
    onError: (e) => setError(errorMessage(e)),
  })
  const submit = () => {
    const v = range.trim()
    if (!parseCidr(v)) {
      setError(`Invalid value for field 'ipCidrRange': '${v}'. ${CIDR_INVALID}`)
      return
    }
    setError(undefined)
    expand.mutate(v)
  }
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        setOpen(o)
        if (o) {
          setRange(subnet.ipCidrRange ?? '')
          setError(undefined)
        }
      }}
    >
      <DialogTrigger asChild>
        <Button variant="outline">
          <Expand aria-hidden />
          Expand range
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogTitle>Expand the primary range of {r.subnetwork}</DialogTitle>
        <DialogDescription>
          The new range must contain <span className="font-mono">{subnet.ipCidrRange}</span> and be
          larger. A range cannot be shrunk again.
        </DialogDescription>
        <form
          className="flex flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault()
            submit()
          }}
        >
          <Field id="expand-range" label="New range" error={error}>
            <Input
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              value={range}
              onChange={(e) => setRange(e.target.value)}
              {...fieldAria('expand-range', error)}
            />
          </Field>
          <DialogFooter>
            <DialogClose asChild>
              <Button type="button" variant="outline">
                Cancel
              </Button>
            </DialogClose>
            <Button type="submit" disabled={expand.isPending}>
              Expand
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
