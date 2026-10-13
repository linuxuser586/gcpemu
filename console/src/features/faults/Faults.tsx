import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Plus, Trash2 } from 'lucide-react'
import { useState } from 'react'
import { useSearchParams } from 'react-router'

import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import { Switch } from '@/components/ui/switch'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { serviceName, SERVICES } from '@/lib/services'
import { toast } from '@/lib/toast'
import { useViewState } from '@/lib/viewState'

import {
  canFire,
  clearFaults,
  faultsQuery,
  removeFault,
  setFaultDisabled,
  type FaultRule,
} from './api'

/** effect describes what a rule does to a request it fires on. */
export function effect(r: FaultRule): string {
  const parts: string[] = []
  if (r.latency && r.latency !== '0s') parts.push(`Delay ${r.latency}`)
  if (r.drop) parts.push('Drop the connection')
  else if (r.code) parts.push(r.httpStatus ? `${r.code} (HTTP ${r.httpStatus})` : r.code)
  return parts.join(', then ')
}

/** when describes how often a matching request fires a rule. */
export function when(r: FaultRule): string {
  const p = r.probability ? `${+(r.probability * 100).toFixed(2)}% of matches` : 'Every match'
  return r.count ? `${p}, at most ${r.count} ${r.count === 1 ? 'time' : 'times'}` : p
}

/** matches: the rule's ID, matchers or code contain text. */
export function matches(r: FaultRule, text: string): boolean {
  const t = text.trim().toLowerCase()
  if (!t) return true
  return [r.id, r.method, r.resource, r.code].some((v) => v?.toLowerCase().includes(t))
}

function Pattern({ re, any }: { re?: string; any: string }) {
  return re ? (
    <span className="font-mono break-all">{re}</span>
  ) : (
    <span className="text-muted-foreground">{any}</span>
  )
}

function FaultRow({ r }: { r: FaultRule }) {
  const qc = useQueryClient()
  const id = r.id ?? ''
  const toggle = useMutation({
    mutationFn: (disabled: boolean) => setFaultDisabled(id, disabled),
    onSuccess: (rule) => {
      void qc.invalidateQueries({ queryKey: faultsQuery().queryKey })
      toast.success(`${rule.disabled ? 'Disabled' : 'Enabled'} rule ${id}.`)
    },
  })
  const remove = useMutation({
    mutationFn: () => removeFault(id),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: faultsQuery().queryKey })
      toast.success(`Removed rule ${id}.`)
    },
  })
  const on = toggle.isPending ? !toggle.variables : !r.disabled
  const fired = r.fired ?? 0
  const spent = !!r.count && fired >= r.count
  return (
    <TableRow data-testid="fault" data-id={id} data-enabled={on}>
      <TableCell>
        <Switch
          checked={on}
          aria-label={`Rule ${id} enabled`}
          disabled={toggle.isPending}
          onClick={() => toggle.mutate(on)}
        />
      </TableCell>
      <TableCell className="font-mono">{id}</TableCell>
      <TableCell title={r.service}>
        {r.service ? serviceName(r.service) : <span className="text-muted-foreground">Any</span>}
      </TableCell>
      <TableCell className="max-w-xs">
        <Pattern re={r.method} any="Any method" />
        {r.resource && (
          <>
            <br />
            <Pattern re={r.resource} any="" />
          </>
        )}
      </TableCell>
      <TableCell>{effect(r)}</TableCell>
      <TableCell className="text-sm">{when(r)}</TableCell>
      <TableCell className="text-right whitespace-nowrap">
        {spent && (
          <Badge variant="outline" className="mr-2">
            Used up
          </Badge>
        )}
        <span className="font-mono tabular-nums" aria-label={`${fired} hits`}>
          {r.count ? `${fired} / ${r.count}` : fired}
        </span>
      </TableCell>
      <TableCell className="w-8">
        <ConfirmDialog
          trigger={
            <Button variant="ghost" size="icon" aria-label={`Remove rule ${id}`}>
              <Trash2 />
            </Button>
          }
          title={`Remove rule ${id}?`}
          description="Matching requests are no longer affected by it, and its hit count is lost."
          confirmLabel="Remove"
          onConfirm={() => remove.mutateAsync()}
        />
      </TableCell>
    </TableRow>
  )
}

/**
 * Faults lists the Instance's Fault rules (FR-UI-016) with their hit
 * counts, and enables, disables, adds and removes them. ?service= and ?q=
 * filter the list; ?project= is kept but does not apply, as rules are
 * the Instance's.
 */
export function Faults() {
  const [view, setView] = useViewState()
  // The input owns the text while typing: a URL update is a navigation,
  // which lands after the next keystroke.
  const [text, setText] = useState(view.q ?? '')
  const [search, setSearch] = useSearchParams()
  const qc = useQueryClient()
  const query = useQuery(faultsQuery())
  const clear = useMutation({
    mutationFn: clearFaults,
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: faultsQuery().queryKey })
      toast.success('Removed every fault rule.')
    },
  })
  const service = search.get('service') ?? ''
  const all = query.data ?? []
  const order = SERVICES.map((s) => s.id)
  const services = [...new Set(all.map((r) => r.service ?? ''))]
    .filter(Boolean)
    .sort((a, b) => order.indexOf(a) - order.indexOf(b))
  const serviceOptions = service && !services.includes(service) ? [...services, service] : services
  // A rule for every Service applies to the one filtered on too.
  const shown = all.filter(
    (r) => (!service || !r.service || r.service === service) && matches(r, text),
  )
  const active = all.filter(canFire).length

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Fault rules</h1>
          <p className="text-sm text-muted-foreground">
            Rules that make matching requests fail, drop or slow down on purpose, checked in order;
            the first that fires applies. They cover every Project of this Instance.
          </p>
        </div>
        <div className="flex gap-2">
          {all.length > 0 && (
            <ConfirmDialog
              trigger={<Button variant="outline">Remove all</Button>}
              title="Remove every fault rule?"
              description={`All ${all.length} rules and their hit counts are removed.`}
              confirmLabel="Remove all"
              onConfirm={() => clear.mutateAsync()}
            />
          )}
          <Button asChild>
            <Link to="/faults/create">
              <Plus aria-hidden />
              Add rule
            </Link>
          </Button>
        </div>
      </div>
      <Card>
        <form
          role="search"
          aria-label="Filter fault rules"
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => e.preventDefault()}
        >
          <label htmlFor="faults-service" className="flex flex-col gap-1 text-sm">
            Service
            <NativeSelect
              id="faults-service"
              value={service}
              onChange={(e) =>
                setSearch((prev) => {
                  const next = new URLSearchParams(prev)
                  if (e.target.value) next.set('service', e.target.value)
                  else next.delete('service')
                  return next
                })
              }
            >
              <option value="">All Services</option>
              {serviceOptions.map((id) => (
                <option key={id} value={id}>
                  {serviceName(id)}
                </option>
              ))}
            </NativeSelect>
          </label>
          <label htmlFor="faults-filter" className="flex min-w-48 flex-1 flex-col gap-1 text-sm">
            Filter
            <Input
              id="faults-filter"
              type="search"
              placeholder="ID, method, resource or code"
              value={text}
              onChange={(e) => {
                setText(e.target.value)
                setView({ q: e.target.value })
              }}
            />
          </label>
        </form>
        <QueryStatus query={query} />
        {query.data && (
          <p className="text-sm text-muted-foreground" role="status">
            {all.length === 0
              ? 'No fault rules: every request is handled normally.'
              : `${active} of ${all.length} ${all.length === 1 ? 'rule' : 'rules'} can fire.`}
          </p>
        )}
        {query.data && all.length > 0 && shown.length === 0 && (
          <p className="text-sm text-muted-foreground">No rules match the filters.</p>
        )}
        {shown.length > 0 && (
          <Table aria-label="Fault rules">
            <TableHeader>
              <TableRow>
                <TableHead>Enabled</TableHead>
                <TableHead>ID</TableHead>
                <TableHead>Service</TableHead>
                <TableHead>Method and resource</TableHead>
                <TableHead>Effect</TableHead>
                <TableHead>When</TableHead>
                <TableHead className="text-right">Hits</TableHead>
                <TableHead>
                  <span className="sr-only">Actions</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((r) => (
                <FaultRow key={r.id} r={r} />
              ))}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  )
}
