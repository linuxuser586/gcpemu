// Formatting shared by the Cloud Storage view.

const UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB']

/** formatBytes renders a size the API gives as a decimal string. */
export function formatBytes(size?: string | number): string {
  if (size === undefined || size === '') return '—'
  let n = Number(size)
  if (!Number.isFinite(n)) return String(size)
  let u = 0
  while (n >= 1024 && u < UNITS.length - 1) {
    n /= 1024
    u++
  }
  return u === 0 ? `${n} B` : `${n.toFixed(n < 10 ? 1 : 0)} ${UNITS[u]}`
}

export const time = (iso?: string) => (iso ? new Date(iso).toLocaleString() : undefined)

/** baseName is the last part of an object name or prefix. */
export function baseName(name: string): string {
  const trimmed = name.endsWith('/') ? name.slice(0, -1) : name
  const i = trimmed.lastIndexOf('/')
  return name.slice(i + 1)
}

/** parentPrefix is the folder an object name or prefix is in. */
export function parentPrefix(name: string): string {
  const trimmed = name.endsWith('/') ? name.slice(0, -1) : name
  const i = trimmed.lastIndexOf('/')
  return i < 0 ? '' : trimmed.slice(0, i + 1)
}

/** prefixCrumbs splits "a/b/c/" into [["a", "a/"], ["b", "a/b/"], ["c", "a/b/c/"]]. */
export function prefixCrumbs(prefix: string): [label: string, prefix: string][] {
  const out: [string, string][] = []
  let acc = ''
  for (const part of prefix.split('/').slice(0, -1)) {
    acc += `${part}/`
    out.push([part, acc])
  }
  return out
}

/** formatSeconds renders a retention period such as "7 days". */
export function formatSeconds(s?: string | number): string | undefined {
  if (s === undefined || s === '') return undefined
  const n = Number(s)
  if (n % 86400 === 0) return `${n / 86400} ${n === 86400 ? 'day' : 'days'}`
  if (n % 3600 === 0) return `${n / 3600} h`
  return `${n} s`
}
