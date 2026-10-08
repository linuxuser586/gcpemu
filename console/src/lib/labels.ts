// Labels are edited as text, one key=value per line or comma-separated,
// as gcloud's --labels takes them.

/** LABEL_KEY is GCP's label key rule. */
const LABEL_KEY = /^[\p{Ll}\p{Lo}][\p{Ll}\p{Lo}\p{N}_-]{0,62}$/u
const LABEL_VALUE = /^[\p{Ll}\p{Lo}\p{N}_-]{0,63}$/u

export function formatLabels(labels?: Record<string, string>): string {
  return Object.entries(labels ?? {})
    .map(([k, v]) => `${k}=${v}`)
    .join('\n')
}

function entries(text: string): string[] {
  return text
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter(Boolean)
}

export function parseLabels(text: string): Record<string, string> {
  const out: Record<string, string> = {}
  for (const e of entries(text)) {
    const [k = '', ...v] = e.split('=')
    out[k.trim()] = v.join('=').trim()
  }
  return out
}

/** labelsError explains the first malformed label in text, if any. */
export function labelsError(text: string): string | undefined {
  for (const e of entries(text)) {
    const [k = '', ...rest] = e.split('=')
    const v = rest.join('=')
    if (!LABEL_KEY.test(k.trim())) {
      return `Invalid label key "${k.trim()}": keys start with a lowercase letter and have at most 63 lowercase letters, digits, underscores or dashes.`
    }
    if (!LABEL_VALUE.test(v.trim())) {
      return `Invalid value for label "${k.trim()}": values have at most 63 lowercase letters, digits, underscores or dashes.`
    }
  }
  return undefined
}
