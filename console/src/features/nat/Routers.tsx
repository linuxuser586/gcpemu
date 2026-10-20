import { useQuery } from '@tanstack/react-query'
import { Plus } from 'lucide-react'
import { Fragment, useState } from 'react'

import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
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
import { subnetsQuery } from '../vpc/api'
import { SubnetList } from '../vpc/VpcLayout'
import { coverage, routersQuery } from './api'
import { natPage, NatTabs, routerPage } from './NatLayout'
import { OfflineSink } from './OfflineSink'

/**
 * Routers is the chosen Project's Cloud Routers, each with its NAT
 * gateways and the subnetworks they cover (?q= filters by router or NAT
 * name), and the offline sink.
 */
export function Routers() {
  const [view, setView] = useViewState()
  const project = view.project ?? ''
  const [q, setQ] = useState(view.q ?? '')
  const routers = useQuery(routersQuery(project))
  const subnets = useQuery(subnetsQuery(project))
  const all = [...(routers.data ?? [])].sort(
    (a, b) => a.name.localeCompare(b.name) || lastSeg(a.region).localeCompare(lastSeg(b.region)),
  )
  const shown = all.filter((r) => r.name.includes(q) || r.nats?.some((n) => n.name.includes(q)))

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Cloud NAT</h1>
          <p className="text-sm text-muted-foreground">
            Cloud Routers and NAT gateways of Project <span className="font-mono">{project}</span>
          </p>
        </div>
        <Button asChild>
          <Link to="/nat/create">
            <Plus aria-hidden />
            Create router
          </Link>
        </Button>
      </div>
      <NatTabs />
      <OfflineSink />
      <Card>
        <form
          role="search"
          aria-label="Filter routers"
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => e.preventDefault()}
        >
          <label htmlFor="nat-filter" className="flex min-w-48 flex-1 flex-col gap-1 text-sm">
            Filter
            <Input
              id="nat-filter"
              type="search"
              placeholder="Router or NAT name"
              value={q}
              onChange={(e) => {
                setQ(e.target.value)
                setView({ q: e.target.value })
              }}
            />
          </label>
        </form>
        <QueryStatus query={routers} />
        {routers.data && all.length === 0 && (
          <p className="text-sm text-muted-foreground">
            No Cloud Routers in this Project yet: create one, then add a NAT gateway to it to let
            workloads on private subnetworks reach the internet.
          </p>
        )}
        {all.length > 0 && shown.length === 0 && (
          <p className="text-sm text-muted-foreground">No routers match the filter.</p>
        )}
        {shown.length > 0 && (
          <Table aria-label="Routers">
            <TableHeader>
              <TableRow>
                <TableHead>Router</TableHead>
                <TableHead>Region</TableHead>
                <TableHead>Network</TableHead>
                <TableHead>NAT gateways</TableHead>
                <TableHead>Covered subnetworks</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((r) => {
                const ref = { region: lastSeg(r.region), router: r.name }
                return (
                  <TableRow key={r.selfLink ?? r.name} data-testid="router">
                    <TableCell>
                      <Link
                        className="font-mono text-primary underline-offset-4 hover:underline"
                        to={routerPage(ref)}
                      >
                        {r.name}
                      </Link>
                    </TableCell>
                    <TableCell className="font-mono">{ref.region}</TableCell>
                    <TableCell className="font-mono">{lastSeg(r.network)}</TableCell>
                    <TableCell>
                      {r.nats?.length ? (
                        <span className="flex flex-wrap gap-x-2">
                          {r.nats.map((n) => (
                            <Link
                              key={n.name}
                              className="font-mono text-primary underline-offset-4 hover:underline"
                              to={natPage(ref, n.name)}
                            >
                              {n.name}
                            </Link>
                          ))}
                        </span>
                      ) : (
                        <span className="text-muted-foreground">None</span>
                      )}
                    </TableCell>
                    <TableCell>
                      {subnets.data
                        ? (r.nats ?? []).map((n, i) => (
                            <Fragment key={n.name}>
                              {i > 0 && <br />}
                              <SubnetList
                                subnets={coverage(r, n, subnets.data).map((c) => c.subnetwork)}
                              />
                            </Fragment>
                          ))
                        : null}
                    </TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  )
}
