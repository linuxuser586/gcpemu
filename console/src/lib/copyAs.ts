// Copy as (FR-UI-015): a resource written out as the gcloud command, the
// REST request (as curl) and the OpenTofu resource block that would create
// it as it is. Each Service view builds a Recipe from the resource the API
// returned; this module renders the three forms.
//
// The gcloud command and the OpenTofu block leave the Endpoint overrides to
// the client's environment (`gcpemu env`, `gcpemu tofu-provider`); the curl
// request goes straight to the Instance the console was served from.

/**
 * RestCall is one public API request: a method, a gateway path and a
 * JSON body, or a local file sent as it is (an upload).
 */
export interface RestCall {
  method: 'POST' | 'PUT' | 'PATCH'
  /** path is the request's path on the gateway, query string included. */
  path: string
  body?: unknown
  /** file is a local file to send as the body instead, with its Content-Type. */
  file?: { path: string; contentType?: string }
}

/**
 * Recipe is one resource in the three forms. A form a client lacks is
 * left out, and the dialog says so.
 */
export interface Recipe {
  /** gcloud is one command, or several to run in order. */
  gcloud?: string | string[]
  rest?: RestCall | RestCall[]
  tofu?: string
}

// ---- shell ----

const SAFE = /^[A-Za-z0-9_./:=@,%+^-]+$/

/** shQuote quotes s for a POSIX shell, leaving plain words alone. */
export function shQuote(s: string): string {
  return SAFE.test(s) ? s : `'${s.replaceAll("'", `'\\''`)}'`
}

/** A flag's value: true writes the bare flag; undefined, null, false and '' skip it. */
export type FlagValue = string | number | boolean | readonly string[] | undefined | null

/** joinLines writes a command's words one per line after the first, as people do with long ones. */
function joinLines(head: string, rest: string[]): string {
  return rest.length ? [head, ...rest].join(' \\\n    ') : head
}

/**
 * gcloud writes `gcloud <args> --flag=value…`, quoting what the shell
 * would split. Flags keep their order; a list joins with commas.
 */
export function gcloud(args: readonly string[], flags: Record<string, FlagValue> = {}): string {
  const out: string[] = []
  for (const [k, v] of Object.entries(flags)) {
    if (v === undefined || v === null || v === false || v === '') continue
    if (Array.isArray(v) && v.length === 0) continue
    if (v === true) {
      out.push(`--${k}`)
      continue
    }
    const s = Array.isArray(v) ? v.join(',') : String(v)
    out.push(`--${k}=${shQuote(s)}`)
  }
  return joinLines(['gcloud', ...args.map(shQuote)].join(' '), out)
}

/** keyValues writes a map as gcloud's KEY=VALUE,… list. */
export const keyValues = (m?: Record<string, string>): string[] =>
  Object.entries(m ?? {}).map(([k, v]) => `${k}=${v}`)

/** curl writes call as a curl command against the gateway at origin. */
export function curl(origin: string, call: RestCall): string {
  const parts = [`-X ${call.method}`]
  if (call.body !== undefined) {
    parts.push(`-H ${shQuote('Content-Type: application/json')}`)
    parts.push(`-d '${JSON.stringify(call.body, null, 2).replaceAll("'", `'\\''`)}'`)
  } else if (call.file) {
    if (call.file.contentType) {
      parts.push(`-H ${shQuote(`Content-Type: ${call.file.contentType}`)}`)
    }
    parts.push(`--data-binary ${shQuote(`@${call.file.path}`)}`)
  }
  return joinLines(`curl ${shQuote(origin + call.path)}`, parts)
}

// ---- HCL ----

/** Expr is an HCL expression written as it is, e.g. jsonencode(…). */
export class Expr {
  constructor(readonly text: string) {}
}

/** Block is a nested block, `name { … }`, as opposed to a map attribute. */
export class Block {
  constructor(readonly body: HclBody) {}
}

export type HclValue =
  | string
  | number
  | boolean
  | Expr
  | Block
  | readonly Block[]
  | readonly HclValue[]
  | { readonly [k: string]: HclValue }
  | undefined
  | null

export type HclBody = Record<string, HclValue>

export const expr = (text: string) => new Expr(text)
export const block = (body: HclBody) => new Block(body)

/** jsonencode writes v as an HCL jsonencode() call over HCL values. */
export const jsonencode = (v: HclValue) => expr(`jsonencode(${value(v, '')})`)

const IDENT = /^[A-Za-z_][A-Za-z0-9_-]*$/

/** hclString writes s as an HCL quoted string. */
export function hclString(s: string): string {
  const esc = s
    .replaceAll('\\', '\\\\')
    .replaceAll('"', '\\"')
    .replaceAll('\n', '\\n')
    .replaceAll('\r', '\\r')
    .replaceAll('\t', '\\t')
    .replaceAll('${', () => '$${')
    .replaceAll('%{', () => '%%{')
  return `"${esc}"`
}

/** tofuName is a resource ID as an OpenTofu resource name. */
export function tofuName(id: string): string {
  const s = id.replaceAll(/[^A-Za-z0-9_-]/g, '_')
  return /^[A-Za-z_]/.test(s) ? s : `_${s}`
}

const isBlocks = (v: HclValue): v is readonly Block[] =>
  Array.isArray(v) && v.length > 0 && v.every((b) => b instanceof Block)

/** empty is a value the block leaves out: unset, or an empty list or map. */
function empty(v: HclValue): boolean {
  if (v === undefined || v === null) return true
  if (Array.isArray(v)) return v.length === 0
  if (typeof v === 'object' && !(v instanceof Expr) && !(v instanceof Block)) {
    return Object.keys(v).length === 0
  }
  return false
}

function value(v: HclValue, indent: string): string {
  if (typeof v === 'string') return hclString(v)
  if (typeof v === 'number' || typeof v === 'boolean') return String(v)
  if (v instanceof Expr) return v.text
  if (v === undefined || v === null || v instanceof Block) return 'null'
  if (Array.isArray(v)) {
    const items = (v as readonly HclValue[]).map((x) => value(x, `${indent}  `))
    const flat = `[${items.join(', ')}]`
    if (flat.length <= 80 && !flat.includes('\n')) return flat
    return `[\n${items.map((x) => `${indent}  ${x},`).join('\n')}\n${indent}]`
  }
  const entries = Object.entries(v as Record<string, HclValue>).filter(([, x]) => !empty(x))
  const key = (k: string) => (IDENT.test(k) ? k : hclString(k))
  const flat = `{ ${entries.map(([k, x]) => `${key(k)} = ${value(x, indent)}`).join(', ')} }`
  if (flat.length <= 60 && !flat.includes('\n')) return flat
  const w = Math.max(...entries.map(([k]) => key(k).length))
  const lines = entries.map(
    ([k, x]) => `${indent}  ${key(k).padEnd(w)} = ${value(x, `${indent}  `)}`,
  )
  return `{\n${lines.join('\n')}\n${indent}}`
}

/**
 * body writes a block's attributes and nested blocks as `tofu fmt` does:
 * the equals signs of adjacent one-line attributes line up, and nested
 * blocks follow the attributes.
 */
function body(b: HclBody, indent: string): string[] {
  const attrs: [string, string][] = []
  const blocks: string[] = []
  for (const [k, v] of Object.entries(b)) {
    if (v instanceof Block) {
      blocks.push(...nested(k, v, indent))
    } else if (isBlocks(v)) {
      for (const x of v) blocks.push(...nested(k, x, indent))
    } else if (!empty(v)) {
      attrs.push([k, value(v, indent)])
    }
  }
  const lines: string[] = []
  let group: [string, string][] = []
  const flush = () => {
    const w = Math.max(0, ...group.map(([k]) => k.length))
    for (const [k, v] of group) lines.push(`${indent}${k.padEnd(w)} = ${v}`)
    group = []
  }
  for (const [k, v] of attrs) {
    group.push([k, v])
    if (v.includes('\n')) flush()
  }
  flush()
  return [...lines, ...blocks]
}

function nested(name: string, b: Block, indent: string): string[] {
  const inner = body(b.body, `${indent}  `)
  if (!inner.length) return [`${indent}${name} {}`]
  return [`${indent}${name} {`, ...inner, `${indent}}`]
}

/** resource writes `resource "type" "name" { … }`. */
export function resource(type: string, name: string, attrs: HclBody): string {
  return [
    `resource ${hclString(type)} ${hclString(tofuName(name))} {`,
    ...body(attrs, '  '),
    '}',
  ].join('\n')
}

// ---- resources ----

/** omit is obj without the named fields, e.g. a resource's output-only ones. */
export function omit<T extends object>(obj: T, keys: readonly string[]): Partial<T> {
  const out: Record<string, unknown> = {}
  for (const [k, v] of Object.entries(obj)) if (!keys.includes(k)) out[k] = v
  return out as Partial<T>
}

/** last is the ID at the end of a resource name or URL. */
export const last = (name: string) => name.slice(name.lastIndexOf('/') + 1)
