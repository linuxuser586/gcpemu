import type { AclEntry, DatabaseFlag, Flag } from './api'

// Text forms of an instance's list settings, edited one per line as
// gcloud's --database-flags and --authorized-networks take them.

function lines(text: string): string[] {
  return text
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter(Boolean)
}

export const formatFlags = (flags: DatabaseFlag[] = []) =>
  flags.map((f) => `${f.name}=${f.value}`).join('\n')

export function parseFlags(text: string): DatabaseFlag[] {
  // A flag's value may itself hold commas (REPEATED_STRING), so only
  // newlines separate flags.
  return text
    .split('\n')
    .map((s) => s.trim())
    .filter(Boolean)
    .map((e) => {
      const [name = '', ...value] = e.split('=')
      return { name: name.trim(), value: value.join('=').trim() }
    })
}

const FLAG_LINE = /^[a-z_.0-9]+=.*$/

/**
 * flagsError explains the first flag the API would reject, with its
 * message: an unknown flag for the version, a duplicate, or a value out of
 * range. Without the list of flags it checks only the syntax.
 */
export function flagsError(text: string, known?: Flag[]): string | undefined {
  const seen = new Set<string>()
  for (const line of text.split('\n').map((s) => s.trim())) {
    if (!line) continue
    if (!FLAG_LINE.test(line)) return `Invalid flag "${line}": write name=value, one per line.`
  }
  for (const f of parseFlags(text)) {
    if (seen.has(f.name)) return `Invalid request: Duplicate flag: ${f.name}.`
    seen.add(f.name)
    if (!known) continue
    const def = known.find((k) => k.name === f.name)
    if (!def) return `Invalid request: Invalid flag name: ${f.name}.`
    if (!validFlagValue(def, f.value)) {
      return `Invalid request: Invalid value for flag ${f.name}: "${f.value}".`
    }
  }
  return undefined
}

function validFlagValue(def: Flag, v: string): boolean {
  const min = def.minValue === undefined ? -Infinity : Number(def.minValue)
  const max = def.maxValue === undefined ? Infinity : Number(def.maxValue)
  switch (def.type) {
    case 'BOOLEAN':
      return v === 'on' || v === 'off'
    case 'INTEGER':
      return /^-?\d+$/.test(v) && Number(v) >= min && Number(v) <= max
    case 'FLOAT':
      return v !== '' && !Number.isNaN(Number(v)) && Number(v) >= min && Number(v) <= max
    case 'STRING':
      return !def.allowedStringValues?.length || def.allowedStringValues.includes(v)
    case 'REPEATED_STRING':
      return (
        !def.allowedStringValues?.length ||
        v.split(',').every((p) => def.allowedStringValues?.includes(p.trim()))
      )
    default:
      return true
  }
}

export const formatNetworks = (acl: AclEntry[] = []) =>
  acl.map((a) => (a.name ? `${a.name}=${a.value}` : a.value)).join('\n')

/** parseNetworks reads "value" or "name=value" entries. */
export function parseNetworks(text: string): AclEntry[] {
  return lines(text).map((e) => {
    const i = e.indexOf('=')
    return i < 0 ? { value: e } : { name: e.slice(0, i).trim(), value: e.slice(i + 1).trim() }
  })
}

const IPV4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/

const validIPv4 = (s: string) => IPV4.test(s) && s.split('.').every((p) => Number(p) <= 255)

/** networksError explains the first authorized network the API would reject. */
export function networksError(text: string): string | undefined {
  for (const { value } of parseNetworks(text)) {
    if (!value.includes('/')) {
      if (!validIPv4(value))
        return `Invalid request: Non-routable or private authorized network (${value}).`
      continue
    }
    const [ip = '', bits = ''] = value.split('/')
    if (!validIPv4(ip) || !/^\d{1,2}$/.test(bits) || Number(bits) > 32) {
      return `Invalid request: Invalid authorized network (${value}).`
    }
  }
  return undefined
}
