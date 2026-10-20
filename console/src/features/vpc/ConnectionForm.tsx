import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm } from 'react-hook-form'
import { z } from 'zod'

import { errorMessage } from '@/api/errors'
import { useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ResourceEditor, type Body } from '@/components/resource/ResourceEditor'
import { Card } from '@/components/ui/card'
import { toast } from '@/lib/toast'

import {
  allAddressesQuery,
  connectionsQuery,
  createConnection,
  networkPath,
  patchConnection,
  type Connection,
  type NetworkRef,
} from './api'
import { ipOf } from './Addresses'
import { networkPage } from './links'
import { allocationsOf } from './PrivateServicesAccess'
import { useNetworkRef } from './VpcLayout'

// The form for a network's private services access connection
// (FR-UI-011): connections.create when the network has none, else
// connections.patch of its allocated ranges, which they replace.

const schema = z.object({
  ranges: z.array(z.string()).min(1, 'At least one reserved peering range is required.'),
  force: z.boolean(),
})
export type Values = z.infer<typeof schema>

export function ConnectionPage() {
  const r = useNetworkRef()
  const connections = useQuery(connectionsQuery(r))
  const addresses = useQuery(allAddressesQuery(r.project))
  if (!connections.data || !addresses.data) {
    return (
      <>
        <QueryStatus query={connections} />
        <QueryStatus query={addresses} />
      </>
    )
  }
  return (
    <ConnectionForm
      r={r}
      conn={connections.data[0]}
      ranges={allocationsOf(addresses.data, r).map((a) => ({ name: a.name, ip: ipOf(a) }))}
    />
  )
}

function ConnectionForm({
  r,
  conn,
  ranges,
}: {
  r: NetworkRef
  conn?: Connection
  ranges: { name: string; ip: string }[]
}) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: { ranges: conn?.reservedPeeringRanges ?? [], force: false },
  })
  const errors = form.formState.errors
  const toBody = (v: Values, base: Body): Body => ({
    ...base,
    network: networkPath(r),
    reservedPeeringRanges: v.ranges,
  })
  const fromBody = (body: Body, prev: Values): Values => ({
    ...prev,
    ranges: Array.isArray(body.reservedPeeringRanges)
      ? body.reservedPeeringRanges.filter((x): x is string => typeof x === 'string')
      : [],
  })
  const back = () => navigate(networkPage(r.network))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async ({ body, force }: { body: Body; force: boolean }) => {
      const want = (body.reservedPeeringRanges as string[] | undefined) ?? []
      try {
        await (conn ? patchConnection(r, want, force) : createConnection(r, want))
      } catch (e) {
        form.setError('ranges', { type: 'server', message: errorMessage(e) })
        throw e
      }
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(
        conn
          ? `Updated the private services access connection of ${r.network}.`
          : `Connected ${r.network} for private services access.`,
      )
      back()
    },
  })
  return (
    <Card>
      <h1 className="text-lg font-semibold">
        {conn ? 'Edit the connection of' : 'Connect'} <span className="font-mono">{r.network}</span>{' '}
        <span className="text-sm font-normal text-muted-foreground">
          for private services access
        </span>
      </h1>
      <ResourceEditor
        form={form}
        initialBody={toBody(form.getValues(), {})}
        toBody={toBody}
        fromBody={fromBody}
        fields={['ranges']}
        onSubmit={(body, values) => save.mutateAsync({ body, force: values.force })}
        submitLabel={conn ? 'Save' : 'Connect'}
        onCancel={back}
        jsonHint={
          conn ? (
            <>
              Sent to <span className="font-mono">services.connections.patch</span> with{' '}
              <span className="font-mono">updateMask=reservedPeeringRanges</span>: the ranges
              replace the connection&apos;s.
            </>
          ) : (
            <>
              Sent to <span className="font-mono">services.connections.create</span> of{' '}
              <span className="font-mono">servicenetworking.googleapis.com</span>.
            </>
          )
        }
      >
        <fieldset
          className="flex flex-col gap-2"
          aria-describedby={errors.ranges ? 'connection-ranges-error' : undefined}
        >
          <legend className="text-sm font-medium">Allocated ranges</legend>
          {ranges.length === 0 && (
            <p className="text-sm text-muted-foreground">
              The network has no VPC_PEERING ranges: allocate one first.
            </p>
          )}
          {ranges.map((x) => (
            <label key={x.name} className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                className="size-4"
                value={x.name}
                {...form.register('ranges')}
              />
              <span className="font-mono">{x.name}</span>
              <span className="font-mono text-muted-foreground">{x.ip}</span>
            </label>
          ))}
          {errors.ranges && (
            <p id="connection-ranges-error" role="alert" className="text-sm text-destructive">
              {errors.ranges.message}
            </p>
          )}
        </fieldset>
        {conn && (
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" className="size-4" {...form.register('force')} />
            Force: remove ranges even while producer services use them
          </label>
        )}
      </ResourceEditor>
    </Card>
  )
}
