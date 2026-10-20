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

import { byPriority, firewallsQuery, priorityOf, type Firewall } from './api'
import { formatRules } from './form'
import { FirewallLink, NetworkLink } from './links'
import { RecordedNotice } from './RecordedNotice'
import { VpcHeader } from './VpcLayout'

/** action is whether a rule allows or denies its protocols and ports. */
export const action = (f: Firewall) => (f.denied?.length ? 'Deny' : 'Allow')

/** rules are a rule's protocols and ports, as gcloud's --rules writes them. */
export const rules = (f: Firewall) => formatRules(f.denied?.length ? f.denied : f.allowed)

/** targets says which VM instances a rule applies to. */
export function targets(f: Firewall) {
  if (f.targetTags?.length) return `Tags: ${f.targetTags.join(', ')}`
  if (f.targetServiceAccounts?.length)
    return `Service accounts: ${f.targetServiceAccounts.join(', ')}`
  return 'All VM instances'
}

/** ranges are where a rule's traffic comes from (ingress) or goes to (egress). */
export function ranges(f: Firewall) {
  if (f.direction === 'EGRESS') return (f.destinationRanges ?? []).join(', ')
  return [
    ...(f.sourceRanges ?? []),
    ...(f.sourceTags ?? []).map((t) => `tag ${t}`),
    ...(f.sourceServiceAccounts ?? []),
  ].join(', ')
}

/** Firewalls is the chosen Project's firewall rules. */
export function Firewalls() {
  const [view] = useViewState()
  const query = useQuery(firewallsQuery(view.project ?? ''))
  const all = [...(query.data ?? [])].sort(byPriority)
  return (
    <div className="flex flex-col gap-6">
      <VpcHeader
        action={
          <Button asChild>
            <Link to="/compute/create-firewall">
              <Plus aria-hidden />
              Create firewall rule
            </Link>
          </Button>
        }
      />
      <RecordedNotice />
      <Card>
        <QueryStatus query={query} />
        {query.data && all.length === 0 && (
          <p className="text-sm text-muted-foreground">No firewall rules in this Project yet.</p>
        )}
        {all.length > 0 && <FirewallTable firewalls={all} network />}
      </Card>
    </div>
  )
}

/** FirewallTable lists firewall rules, with their network unless one owns them all. */
export function FirewallTable({
  firewalls,
  network,
}: {
  firewalls: Firewall[]
  network?: boolean
}) {
  return (
    <Table aria-label="Firewall rules">
      <TableHeader>
        <TableRow>
          <TableHead>Name</TableHead>
          {network && <TableHead>Network</TableHead>}
          <TableHead>Direction</TableHead>
          <TableHead className="text-right">Priority</TableHead>
          <TableHead>Action</TableHead>
          <TableHead>Protocols and ports</TableHead>
          <TableHead>Targets</TableHead>
          <TableHead>Ranges</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {firewalls.map((f) => (
          <TableRow key={f.selfLink ?? f.name} data-testid="firewall">
            <TableCell>
              <FirewallLink name={f.name} />
              {f.disabled && <span className="block text-xs text-muted-foreground">Disabled</span>}
            </TableCell>
            {network && (
              <TableCell>
                <NetworkLink link={f.network} />
              </TableCell>
            )}
            <TableCell className="font-mono text-xs">{f.direction ?? 'INGRESS'}</TableCell>
            <TableCell className="text-right font-mono">{priorityOf(f)}</TableCell>
            <TableCell>{action(f)}</TableCell>
            <TableCell className="font-mono">{rules(f)}</TableCell>
            <TableCell>{targets(f)}</TableCell>
            <TableCell className="font-mono">{ranges(f)}</TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}
