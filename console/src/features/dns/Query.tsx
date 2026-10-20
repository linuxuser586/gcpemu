import { useQuery } from '@tanstack/react-query'
import { RefreshCw, Search } from 'lucide-react'
import { useState } from 'react'

import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { DetailList, Mono } from '@/components/resource/DetailList'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { useLatestSearchParams } from '@/lib/latestSearchParams'
import { useViewState } from '@/lib/viewState'

import { dnsQuery, networkLabel, TYPES, zoneNetworks, zonesQuery, type DnsRecord } from './api'
import { DnsNav, zonePath } from './DnsLayout'

// The query box (SRS 4.8.3): resolves a name against the Instance's DNS
// server, from the host or from a VPC network, through GET
// /_emu/v1/dns/query. The question is in the URL (?name=, ?type=,
// ?network=), so that an answer can be shared and asked again.

const QUERY_TYPES = [...TYPES.filter((t) => t !== 'SPF'), 'ANY'] as const

/** Section shows one section of the response. */
function Section({ title, records }: { title: string; records: DnsRecord[] }) {
  return (
    <div className="flex flex-col gap-2">
      <h3 className="text-sm font-semibold">{title}</h3>
      {records.length === 0 ? (
        <p className="text-sm text-muted-foreground">No records.</p>
      ) : (
        <Table aria-label={title}>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Type</TableHead>
              <TableHead>TTL</TableHead>
              <TableHead>Data</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {records.map((r, i) => (
              <TableRow key={i} data-testid="record">
                <TableCell className="font-mono">{r.name}</TableCell>
                <TableCell>{r.type}</TableCell>
                <TableCell>{r.ttl}</TableCell>
                <TableCell className="font-mono break-all">{r.data}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </div>
  )
}

/** zoneLink links the answering zone, in its own Project. */
function ZoneLink({ zone }: { zone: string }) {
  const m = /^projects\/([^/]+)\/managedZones\/([^/]+)$/.exec(zone)
  if (!m) return <Mono>{zone}</Mono>
  return (
    <Link
      className="font-mono text-primary underline-offset-4 hover:underline"
      to={`${zonePath(m[2]!)}?${new URLSearchParams({ project: m[1]! })}`}
    >
      {zone}
    </Link>
  )
}

export function Query() {
  const [view] = useViewState()
  const project = view.project ?? ''
  const [search, setSearch] = useLatestSearchParams()
  const asked = {
    name: search.get('name') ?? '',
    type: search.get('type') || 'A',
    network: search.get('network') ?? '',
  }
  const [name, setName] = useState(asked.name)
  const [type, setType] = useState(asked.type)
  const [network, setNetwork] = useState(asked.network)
  const zones = useQuery({ ...zonesQuery(project), enabled: !!project })
  const result = useQuery({ ...dnsQuery(asked), enabled: !!asked.name })

  // Networks a private zone of the Project is bound to; any other network
  // sees what the host sees.
  const networks = [...new Set((zones.data ?? []).flatMap(zoneNetworks))].sort()
  if (asked.network && !networks.includes(asked.network)) networks.push(asked.network)

  const ask = (e: React.FormEvent) => {
    e.preventDefault()
    if (!name.trim()) return
    setSearch((prev) => {
      const next = new URLSearchParams(prev)
      next.set('name', name.trim())
      next.set('type', type)
      if (network) next.set('network', network)
      else next.delete('network')
      return next
    })
  }
  const r = result.data

  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-2xl font-semibold">Cloud DNS</h1>
        <p className="text-sm text-muted-foreground">
          Resolve a name against the Instance&apos;s DNS server, as a client on the host or on a VPC
          network would.
        </p>
      </div>
      <DnsNav />
      <Card>
        <form
          aria-label="DNS query"
          className="flex flex-wrap items-end gap-3"
          onSubmit={ask}
          noValidate
        >
          <label htmlFor="dns-query-name" className="flex min-w-64 flex-1 flex-col gap-1 text-sm">
            Name
            <Input
              id="dns-query-name"
              spellCheck={false}
              autoComplete="off"
              placeholder="www.example.com."
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </label>
          <label htmlFor="dns-query-type" className="flex flex-col gap-1 text-sm">
            Type
            <NativeSelect
              id="dns-query-type"
              className="w-28"
              value={type}
              onChange={(e) => setType(e.target.value)}
            >
              {QUERY_TYPES.map((t) => (
                <option key={t} value={t}>
                  {t}
                </option>
              ))}
            </NativeSelect>
          </label>
          <label htmlFor="dns-query-network" className="flex flex-col gap-1 text-sm">
            From
            <NativeSelect
              id="dns-query-network"
              className="w-64"
              value={network}
              onChange={(e) => setNetwork(e.target.value)}
            >
              <option value="">Host (public zones)</option>
              {networks.map((n) => (
                <option key={n} value={n}>
                  Network {networkLabel(n, project)}
                </option>
              ))}
            </NativeSelect>
          </label>
          <Button type="submit" disabled={!name.trim()}>
            <Search aria-hidden />
            Resolve
          </Button>
        </form>
        {asked.name && <QueryStatus query={result} />}
      </Card>
      {r && (
        <Card aria-label="DNS response">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <CardTitle>
              <span className="font-mono">{r.name}</span> {r.type}
            </CardTitle>
            <Button
              variant="outline"
              size="sm"
              disabled={result.isFetching}
              onClick={() => void result.refetch()}
            >
              <RefreshCw aria-hidden />
              Ask again
            </Button>
          </div>
          <DetailList
            label="Response"
            rows={[
              [
                'Status',
                <Badge key="s" variant={r.rcode === 'NOERROR' ? 'success' : 'destructive'}>
                  {r.rcode}
                </Badge>,
              ],
              ['Answered by', r.zone ? <ZoneLink zone={r.zone} /> : 'Host resolver (forwarded)'],
              ['Authoritative', r.authoritative ? 'Yes' : 'No'],
              ['From', r.network ? <Mono>{r.network}</Mono> : 'Host'],
              ['Server', <Mono key="srv">{r.server}</Mono>],
              ['Time', `${r.latencyMs.toFixed(1)} ms`],
            ]}
          />
          {r.rcode === 'REFUSED' && !r.zone && (
            <p className="text-sm text-muted-foreground">
              No zone visible {r.network ? 'on this network' : 'from the host'} holds this name, and
              forwarding to the host resolver is off (
              <span className="font-mono">--dns-no-forward</span>).
            </p>
          )}
          <Section title="Answer" records={r.answer} />
          {r.authority.length > 0 && <Section title="Authority" records={r.authority} />}
          {r.additional.length > 0 && <Section title="Additional" records={r.additional} />}
        </Card>
      )}
    </div>
  )
}
