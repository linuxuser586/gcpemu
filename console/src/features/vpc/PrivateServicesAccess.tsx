import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Pencil, Plus, Trash2 } from 'lucide-react'
import { useId } from 'react'

import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { CopyAs } from '@/components/resource/CopyAs'
import { DetailList, Mono } from '@/components/resource/DetailList'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { toast } from '@/lib/toast'

import { relPath } from '../lb/api'
import {
  allAddressesQuery,
  connectionsQuery,
  containerNetworksQuery,
  deleteConnection,
  networkPath,
  PSA_SERVICE,
  type Address,
  type NetworkRef,
} from './api'
import { ipOf } from './Addresses'
import { FallbackBadge } from './ContainerNetworks'
import { connectionRecipe } from './copy'
import { AddressLink, allocatePage, connectionPage, containerNetworkAnchor } from './links'

// Private services access (servicenetworking v1): a network's VPC_PEERING
// global addresses, the ranges it allocates to service producers such as
// Cloud SQL, and its connection to them. Each allocation in use is backed
// by a VPC container network (CONTEXT.md), the emulator's own, which the
// admin API reports.

/** allocationsOf are the network's private services access ranges. */
export const allocationsOf = (addresses: Address[], r: NetworkRef) =>
  addresses
    .filter((a) => a.purpose === 'VPC_PEERING' && relPath(a.network) === networkPath(r))
    .sort((a, b) => a.name.localeCompare(b.name))

/**
 * PrivateServicesAccess is a network's allocated ranges, each with the
 * VPC container network behind it, and its connection to the service
 * producer.
 */
export function PrivateServicesAccess({ r }: { r: NetworkRef }) {
  const id = useId()
  const qc = useQueryClient()
  const addresses = useQuery(allAddressesQuery(r.project))
  const connections = useQuery(connectionsQuery(r))
  const containers = useQuery(containerNetworksQuery(r.project, r.network))
  const conn = connections.data?.[0]
  const ranges = allocationsOf(addresses.data ?? [], r)
  const remove = useMutation({
    mutationFn: () => deleteConnection(r),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(`Deleted the private services access connection of ${r.network}.`)
    },
  })
  return (
    <Card aria-labelledby={`${id}-title`}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <CardTitle id={`${id}-title`}>Private services access</CardTitle>
        <Button variant="outline" size="sm" asChild>
          <Link to={allocatePage(r.network)}>
            <Plus aria-hidden />
            Allocate range
          </Link>
        </Button>
      </div>
      <p className="text-sm text-muted-foreground">
        Ranges allocated to service producers such as Cloud SQL private IP, and the connection that
        hands them over.
      </p>
      <QueryStatus query={addresses} />
      {addresses.data && ranges.length === 0 && (
        <p className="text-sm text-muted-foreground">No ranges allocated in this network.</p>
      )}
      {ranges.length > 0 && (
        <Table aria-label="Allocated ranges">
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Range</TableHead>
              <TableHead>Connection</TableHead>
              <TableHead>Container network</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {ranges.map((a) => {
              const cn = containers.data?.find((c) => c.allocation === relPath(a.selfLink))
              return (
                <TableRow key={a.name} data-testid="allocation">
                  <TableCell>
                    <AddressLink region="" name={a.name} />
                  </TableCell>
                  <TableCell className="font-mono">{ipOf(a)}</TableCell>
                  <TableCell>
                    {conn?.reservedPeeringRanges?.includes(a.name) ? 'Connected' : 'Not connected'}
                  </TableCell>
                  <TableCell>
                    {cn ? (
                      <span className="flex flex-col items-start gap-1">
                        <a
                          href={`#${containerNetworkAnchor(cn.bridge)}`}
                          className="font-mono break-all text-primary underline-offset-4 hover:underline"
                        >
                          {cn.bridge}
                        </a>
                        {cn.fallbackRange ? (
                          <span className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                            Fallback range <span className="font-mono">{cn.fallbackRange}</span>
                            <FallbackBadge cn={cn} />
                          </span>
                        ) : (
                          <span className="text-xs text-muted-foreground">Declared range</span>
                        )}
                        <span className="text-xs text-muted-foreground">
                          {cn.containers.length === 1
                            ? '1 Managed container'
                            : `${cn.containers.length} Managed containers`}
                        </span>
                      </span>
                    ) : (
                      <span className="text-muted-foreground">None yet</span>
                    )}
                  </TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      )}
      <QueryStatus query={connections} />
      {connections.data && !conn && (
        <div className="flex flex-wrap items-center justify-between gap-2">
          <p className="text-sm text-muted-foreground">
            No private services access connection: Cloud SQL private IP needs one.
          </p>
          {ranges.length > 0 && (
            <Button variant="outline" size="sm" asChild>
              <Link to={connectionPage(r.network)}>Connect</Link>
            </Button>
          )}
        </div>
      )}
      {conn && (
        <>
          <DetailList
            label="Connection details"
            rows={[
              ['Service', <Mono key="s">{PSA_SERVICE}</Mono>],
              ['Peering', conn.peering && <Mono key="p">{conn.peering}</Mono>],
              [
                'Allocated ranges',
                <Mono key="r">{(conn.reservedPeeringRanges ?? []).join(', ')}</Mono>,
              ],
            ]}
          />
          <div className="flex flex-wrap gap-2">
            <CopyAs
              what="connection"
              size="sm"
              recipe={() => connectionRecipe(conn, r.network, r.project)}
            />
            <Button variant="outline" size="sm" asChild>
              <Link to={connectionPage(r.network)}>
                <Pencil aria-hidden />
                Edit ranges
              </Link>
            </Button>
            <ConfirmDialog
              trigger={
                <Button variant="outline" size="sm">
                  <Trash2 aria-hidden />
                  Delete connection
                </Button>
              }
              title={`Delete the private services access connection of ${r.network}?`}
              description="The API refuses while producer services, such as a Cloud SQL instance with a private IP, still use it."
              confirmLabel="Delete connection"
              onConfirm={() => remove.mutateAsync()}
            />
          </div>
        </>
      )}
    </Card>
  )
}
