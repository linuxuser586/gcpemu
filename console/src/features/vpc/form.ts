import type { FieldValues, Path, UseFormReturn } from 'react-hook-form'

import { errorMessage } from '@/api/errors'

import { invalid, requiredMessage } from '../lb/spec'
import { resourceField, type SecondaryRange } from './api'

// What the network and subnetwork forms share: reading API values and
// showing the API's errors on the fields they name.

export type Obj = Record<string, unknown>

export const str = (v: unknown) =>
  typeof v === 'string' ? v : typeof v === 'number' ? String(v) : ''

/** NAME is the API's rule for a resource name. */
export const NAME = /^[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?$/

/** mtuError is the API's message for an MTU out of range, if v is one. */
export function mtuError(v: string): string | undefined {
  const t = v.trim()
  if (!t) return undefined
  if (!/^\d+$/.test(t)) return invalid('mtu', t, 'Must be a whole number.')
  const n = Number(t)
  return n < 1300 || n > 8896 ? invalid('mtu', t, 'Must be between 1300 and 8896.') : undefined
}

const IPV4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})\/(\d{1,2})$/

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
 * withFieldError shows an API error on the form field it names (fields
 * maps the API field's first segment to the form's), then rethrows it
 * for the editor to show above the buttons.
 */
export async function withFieldError<V extends FieldValues>(
  form: UseFormReturn<V>,
  fields: Record<string, string>,
  send: () => Promise<unknown>,
) {
  try {
    return await send()
  } catch (e) {
    const head = resourceField(errorMessage(e))?.split(/[.[]/)[0]
    const f = head && fields[head]
    if (f && f in form.getValues()) {
      form.setError(f as Path<V>, { type: 'server', message: errorMessage(e) })
    }
    throw e
  }
}
