import type { ReactNode } from 'react'

export type DetailRow = [label: string, value: ReactNode]

/** DetailList shows a resource's fields as a term list, skipping empty ones. */
export function DetailList({ rows, label }: { rows: DetailRow[]; label?: string }) {
  return (
    <dl aria-label={label} className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-1.5 text-sm">
      {rows
        .filter(([, v]) => v !== undefined && v !== null && v !== '' && v !== false)
        .map(([k, v]) => (
          <div key={k} className="contents">
            <dt className="text-muted-foreground">{k}</dt>
            <dd className="min-w-0 break-words">{v}</dd>
          </div>
        ))}
    </dl>
  )
}

/** Mono renders an identifier in the monospace face. */
export function Mono({ children }: { children: ReactNode }) {
  return <span className="font-mono break-all">{children}</span>
}

/** JsonView shows a resource exactly as the API returned it. */
export function JsonView({ value, label }: { value: unknown; label: string }) {
  return (
    <pre
      aria-label={label}
      className="max-h-[32rem] overflow-auto rounded-md bg-muted p-3 font-mono text-xs"
    >
      {JSON.stringify(value, null, 2)}
    </pre>
  )
}
