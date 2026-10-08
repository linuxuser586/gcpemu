// FR-UI-020: the console adds at most 3 MB, gzip-compressed, to the
// binary. Measures every file `pnpm build` wrote to dist/.
import { readdirSync, readFileSync } from 'node:fs'
import path from 'node:path'
import { gzipSync } from 'node:zlib'

const LIMIT = 3 * 1024 * 1024
const dist = path.resolve(import.meta.dirname, '../dist')

let total = 0
for (const entry of readdirSync(dist, { recursive: true, withFileTypes: true })) {
  if (!entry.isFile()) continue
  const file = path.join(entry.parentPath, entry.name)
  const size = gzipSync(readFileSync(file), { level: 9 }).length
  total += size
  console.log(`${(size / 1024).toFixed(1).padStart(9)} KiB  ${path.relative(dist, file)}`)
}
const kib = (total / 1024).toFixed(1)
if (total > LIMIT) {
  console.error(`console bundle is ${kib} KiB gzip-compressed, over the 3 MiB budget (FR-UI-020)`)
  process.exit(1)
}
console.log(`console bundle: ${kib} KiB gzip-compressed (budget 3072 KiB)`)
