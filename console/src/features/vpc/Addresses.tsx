import { useQuery } from '@tanstack/react-query'
import { Plus } from 'lucide-react'

import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
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
import { useViewState } from '@/lib/viewState'

import { lastSeg } from '../lb/api'
import { allAddressesQuery, type Address } from './api'
import { AddressLink, AddressUsers } from './links'
import { VpcHeader } from './VpcLayout'

/** ipOf is an address's IP, with its prefix length for a range. */
export const ipOf = (a: Address) =>
  a.address ? `${a.address}${a.prefixLength ? `/${a.prefixLength}` : ''}` : '—'

/** scopeLabel is an address's region, or Global. */
export const scopeLabel = (a: Address) => lastSeg(a.region) || 'Global'

/** Addresses is the chosen Project's regional and global addresses. */
export function Addresses() {
  const [view] = useViewState()
  const query = useQuery(allAddressesQuery(view.project ?? ''))
  const all = [...(query.data ?? [])].sort(
    (a, b) => a.name.localeCompare(b.name) || scopeLabel(a).localeCompare(scopeLabel(b)),
  )
  return (
    <div className="flex flex-col gap-6">
      <VpcHeader
        action={
          <Button asChild>
            <Link to="/compute/create-address">
              <Plus aria-hidden />
              Reserve address
            </Link>
          </Button>
        }
      />
      <Card>
        <QueryStatus query={query} />
        {query.data && all.length === 0 && (
          <p className="text-sm text-muted-foreground">
            No reserved addresses in this Project yet.
          </p>
        )}
        {all.length > 0 && <AddressTable addresses={all} />}
      </Card>
    </div>
  )
}

/** AddressTable lists addresses with their scope, type, purpose, status and users. */
export function AddressTable({ addresses }: { addresses: Address[] }) {
  return (
    <Table aria-label="IP addresses">
      <TableHeader>
        <TableRow>
          <TableHead>Name</TableHead>
          <TableHead>Scope</TableHead>
          <TableHead>Address</TableHead>
          <TableHead>Type</TableHead>
          <TableHead>Purpose</TableHead>
          <TableHead>Status</TableHead>
          <TableHead>In use by</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {addresses.map((a) => (
          <TableRow key={a.selfLink ?? a.name} data-testid="address">
            <TableCell>
              <AddressLink region={lastSeg(a.region)} name={a.name} />
            </TableCell>
            <TableCell>{scopeLabel(a)}</TableCell>
            <TableCell className="font-mono">{ipOf(a)}</TableCell>
            <TableCell className="font-mono">{a.addressType ?? 'EXTERNAL'}</TableCell>
            <TableCell className="font-mono">{a.purpose ?? '—'}</TableCell>
            <TableCell className="font-mono">{a.status}</TableCell>
            <TableCell>
              <AddressUsers users={a.users} />
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}
