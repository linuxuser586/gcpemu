import { useQuery } from '@tanstack/react-query'

import { resourceCountsQuery } from '@/api/queries'
import { QueryStatus } from '@/components/QueryStatus'
import { Card, CardHeader, CardTitle } from '@/components/ui/card'
import { serviceName, SERVICES } from '@/lib/services'

const acronyms = new Set(['dns', 'grpc', 'hmac', 'http', 'https', 'ssl', 'tcp', 'tls', 'url'])

/** humanize turns a collection name ("hmacKeys") into words ("HMAC keys"). */
export function humanize(type: string): string {
  const words = type
    .replace(/([a-z])([A-Z])/g, '$1 $2')
    .toLowerCase()
    .split(' ')
    .map((w) => (acronyms.has(w) ? w.toUpperCase() : w))
    .join(' ')
  return words.charAt(0).toUpperCase() + words.slice(1)
}

export function CountList({ counts }: { counts: Record<string, number> }) {
  const types = Object.keys(counts).sort()
  return (
    <dl className="grid grid-cols-[1fr_auto] gap-x-4 gap-y-1 text-sm">
      {types.map((t) => (
        <div key={t} className="contents">
          <dt className="text-muted-foreground">{humanize(t)}</dt>
          <dd className="text-right font-mono tabular-nums">{counts[t]}</dd>
        </div>
      ))}
    </dl>
  )
}

export function ResourceCountsCard() {
  const query = useQuery(resourceCountsQuery())
  const order = SERVICES.map((s) => s.id)
  const ids = Object.keys(query.data ?? {}).sort((a, b) => order.indexOf(a) - order.indexOf(b))
  return (
    <Card aria-labelledby="counts-title">
      <CardHeader>
        <CardTitle id="counts-title">Resources</CardTitle>
      </CardHeader>
      <QueryStatus query={query} />
      {query.data && (
        <div className="grid gap-x-8 gap-y-4 sm:grid-cols-2">
          {ids.map((id) => (
            <section key={id} aria-label={serviceName(id)}>
              <h3 className="mb-1 text-sm font-medium">{serviceName(id)}</h3>
              <CountList counts={query.data[id] ?? {}} />
            </section>
          ))}
        </div>
      )}
    </Card>
  )
}
