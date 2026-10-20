import { useQuery } from '@tanstack/react-query'
import { useId } from 'react'

import { QueryStatus } from '@/components/QueryStatus'
import { DetailList, Mono } from '@/components/resource/DetailList'
import { Badge } from '@/components/ui/badge'
import { Card, CardTitle } from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'

import { lastSeg } from '../lb/api'
import { containerNetworksQuery, type ContainerNetwork, type SubnetRef, subnetPath } from './api'
import { AddressLink, SubnetLink } from './links'

// VPC container networks (CONTEXT.md): the container network behind each
// subnetwork and private services access allocation. No GCP API has
// them; the admin API does.

type Container = ContainerNetwork['containers'][number]

/** FallbackBadge says whether a network on a Fallback range is Translated. */
function FallbackBadge({ cn }: { cn: ContainerNetwork }) {
  return cn.translated ? (
    <Badge variant="warning">Translated</Badge>
  ) : (
    <Badge variant="destructive">Not translated</Badge>
  )
}

/** role names a Managed container's role, after the glossary where it has a term. */
const role = (c: Container) =>
  c.service === 'compute' && c.role === 'nat' ? 'NAT egress proxy' : c.role

/**
 * ContainerNetworkPanel is the VPC container network of one subnetwork:
 * its ranges, its Host address beside the subnetwork's Gateway address,
 * and the Managed containers on it with the address each reports and the
 * one it holds.
 */
export function ContainerNetworkPanel({ r, gateway }: { r: SubnetRef; gateway?: string }) {
  const id = useId()
  const query = useQuery(containerNetworksQuery(r.project))
  const cn = query.data?.find((c) => c.subnetwork === subnetPath(r))
  const apart = !!cn?.hostAddress && !!gateway && cn.hostAddress !== gateway
  return (
    <Card aria-labelledby={`${id}-title`}>
      <CardTitle id={`${id}-title`}>Container network</CardTitle>
      <QueryStatus query={query} />
      {query.data && !cn && (
        <p className="text-sm text-muted-foreground">
          No container network yet: the emulator creates one when a Managed container, such as a
          Cloud SQL instance or a GKE node, first uses this subnetwork.
        </p>
      )}
      {cn && (
        <>
          <DetailList
            label="Container network details"
            rows={[
              ['Container network', <Mono key="b">{cn.bridge}</Mono>],
              ['Declared range', <Mono key="d">{cn.declaredRange}</Mono>],
              [
                'Fallback range',
                cn.fallbackRange ? (
                  <span key="f" className="flex flex-wrap items-center gap-2">
                    <Mono>{cn.fallbackRange}</Mono>
                    <FallbackBadge cn={cn} />
                  </span>
                ) : (
                  'None: on the Declared range'
                ),
              ],
              ['Gateway address', gateway && <Mono key="g">{gateway}</Mono>],
              [
                'Host address',
                cn.hostAddress ? <Mono key="h">{cn.hostAddress}</Mono> : 'Not known yet',
              ],
            ]}
          />
          {cn.fallbackRange && (
            <p role="note" aria-label="Fallback range" className="text-sm text-warning">
              The Declared range {cn.declaredRange} overlapped another network on the host, so the
              container network uses the Fallback range {cn.fallbackRange}.{' '}
              {cn.translated
                ? 'It is translated: Managed containers still hold their Declared addresses, aliased onto their interfaces, so Declared addresses still work.'
                : 'It is not translated: Managed containers hold and report Fallback-range addresses, and Fallback-range addresses are what clients must use.'}
            </p>
          )}
          {apart && (
            <p role="note" aria-label="Host address" className="text-sm text-muted-foreground">
              Workloads reach the host at its Host address {cn.hostAddress}, not at the Gateway
              address {gateway}.
            </p>
          )}
          {cn.containers.length === 0 ? (
            <p className="text-sm text-muted-foreground">No Managed container is attached.</p>
          ) : (
            <ContainerTable containers={cn.containers} />
          )}
        </>
      )}
    </Card>
  )
}

function ContainerTable({ containers }: { containers: Container[] }) {
  return (
    <Table aria-label="Managed containers">
      <TableHeader>
        <TableRow>
          <TableHead>Container</TableHead>
          <TableHead>Service</TableHead>
          <TableHead>Role</TableHead>
          <TableHead>Declared address</TableHead>
          <TableHead>Address held</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {containers.map((c) => (
          <TableRow key={c.name} data-testid="managed-container">
            <TableCell className="font-mono break-all">{c.name}</TableCell>
            <TableCell className="font-mono">{c.service}</TableCell>
            <TableCell>{role(c)}</TableCell>
            <TableCell className="font-mono">{c.declaredAddress || '—'}</TableCell>
            <TableCell className="font-mono">{c.address}</TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}

/**
 * ContainerNetworkSummary is a network's VPC container networks, one row
 * per subnetwork or private services access allocation.
 */
export function ContainerNetworkSummary({
  project,
  network,
}: {
  project: string
  network: string
}) {
  const id = useId()
  const query = useQuery(containerNetworksQuery(project, network))
  return (
    <Card aria-labelledby={`${id}-title`}>
      <CardTitle id={`${id}-title`}>Container networks</CardTitle>
      <p className="text-sm text-muted-foreground">
        The emulator backs each subnetwork, once a Managed container uses it, with a container
        network of its own.
      </p>
      <QueryStatus query={query} />
      {query.data && query.data.length === 0 && (
        <p className="text-sm text-muted-foreground">No container networks yet.</p>
      )}
      {query.data && query.data.length > 0 && (
        <Table aria-label="Container networks">
          <TableHeader>
            <TableRow>
              <TableHead>Subnetwork</TableHead>
              <TableHead>Container network</TableHead>
              <TableHead>Range</TableHead>
              <TableHead>Host address</TableHead>
              <TableHead>Managed containers</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {query.data.map((cn) => (
              <TableRow key={cn.bridge} data-testid="container-network">
                <TableCell>
                  {cn.subnetwork ? (
                    <SubnetLink region={cn.region ?? ''} name={lastSeg(cn.subnetwork)} />
                  ) : (
                    <span className="flex flex-col">
                      <AddressLink region="" name={lastSeg(cn.allocation)} />
                      <span className="text-xs text-muted-foreground">Private services access</span>
                    </span>
                  )}
                </TableCell>
                <TableCell className="font-mono break-all">{cn.bridge}</TableCell>
                <TableCell>
                  {cn.fallbackRange ? (
                    <span className="flex flex-wrap items-center gap-2">
                      <span className="font-mono">{cn.fallbackRange}</span>
                      <FallbackBadge cn={cn} />
                      <span className="text-xs text-muted-foreground">
                        for <span className="font-mono">{cn.declaredRange}</span>
                      </span>
                    </span>
                  ) : (
                    <span className="flex flex-col">
                      <span className="font-mono">{cn.declaredRange}</span>
                      <span className="text-xs text-muted-foreground">Declared range</span>
                    </span>
                  )}
                </TableCell>
                <TableCell className="font-mono">{cn.hostAddress || '—'}</TableCell>
                <TableCell>
                  {cn.containers.length === 1
                    ? '1 container'
                    : `${cn.containers.length} containers`}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}
