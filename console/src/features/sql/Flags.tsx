import { useQuery } from '@tanstack/react-query'
import { Pencil } from 'lucide-react'
import { useState } from 'react'

import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'

import { flagsQuery, type Flag } from './api'
import { useInstance } from './InstancePage'
import { instancePath } from './SqlLayout'

/** allowed describes the values a flag takes. */
export function allowed(f: Flag): string {
  switch (f.type) {
    case 'BOOLEAN':
      return 'on | off'
    case 'INTEGER':
    case 'FLOAT':
      return `${f.minValue ?? '−∞'} to ${f.maxValue ?? '∞'}`
    default:
      return f.allowedStringValues?.length ? f.allowedStringValues.join(' | ') : 'any text'
  }
}

/**
 * Flags shows the instance's database flags (FR-SQL-003) with what each
 * takes, and the flags its version allows; they are set in the edit form.
 */
export function Flags() {
  const i = useInstance()
  const flags = useQuery(flagsQuery())
  const [text, setText] = useState('')
  const version = i.databaseVersion ?? ''
  const known = (flags.data ?? []).filter((f) => !f.appliesTo || f.appliesTo.includes(version))
  const set = i.settings?.databaseFlags ?? []
  const shown = known.filter((f) => f.name.includes(text.trim()))
  const def = (name: string) => known.find((f) => f.name === name)
  return (
    <div className="flex flex-col gap-6">
      <Card>
        <div className="flex flex-wrap items-center justify-between gap-2">
          <CardTitle>Flags set on this instance</CardTitle>
          <Button variant="outline" size="sm" asChild>
            <Link to={`${instancePath(i.name)}/edit`}>
              <Pencil aria-hidden />
              Edit flags
            </Link>
          </Button>
        </div>
        {set.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            No flags are set: PostgreSQL runs with Cloud SQL&rsquo;s defaults.
          </p>
        ) : (
          <Table aria-label="Database flags">
            <TableHeader>
              <TableRow>
                <TableHead>Flag</TableHead>
                <TableHead>Value</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Allowed values</TableHead>
                <TableHead>Restart</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {set.map((f) => {
                const d = def(f.name)
                return (
                  <TableRow key={f.name} data-testid="flag">
                    <TableCell className="font-mono">{f.name}</TableCell>
                    <TableCell className="font-mono">{f.value}</TableCell>
                    <TableCell>{d?.type ?? '—'}</TableCell>
                    <TableCell className="font-mono">{d ? allowed(d) : '—'}</TableCell>
                    <TableCell>{d?.requiresRestart ? 'Required' : 'No'}</TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        )}
      </Card>
      <Card>
        <CardTitle>Flags {version} allows</CardTitle>
        <label htmlFor="flag-filter" className="flex max-w-sm flex-col gap-1 text-sm">
          Filter
          <Input
            id="flag-filter"
            type="search"
            placeholder="Flag name"
            value={text}
            onChange={(e) => setText(e.target.value)}
          />
        </label>
        <QueryStatus query={flags} />
        {shown.length > 0 && (
          <Table aria-label="Allowed flags">
            <TableHeader>
              <TableRow>
                <TableHead>Flag</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Allowed values</TableHead>
                <TableHead>Restart</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shown.map((f) => (
                <TableRow key={f.name}>
                  <TableCell className="font-mono">{f.name}</TableCell>
                  <TableCell>{f.type}</TableCell>
                  <TableCell className="font-mono break-all">{allowed(f)}</TableCell>
                  <TableCell>{f.requiresRestart ? 'Required' : 'No'}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  )
}
