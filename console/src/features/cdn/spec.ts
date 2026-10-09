import { isObj, text } from '../lb/api'
import {
  getPath,
  invalid,
  lines,
  setPath,
  type FieldSpec,
  type FormValue,
  type KindSpec,
  type Values,
} from '../lb/spec'
import type { OriginColl } from './api'

// The cache settings form of an origin (FR-UI-011): a backend service's or
// bucket's cdnPolicy (FR-CDN-002, 003, 006, 007), declared as the API fields
// it edits with the lb view's field specs, so that the form and the JSON
// editor edit one patch body. Checks show the API's messages.

type Obj = Record<string, unknown>

/** Limits GCP enforces (services/lb/cdnpolicy.go). */
export const MAX_TTL = 31622400
export const MAX_SERVE_WHILE_STALE = 604800
export const MAX_NEGATIVE_TTL = 1800
export const NEGATIVE_CODES = [300, 301, 302, 307, 308, 404, 405, 410, 421, 451, 501]
const MAX_BYPASS_HEADERS = 5

export const CACHE_MODES = ['CACHE_ALL_STATIC', 'USE_ORIGIN_HEADERS', 'FORCE_CACHE_ALL'] as const

const P = 'cdnPolicy'
const mode = (v: Values) => text(v.cacheMode) || 'CACHE_ALL_STATIC'
const ttls = (v: Values) => mode(v) !== 'USE_ORIGIN_HEADERS'
const hasMax = (v: Values) => mode(v) === 'CACHE_ALL_STATIC'

const range = (path: string, max: number) => (value: FormValue) => {
  const n = Number(value)
  return n < 0 || n > max ? invalid(path, value, `Must be between 0 and ${max}.`) : undefined
}

const ttl = (key: string, label: string, hint: string, extra?: FieldSpec['check']): FieldSpec => ({
  key,
  path: `${P}.${key}`,
  label,
  type: 'number',
  hint,
  check: (value, v) => range(`${P}.${key}`, MAX_TTL)(value) ?? extra?.(value, v),
})

/** DEFAULT_MAX_TTL is the maxTtl the API sets when it is empty. */
const DEFAULT_MAX_TTL = 86400

/** belowMax is the check that a TTL does not exceed maxTtl. */
const belowMax = (key: string, what: string) => (value: FormValue, v: Values) =>
  hasMax(v) && Number(value) > (v.maxTtl === '' ? DEFAULT_MAX_TTL : Number(v.maxTtl))
    ? invalid(`${P}.${key}`, value, `${what} must be less than or equal to max TTL.`)
    : undefined

/** negative reads negativeCachingPolicy as "code=ttl" lines. */
const negative: FieldSpec['codec'] = {
  read: (api) =>
    Array.isArray(api)
      ? (api as { code?: number; ttl?: number }[])
          .map((p) => `${p.code ?? ''}=${p.ttl ?? 0}`)
          .join('\n')
      : '',
  write: (value) => {
    const l = lines(value as string).map((line) => {
      const [code = '', ttl = ''] = line.split('=').map((s) => s.trim())
      return { code: Number(code), ttl: Number(ttl) }
    })
    return l.length ? l : undefined
  },
}

/** headerNames reads [{headerName}] as one name per line. */
const headerNames: FieldSpec['codec'] = {
  read: (api) =>
    Array.isArray(api)
      ? (api as { headerName?: string }[]).map((h) => h.headerName ?? '').join('\n')
      : '',
  write: (value) => {
    const l = lines(value as string).map((headerName) => ({ headerName }))
    return l.length ? l : undefined
  },
}

const KEY = `${P}.cacheKeyPolicy`

/**
 * keyPart is a cache key part a backend service includes by default:
 * without a cacheKeyPolicy, all of protocol, host and query string are in.
 */
const keyPart = (key: string, label: string): FieldSpec => ({
  key,
  path: `${KEY}.${key}`,
  label,
  type: 'checkbox',
  codec: {
    read: (api, body) => (isObj(getPath(body, KEY)) ? api === true : true),
    write: (value, cur) => (value === true ? true : cur === undefined ? undefined : false),
  },
})

const names = (key: string, label: string, hint: string, show?: (v: Values) => boolean) =>
  ({
    key,
    path: `${KEY}.${key}`,
    label,
    type: 'lines',
    mono: true,
    hint,
    show,
  }) satisfies FieldSpec

const common: FieldSpec[] = [
  {
    key: 'cacheMode',
    path: `${P}.cacheMode`,
    label: 'Cache mode',
    type: 'select',
    options: CACHE_MODES,
    emptyOption: 'Default (CACHE_ALL_STATIC)',
    hint: 'CACHE_ALL_STATIC caches static content and responses with valid caching headers; USE_ORIGIN_HEADERS only what the origin’s headers allow; FORCE_CACHE_ALL every successful response.',
  },
  {
    ...ttl(
      'defaultTtl',
      'Default TTL (seconds)',
      'For responses without a max-age. Empty for 3600 (1 hour).',
      belowMax('defaultTtl', 'Default TTL'),
    ),
    show: ttls,
  },
  {
    ...ttl(
      'maxTtl',
      'Maximum TTL (seconds)',
      'Caps the origin’s max-age. Empty for 86400 (1 day).',
    ),
    show: hasMax,
  },
  {
    ...ttl(
      'clientTtl',
      'Client TTL (seconds)',
      'The max-age clients see. Empty for the default TTL.',
      belowMax('clientTtl', 'Client TTL'),
    ),
    show: ttls,
  },
  {
    key: 'serveWhileStale',
    path: `${P}.serveWhileStale`,
    label: 'Serve while stale (seconds)',
    type: 'number',
    hint: 'How long past expiry an entry may be served while it revalidates or the origin fails. Empty for 0.',
    check: range(`${P}.serveWhileStale`, MAX_SERVE_WHILE_STALE),
  },
  {
    key: 'negativeCaching',
    path: `${P}.negativeCaching`,
    label: 'Negative caching',
    type: 'checkbox',
  },
  {
    key: 'negativeCachingPolicy',
    path: `${P}.negativeCachingPolicy`,
    label: 'Negative caching TTLs',
    type: 'lines',
    mono: true,
    show: (v) => v.negativeCaching === true,
    codec: negative,
    hint: `status=seconds, one per line, e.g. 404=60. Empty for Cloud CDN’s TTL per status code.`,
    check: (value) => {
      const seen = new Set<number>()
      for (const [i, line] of lines(value as string).entries()) {
        const m = /^(\d+)\s*=\s*(\d+)$/.exec(line)
        const at = `${P}.negativeCachingPolicy[${i}]`
        if (!m) return `Invalid negative caching TTL "${line}": write status=seconds, e.g. 404=60.`
        const code = Number(m[1])
        if (!NEGATIVE_CODES.includes(code))
          return invalid(
            `${at}.code`,
            code,
            'Status code must be one of 300, 301, 302, 307, 308, 404, 405, 410, 421, 451 or 501.',
          )
        if (seen.has(code)) return invalid(`${at}.code`, code, 'Status codes must be unique.')
        seen.add(code)
        if (Number(m[2]) > MAX_NEGATIVE_TTL)
          return invalid(`${at}.ttl`, m[2], `Must be between 0 and ${MAX_NEGATIVE_TTL}.`)
      }
      return undefined
    },
  },
  {
    key: 'bypassCacheOnRequestHeaders',
    path: `${P}.bypassCacheOnRequestHeaders`,
    label: 'Bypass the cache on request headers',
    type: 'lines',
    mono: true,
    codec: headerNames,
    hint: 'Header names, one per line (up to 5): a request carrying one goes to the origin.',
    check: (value) => {
      const n = lines(value as string).length
      return n > MAX_BYPASS_HEADERS
        ? invalid(
            `${P}.bypassCacheOnRequestHeaders`,
            n,
            `At most ${MAX_BYPASS_HEADERS} headers can be specified.`,
          )
        : undefined
    },
  },
  {
    key: 'signedUrlCacheMaxAgeSec',
    path: `${P}.signedUrlCacheMaxAgeSec`,
    label: 'Signed request cache TTL (seconds)',
    type: 'number',
    hint: 'How long responses to signed URLs and cookies stay cached. Empty for 3600.',
    check: range(`${P}.signedUrlCacheMaxAgeSec`, MAX_TTL),
  },
]

const includeQuery = (v: Values) => v.includeQueryString === true

/** finish drops settings the cache mode or a switch turns off. */
function finish(body: Obj, v: Values) {
  if (!ttls(v))
    for (const k of ['defaultTtl', 'maxTtl', 'clientTtl']) setPath(body, `${P}.${k}`, undefined)
  else if (!hasMax(v)) setPath(body, `${P}.maxTtl`, undefined)
  if (v.negativeCaching !== true) setPath(body, `${P}.negativeCachingPolicy`, undefined)
}

export const SPECS: Record<OriginColl, KindSpec> = {
  backendServices: {
    coll: 'backendServices',
    fields: [
      ...common,
      keyPart('includeProtocol', 'Cache key: protocol'),
      keyPart('includeHost', 'Cache key: host'),
      keyPart('includeQueryString', 'Cache key: query string'),
      names(
        'queryStringWhitelist',
        'Query parameters to include',
        'Names, one per line. Empty includes the whole query string.',
        includeQuery,
      ),
      {
        ...names(
          'queryStringBlacklist',
          'Query parameters to exclude',
          'Names, one per line; instead of a list to include.',
          includeQuery,
        ),
        check: (value, v) =>
          lines(text(v.queryStringWhitelist)).length && lines(value as string).length
            ? invalid(
                `${KEY}.queryStringBlacklist`,
                lines(value as string).length,
                'Only one of queryStringWhitelist and queryStringBlacklist can be specified.',
              )
            : undefined,
      },
      names('includeHttpHeaders', 'Cache key: request headers', 'Header names, one per line.'),
      names('includeNamedCookies', 'Cache key: cookies', 'Cookie names, one per line.'),
    ],
    finish: (body, v) => {
      finish(body, v)
      if (!includeQuery(v)) {
        setPath(body, `${KEY}.queryStringWhitelist`, undefined)
        setPath(body, `${KEY}.queryStringBlacklist`, undefined)
      }
      // Without a key policy the API includes all three parts: send the
      // parts explicitly once one is left out.
      if (
        !isObj(getPath(body, KEY)) &&
        !(v.includeProtocol && v.includeHost && v.includeQueryString)
      ) {
        setPath(body, KEY, {
          includeProtocol: v.includeProtocol === true,
          includeHost: v.includeHost === true,
          includeQueryString: v.includeQueryString === true,
        })
      }
    },
  },
  backendBuckets: {
    coll: 'backendBuckets',
    fields: [
      ...common,
      names(
        'queryStringWhitelist',
        'Cache key: query parameters',
        'Names, one per line. Empty includes the whole query string.',
      ),
      names('includeHttpHeaders', 'Cache key: request headers', 'Header names, one per line.'),
    ],
    finish,
  },
}
