import type { Taint, TaintEffect } from './api'

// Taints are edited as gcloud's --node-taints takes them: key=value:Effect,
// one per line or comma-separated.

const EFFECTS: Record<string, TaintEffect> = {
  NoSchedule: 'NO_SCHEDULE',
  PreferNoSchedule: 'PREFER_NO_SCHEDULE',
  NoExecute: 'NO_EXECUTE',
}
const NAMES = Object.fromEntries(Object.entries(EFFECTS).map(([k, v]) => [v, k])) as Record<
  TaintEffect,
  string
>

export function formatTaints(taints?: Taint[]): string {
  return (taints ?? [])
    .map((t) => `${t.key}${t.value ? `=${t.value}` : ''}:${NAMES[t.effect] ?? t.effect}`)
    .join('\n')
}

function entries(text: string) {
  return text
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter(Boolean)
}

function parseOne(e: string): Taint | string {
  const i = e.lastIndexOf(':')
  const effect = i < 0 ? undefined : EFFECTS[e.slice(i + 1).trim()]
  const kv = i < 0 ? e : e.slice(0, i)
  const [key = '', ...v] = kv.split('=')
  if (!key.trim() || !effect) {
    return `Invalid taint "${e}": taints are key=value:Effect, where Effect is NoSchedule, PreferNoSchedule or NoExecute.`
  }
  const value = v.join('=').trim()
  return value ? { key: key.trim(), value, effect } : { key: key.trim(), effect }
}

export function parseTaints(text: string): Taint[] {
  return entries(text)
    .map(parseOne)
    .filter((t): t is Taint => typeof t !== 'string')
}

/** taintsError explains the first malformed taint in text, if any. */
export function taintsError(text: string): string | undefined {
  for (const e of entries(text)) {
    const t = parseOne(e)
    if (typeof t === 'string') return t
  }
  return undefined
}
