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
import { subnetsQuery, type Subnetwork } from './api'
import { NetworkLink, SubnetLink, VpcHeader } from './VpcLayout'

/** bySubnet orders subnetworks by name, then region. */
export const bySubnet = (a: Subnetwork, b: Subnetwork) =>
  a.name.localeCompare(b.name) || lastSeg(a.region).localeCompare(lastSeg(b.region))

/** Subnetworks is the chosen Project's subnetworks in every region. */
export function Subnetworks() {
  const [view] = useViewState()
  const query = useQuery(subnetsQuery(view.project ?? ''))
  const all = [...(query.data ?? [])].sort(bySubnet)
  return (
    <div className="flex flex-col gap-6">
      <VpcHeader
        action={
          <Button asChild>
            <Link to="/compute/create-subnetwork">
              <Plus aria-hidden />
              Create subnetwork
            </Link>
          </Button>
        }
      />
      <Card>
        <QueryStatus query={query} />
        {query.data && all.length === 0 && (
          <p className="text-sm text-muted-foreground">
            No subnetworks in this Project yet: create a network first, then add subnetworks to it.
          </p>
        )}
        {all.length > 0 && <SubnetTable subnets={all} network />}
      </Card>
    </div>
  )
}

/** SubnetTable lists subnetworks, with their network unless one owns them all. */
export function SubnetTable({ subnets, network }: { subnets: Subnetwork[]; network?: boolean }) {
  return (
    <Table aria-label="Subnetworks">
      <TableHeader>
        <TableRow>
          <TableHead>Name</TableHead>
          <TableHead>Region</TableHead>
          {network && <TableHead>Network</TableHead>}
          <TableHead>Primary range</TableHead>
          <TableHead>Secondary ranges</TableHead>
          <TableHead>Private Google Access</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {subnets.map((s) => {
          const region = lastSeg(s.region)
          return (
            <TableRow key={s.selfLink ?? `${region}/${s.name}`} data-testid="subnetwork">
              <TableCell>
                <SubnetLink region={region} name={s.name} />
              </TableCell>
              <TableCell className="font-mono">{region}</TableCell>
              {network && (
                <TableCell>
                  <NetworkLink link={s.network} />
                </TableCell>
              )}
              <TableCell className="font-mono">{s.ipCidrRange}</TableCell>
              <TableCell>
                <span className="flex flex-wrap gap-x-4">
                  {(s.secondaryIpRanges ?? []).map((r) => (
                    <span key={r.rangeName}>
                      <span className="text-xs text-muted-foreground">{r.rangeName}</span>{' '}
                      <span className="font-mono">{r.ipCidrRange}</span>
                    </span>
                  ))}
                </span>
              </TableCell>
              <TableCell>{s.privateIpGoogleAccess ? 'On' : 'Off'}</TableCell>
            </TableRow>
          )
        })}
      </TableBody>
    </Table>
  )
}
