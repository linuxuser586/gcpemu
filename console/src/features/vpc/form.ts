import type { FieldValues, Path, UseFormReturn } from 'react-hook-form'

import { errorMessage } from '@/api/errors'

import { lastSeg, relPath } from '../lb/api'
import { invalid, requiredMessage } from '../lb/spec'
import { resourceField, type FirewallRule, type SecondaryRange } from './api'

// What the VPC view's forms share: reading API values and showing the
// API's errors on the fields they name.

export type Obj = Record<string, unknown>

export const str = (v: unknown) =>
  typeof v === 'string' ? v : typeof v === 'number' ? String(v) : ''

/** NAME is the API's rule for a resource name. */
export const NAME = /^[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?$/

/** networkRef writes a network field: a bare name is in project. */
export const networkRef = (v: string, project: string) =>
  NAME.test(v.trim()) ? `projects/${project}/global/networks/${v.trim()}` : v.trim()

/** networkValue reads a network field: a name for project's networks. */
export function networkValue(link: unknown, project: string) {
  const p = relPath(str(link))
  return p.startsWith(`projects/${project}/global/networks/`) ? lastSeg(p) : p
}

/** mtuError is the API's message for an MTU out of range, if v is one. */
export function mtuError(v: string): string | undefined {
  const t = v.trim()
  if (!t) return undefined
  if (!/^\d+$/.test(t)) return invalid('mtu', t, 'Must be a whole number.')
  const n = Number(t)
  return n < 1300 || n > 8896 ? invalid('mtu', t, 'Must be between 1300 and 8896.') : undefined
}

const IPV4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})\/(\d{1,2})$/

/** isIPv4 is whether s is a dotted IPv4 address. */
export function isIPv4(s: string): boolean {
  const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(s.trim())
  return !!m && m.slice(1).every((o) => Number(o) <= 255)
}

/**
 * parseCidr reads an IPv4 CIDR network address range, as the API does:
 * a host address with bits set past the prefix is not one.
 */
export function parseCidr(s: string): { base: number; bits: number } | undefined {
  const m = IPV4.exec(s.trim())
  if (!m) return undefined
  const octets = m.slice(1, 5).map(Number)
  const bits = Number(m[5])
  if (octets.some((o) => o > 255) || bits > 32) return undefined
  const base = octets.reduce((a, o) => a * 256 + o, 0)
  const size = 2 ** (32 - bits)
  return base % size === 0 ? { base, bits } : undefined
}

export const CIDR_INVALID = 'Must be a valid IPv4 CIDR network address range.'

/** primaryRangeError is the API's message for a subnetwork's ipCidrRange, if it is invalid. */
export function primaryRangeError(v: string): string | undefined {
  if (!v.trim()) return requiredMessage('ipCidrRange')
  const c = parseCidr(v)
  if (!c) return invalid('ipCidrRange', v, CIDR_INVALID)
  if (c.bits < 8 || c.bits > 29)
    return invalid('ipCidrRange', v, 'The prefix length must be between 8 and 29.')
  return undefined
}

/** parseRanges reads secondary ranges, one "NAME CIDR" per line. */
export function parseRanges(text: string): SecondaryRange[] {
  return text
    .split('\n')
    .map((l) => l.trim())
    .filter(Boolean)
    .map((l) => {
      const [rangeName = '', ipCidrRange = ''] = l.split(/[\s=]+/)
      return { rangeName, ipCidrRange }
    })
}

export const formatRanges = (r?: SecondaryRange[]) =>
  (r ?? []).map((x) => `${x.rangeName} ${x.ipCidrRange}`).join('\n')

/** rangesError is the API's message for the first invalid secondary range. */
export function rangesError(text: string): string | undefined {
  const lines = text.split('\n').filter((l) => l.trim())
  if (lines.some((l) => l.trim().split(/[\s=]+/).length !== 2)) {
    return 'Each line is a range name and its CIDR range, e.g. pods 10.4.0.0/14.'
  }
  const seen = new Set<string>()
  for (const [i, r] of parseRanges(text).entries()) {
    if (!NAME.test(r.rangeName)) {
      return `Invalid value for field 'resource.secondaryIpRanges.rangeName': '${r.rangeName}'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'`
    }
    if (seen.has(r.rangeName)) {
      return invalid('secondaryIpRanges', r.rangeName, 'Secondary range names must be unique.')
    }
    seen.add(r.rangeName)
    const c = parseCidr(r.ipCidrRange)
    if (!c || c.bits < 4 || c.bits > 29) {
      return invalid(`secondaryIpRanges[${i}].ipCidrRange`, r.ipCidrRange, CIDR_INVALID)
    }
  }
  return undefined
}

/**
 * conflictRange is the range an API overlap error names, which names no
 * field: X in "Invalid IPCidrRange: X conflicts with existing …".
 */
export const conflictRange = (message: string) =>
  /^Invalid IPCidrRange: (\S+) conflicts with /.exec(message)?.[1]

/**
 * withFieldError shows an API error on the form field it names (fields
 * maps the API field's first segment to the form's; fieldOf names the
 * form field of an error that names none), then rethrows it for the
 * editor to show above the buttons.
 */
export async function withFieldError<V extends FieldValues>(
  form: UseFormReturn<V>,
  fields: Record<string, string>,
  send: () => Promise<unknown>,
  fieldOf?: (message: string) => string | undefined,
) {
  try {
    return await send()
  } catch (e) {
    const head = resourceField(errorMessage(e))?.split(/[.[]/)[0]
    const f = (head && fields[head]) || fieldOf?.(errorMessage(e))
    if (f && f in form.getValues()) {
      form.setError(f as Path<V>, { type: 'server', message: errorMessage(e) })
    }
    throw e
  }
}

/** formatRules writes firewall rules as gcloud's --rules does: tcp:22,tcp:80-90,icmp. */
export const formatRules = (rules?: FirewallRule[]) =>
  (rules ?? [])
    .flatMap((r) => (r.ports?.length ? r.ports.map((p) => `${r.IPProtocol}:${p}`) : [r.IPProtocol]))
    .join(',')

const PROTOCOLS = new Set(['tcp', 'udp', 'icmp', 'esp', 'ah', 'sctp', 'ipip', 'all'])
const WITH_PORTS = new Set(['tcp', 'udp', 'sctp', '6', '17', '132'])

/** rulesError is the message for the first invalid firewall rule, as gcloud's --rules writes them. */
export function rulesError(text: string, field: 'allowed' | 'denied'): string | undefined {
  const items = text.split(',').map((x) => x.trim())
  if (!text.trim()) return "Exactly one of 'allowed' or 'denied' must be specified."
  for (const item of items) {
    const [proto = '', port, ...rest] = item.toLowerCase().split(':')
    const num = /^\d+$/.test(proto) && Number(proto) <= 255
    if (!PROTOCOLS.has(proto) && !num) {
      return invalid(
        field,
        item,
        'A rule is a protocol (tcp, udp, icmp, esp, ah, sctp, ipip, all or a number), with ports for tcp, udp and sctp: tcp:22, udp:5000-5100.',
      )
    }
    if (port === undefined) continue
    if (rest.length || !WITH_PORTS.has(proto)) {
      return invalid(field, item, 'Only tcp, udp and sctp rules take ports.')
    }
    const m = /^(\d+)(?:-(\d+))?$/.exec(port)
    const lo = Number(m?.[1])
    const hi = m?.[2] === undefined ? lo : Number(m[2])
    if (!m || hi > 65535 || lo > hi) return invalid(field, item, 'Ports are 0 to 65535.')
  }
  return undefined
}

/** parseRules reads firewall rules as gcloud's --rules writes them, a protocol's ports together. */
export function parseRules(text: string): FirewallRule[] {
  const out: FirewallRule[] = []
  for (const item of text
    .split(',')
    .map((x) => x.trim().toLowerCase())
    .filter(Boolean)) {
    const [IPProtocol = '', port] = item.split(':')
    let rule = out.find((r) => r.IPProtocol === IPProtocol)
    if (!rule) {
      rule = { IPProtocol, ports: [] }
      out.push(rule)
    }
    // A protocol without ports is all its ports.
    if (port === undefined) rule.ports = undefined
    else rule.ports?.push(port)
  }
  return out.map((r) => (r.ports?.length ? r : { IPProtocol: r.IPProtocol }))
}

/** parseList reads a list of ranges, tags or service accounts, separated by commas or spaces. */
export const parseList = (text: string) => text.split(/[\s,]+/).filter(Boolean)

export const formatList = (l?: string[]) => (l ?? []).join(', ')

/** priorityError is the API's message for a priority out of range, if v is one. */
export function priorityError(v: string): string | undefined {
  const t = v.trim()
  if (!t) return undefined
  if (!/^\d+$/.test(t)) return invalid('priority', t, 'Must be a whole number.')
  return Number(t) > 65535 ? invalid('priority', t, 'Must be between 0 and 65535.') : undefined
}
