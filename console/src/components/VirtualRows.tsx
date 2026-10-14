import { useEffect, useRef, useState, type ReactNode } from 'react'

/**
 * VirtualRows renders only the rows in view of a fixed-height scroller,
 * so that a listing of 100,000 objects stays responsive (FR-UI-022).
 * Rows have one fixed height. onEnd fires when the last rows come into
 * view, to load the next page.
 */
export function VirtualRows<T>({
  rows,
  rowHeight,
  height,
  render,
  onEnd,
  label,
  header,
}: {
  rows: readonly T[]
  rowHeight: number
  height: number
  render: (row: T, index: number) => ReactNode
  onEnd?: () => void
  label: string
  header: ReactNode
}) {
  const ref = useRef<HTMLDivElement>(null)
  const [top, setTop] = useState(0)
  const overscan = 10
  const first = Math.max(0, Math.floor(top / rowHeight) - overscan)
  const last = Math.min(rows.length, Math.ceil((top + height) / rowHeight) + overscan)
  const nearEnd = last >= rows.length - overscan

  useEffect(() => {
    if (nearEnd) onEnd?.()
  }, [nearEnd, onEnd, rows.length])

  return (
    <div
      ref={ref}
      role="table"
      aria-label={label}
      aria-rowcount={rows.length + 1}
      className="overflow-auto rounded-md border"
      style={{ maxHeight: height }}
      onScroll={(e) => setTop(e.currentTarget.scrollTop)}
    >
      <div role="rowgroup" className="sticky top-0 z-10 border-b bg-card">
        {header}
      </div>
      <div role="rowgroup" style={{ height: rows.length * rowHeight, position: 'relative' }}>
        {rows.slice(first, last).map((row, i) => (
          <div
            key={first + i}
            style={{ position: 'absolute', top: (first + i) * rowHeight, height: rowHeight }}
            className="w-full"
          >
            {render(row, first + i)}
          </div>
        ))}
      </div>
    </div>
  )
}
