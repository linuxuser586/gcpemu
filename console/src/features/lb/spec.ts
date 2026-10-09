import { formatLabels, labelsError, parseLabels } from '@/lib/labels'

import { applyMerge, isObj, mergeDiff, relPath, text, type Coll, type Resource } from './api'

// The create and edit forms of every load-balancing kind (FR-UI-011),
// declared as the API fields they edit. A field reads its value out of a
// request body and writes it back, so that the form and the raw JSON
// editor edit one body; validation shows the API's own messages.

type Obj = Record<string, unknown>

/** Matcher is one path matcher of a URL map, with the hosts routed to it. */
export interface Matcher {
  name: string
  /** hosts are comma-separated. */
  hosts: string
  defaultService: string
  /** pathRules are "paths = backend", one per line. */
  pathRules: string
}

export type FormValue = string | boolean | string[] | Matcher[]
export type Values = Record<string, FormValue>

export type FieldType =
  | 'text'
  | 'textarea'
  | 'select'
  | 'number'
  | 'checkbox'
  | 'lines'
  | 'labels'
  | 'ref'
  | 'refs'
  | 'routing'

/** Target is a collection a reference field offers resources of. */
export type Target = Coll | 'networkEndpointGroups'

export interface FieldSpec {
  key: string
  label: string
  type: FieldType
  /**
   * path is the dotted API field; a function of the values read so far
   * when its place depends on another field (a health check's port). The
   * scope field has none: it is the request's URL.
   */
  path?: string | ((v: Values) => string)
  hint?: string
  options?: readonly string[]
  /** emptyOption labels a select's "" option, which leaves the field unset. */
  emptyOption?: string
  targets?: readonly Target[]
  /** asList: a single reference the API keeps in a list (healthChecks). */
  asList?: boolean
  /** required, or the API's message when it is not the usual one. */
  required?: boolean | string
  /** createOnly fields cannot change after create. */
  createOnly?: boolean
  /** scope is the region field: "" for a global resource. */
  scope?: boolean
  show?: (v: Values) => boolean
  /** check returns the API's message for an invalid value. */
  check?: (value: FormValue, v: Values) => string | undefined
  /** initial is the create form's value. */
  initial?: FormValue
  mono?: boolean
}

export interface KindSpec {
  coll: Coll
  fields: readonly FieldSpec[]
  /** finish tidies a written body, e.g. drops the unused health check block. */
  finish?: (body: Obj, v: Values) => void
}

// ---- paths ----

function getPath(o: Obj, path: string): unknown {
  let cur: unknown = o
  for (const p of path.split('.')) {
    if (!isObj(cur)) return undefined
    cur = cur[p]
  }
  return cur
}

/** setPath sets a dotted field, creating parents; undefined removes it. */
function setPath(o: Obj, path: string, value: unknown) {
  const parts = path.split('.')
  const last = parts.pop()!
  let cur = o
  for (const p of parts) {
    if (!isObj(cur[p])) {
      if (value === undefined) return
      cur[p] = {}
    } else cur[p] = { ...cur[p] }
    cur = cur[p] as Obj
  }
  if (value === undefined) delete cur[last]
  else cur[last] = value
}

const pathOf = (f: FieldSpec, v: Values) => (typeof f.path === 'function' ? f.path(v) : f.path)

// ---- references ----

/** short is a reference without its Project: "global/backendServices/api". */
export const short = (link: unknown) =>
  typeof link === 'string' ? relPath(link).replace(/^projects\/[^/]+\//, '') : ''

const sameRef = (a: unknown, b: unknown) => !!a && short(a) === short(b)

const str = (v: unknown) => (typeof v === 'string' ? v : '')
const list = (v: unknown) => (Array.isArray(v) ? v.filter((x) => typeof x === 'string') : [])
const lines = (s: string) =>
  s
    .split('\n')
    .map((l) => l.trim())
    .filter(Boolean)

// ---- the URL map's routing ----

/** parsePathRule reads "paths = backend"; undefined for a malformed line. */
export function parsePathRule(line: string): { paths: string[]; service: string } | undefined {
  const at = line.lastIndexOf('=')
  if (at < 0) return undefined
  const paths = line
    .slice(0, at)
    .split(',')
    .map((p) => p.trim())
    .filter(Boolean)
  const service = line.slice(at + 1).trim()
  return paths.length && service ? { paths, service } : undefined
}

function readRouting(body: Obj): Matcher[] {
  const hostRules = Array.isArray(body.hostRules) ? (body.hostRules as Obj[]) : []
  const pms = Array.isArray(body.pathMatchers) ? (body.pathMatchers as Obj[]) : []
  return pms.map((pm) => ({
    name: str(pm.name),
    hosts: hostRules
      .filter((h) => h.pathMatcher === pm.name)
      .flatMap((h) => list(h.hosts))
      .join(', '),
    defaultService: relPath(str(pm.defaultService)),
    pathRules: (Array.isArray(pm.pathRules) ? (pm.pathRules as Obj[]) : [])
      .filter((r) => typeof r.service === 'string')
      .map((r) => `${list(r.paths).join(', ')} = ${short(r.service)}`)
      .join('\n'),
  }))
}

/**
 * writeRouting writes the matchers as host rules and path matchers. Each
 * matcher keeps what the form does not edit (route rules, path rules that
 * redirect or have a route action, header actions); unchanged rules keep
 * the API's links.
 */
function writeRouting(body: Obj, ms: Matcher[]) {
  const oldPms = Array.isArray(body.pathMatchers) ? (body.pathMatchers as Obj[]) : []
  const oldHrs = Array.isArray(body.hostRules) ? (body.hostRules as Obj[]) : []
  const hostRules: Obj[] = []
  const pathMatchers = ms.map((m) => {
    const old = oldPms.find((p) => p.name === m.name) ?? {}
    const pm: Obj = { ...old, name: m.name }
    if (!m.defaultService) delete pm.defaultService
    else if (!sameRef(old.defaultService, m.defaultService)) pm.defaultService = m.defaultService
    const oldRules = Array.isArray(old.pathRules) ? (old.pathRules as Obj[]) : []
    const rules: Obj[] = []
    for (const line of lines(m.pathRules)) {
      const r = parsePathRule(line)
      if (!r) continue
      const kept = oldRules.find(
        (o) =>
          JSON.stringify(list(o.paths)) === JSON.stringify(r.paths) &&
          sameRef(o.service, r.service),
      )
      rules.push(kept ?? r)
    }
    rules.push(...oldRules.filter((o) => typeof o.service !== 'string'))
    if (rules.length) pm.pathRules = rules
    else delete pm.pathRules
    const hosts = m.hosts
      .split(',')
      .map((h) => h.trim())
      .filter(Boolean)
    if (hosts.length) {
      const kept = oldHrs.find(
        (h) => h.pathMatcher === m.name && JSON.stringify(list(h.hosts)) === JSON.stringify(hosts),
      )
      hostRules.push(kept ?? { hosts, pathMatcher: m.name })
    }
    return pm
  })
  if (hostRules.length) body.hostRules = hostRules
  else delete body.hostRules
  if (pathMatchers.length) body.pathMatchers = pathMatchers
  else delete body.pathMatchers
}

// ---- read and write ----

function fromApi(f: FieldSpec, body: Obj, v: Values): FormValue {
  if (f.type === 'routing') return readRouting(body)
  const api = getPath(body, pathOf(f, v) ?? '')
  switch (f.type) {
    case 'checkbox':
      return api === true
    case 'number':
      return typeof api === 'number' || typeof api === 'string' ? text(api) : ''
    case 'lines':
      return list(api).join('\n')
    case 'labels':
      return formatLabels(isObj(api) ? (api as Record<string, string>) : undefined)
    case 'ref':
      // A single-valued list (a backend service's health check).
      return relPath(str(Array.isArray(api) ? api[0] : api))
    case 'refs':
      if (Array.isArray(api) && api.every(isObj)) {
        return api.map((b) => relPath(str(b.group)))
      }
      return list(api).map(relPath)
    default:
      return str(api)
  }
}

function toApi(f: FieldSpec, value: FormValue, cur: unknown): unknown {
  switch (f.type) {
    case 'checkbox':
      return value === true ? true : cur === undefined ? undefined : false
    case 'number':
      return value === '' ? undefined : Number(value)
    case 'lines': {
      const l = lines(value as string)
      return l.length ? l : undefined
    }
    case 'labels': {
      const l = parseLabels(value as string)
      return Object.keys(l).length ? l : undefined
    }
    case 'ref': {
      if (!value) return undefined
      if (f.asList)
        return Array.isArray(cur) && cur.length === 1 && sameRef(cur[0], value) ? cur : [value]
      return sameRef(cur, value) ? cur : value
    }
    case 'refs': {
      const want = value as string[]
      if (f.key === 'backends') {
        // Backends keep their settings; a new NEG balances by rate.
        const old = Array.isArray(cur) ? (cur as Obj[]) : []
        const out = want.map(
          (g) =>
            old.find((b) => sameRef(b.group, g)) ?? {
              group: g,
              balancingMode: 'RATE',
              maxRatePerEndpoint: 100,
            },
        )
        return out.length ? out : undefined
      }
      const old = list(cur)
      if (old.length === want.length && old.every((o, i) => sameRef(o, want[i]))) return cur
      return want.length ? want : undefined
    }
    default:
      return value === '' ? undefined : value
  }
}

/** readValues reads a form's values out of a request body. */
export function readValues(spec: KindSpec, body: Obj, prev: Values = {}): Values {
  const v: Values = {}
  for (const f of spec.fields) {
    v[f.key] = f.scope ? (prev[f.key] ?? '') : fromApi(f, body, v)
  }
  return v
}

/**
 * writeValues writes a form's values onto a copy of body. Hidden fields
 * and, on edit, create-only ones are left as they are.
 */
export function writeValues(spec: KindSpec, body: Obj, v: Values, edit = false): Obj {
  const out: Obj = structuredClone(body)
  for (const f of spec.fields) {
    if (f.scope || (edit && f.createOnly) || (f.show && !f.show(v))) continue
    if (f.type === 'routing') {
      writeRouting(out, v[f.key] as Matcher[])
      continue
    }
    const path = pathOf(f, v)!
    setPath(out, path, toApi(f, v[f.key]!, getPath(out, path)))
  }
  spec.finish?.(out, v)
  return out
}

/** initialValues are a create form's values. */
export function initialValues(spec: KindSpec, region = ''): Values {
  const v = readValues(spec, {})
  for (const f of spec.fields) {
    if (f.initial !== undefined) v[f.key] = f.initial
    if (f.scope) v[f.key] = region
  }
  return v
}

/** createBody is the insert request body of a create form. */
export const createBody = (spec: KindSpec) => (v: Values, base: Obj) => writeValues(spec, base, v)

/**
 * patchBody is the patch request body of an edit form: a JSON merge patch
 * of what changed, with the fingerprint it was read at.
 */
export function patchBody(spec: KindSpec, cur: Resource) {
  return (v: Values, base: Obj): Obj => {
    const next = writeValues(spec, applyMerge(cur, base), v, true)
    const d = mergeDiff(cur, next)
    if (Object.keys(d).length && cur.fingerprint && !('fingerprint' in d))
      d.fingerprint = cur.fingerprint
    return d
  }
}

export const patchValues = (spec: KindSpec, cur: Resource) => (body: Obj, prev: Values) =>
  readValues(spec, applyMerge(cur, body), prev)

// ---- validation ----

const NAME = /^[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?$/

/** nameMessage is the API's message for an invalid resource name. */
export const nameMessage = (name: string) =>
  `Invalid value for field 'resource.name': '${name}'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'`

/** requiredMessage is the API's message for a missing field. */
export const requiredMessage = (path: string) => `Required field 'resource.${path}' not specified`

const invalid = (path: string, value: unknown, why: string) =>
  `Invalid value for field 'resource.${path}': '${text(value)}'. ${why}`

const empty = (v: FormValue | undefined) =>
  v === undefined || v === '' || (Array.isArray(v) && v.length === 0)

/** validate returns the error of each invalid field, by key. */
export function validate(spec: KindSpec, v: Values, edit = false): Record<string, string> {
  const errors: Record<string, string> = {}
  for (const f of spec.fields) {
    if ((edit && f.createOnly) || (f.show && !f.show(v))) continue
    const value = v[f.key]
    if (f.required && empty(value)) {
      errors[f.key] =
        typeof f.required === 'string'
          ? f.required
          : f.key === 'name'
            ? nameMessage('')
            : requiredMessage(pathOf(f, v) ?? f.key)
      continue
    }
    if (f.type === 'number' && value !== '' && !/^-?\d+$/.test(text(value))) {
      errors[f.key] = invalid(pathOf(f, v)!, value, 'Must be a whole number.')
      continue
    }
    const msg = !empty(value) ? f.check?.(value!, v) : undefined
    if (msg) errors[f.key] = msg
  }
  return errors
}

// ---- the kinds ----

const between =
  (path: string, lo: number, hi: number, why = `Must be between ${lo} and ${hi}.`) =>
  (value: FormValue) => {
    const n = Number(value)
    return n < lo || n > hi ? invalid(path, value, why) : undefined
  }

const name: FieldSpec = {
  key: 'name',
  path: 'name',
  label: 'Name',
  type: 'text',
  required: true,
  createOnly: true,
  mono: true,
  check: (v) => (NAME.test(v as string) ? undefined : nameMessage(v as string)),
}

const region = (what: string): FieldSpec => ({
  key: 'region',
  label: 'Region',
  type: 'text',
  scope: true,
  createOnly: true,
  mono: true,
  hint: `Empty for a global ${what}; a region such as us-central1 for a regional one.`,
})

const description: FieldSpec = {
  key: 'description',
  path: 'description',
  label: 'Description',
  type: 'text',
}

const headers = (key: string, path: string, label: string): FieldSpec => ({
  key,
  path,
  label,
  type: 'lines',
  mono: true,
  hint: 'name:value, one per line. Values may use variables such as {client_region}.',
  check: (v) => {
    const bad = lines(v as string).findIndex((h) => !/^[^:\s]+:/.test(h))
    return bad < 0
      ? undefined
      : invalid(
          `${path}[${bad}]`,
          lines(v as string)[bad],
          "Headers must be of the form 'name:value'.",
        )
  },
})

const SCHEMES = ['EXTERNAL_MANAGED', 'EXTERNAL', 'INTERNAL_MANAGED', 'INTERNAL_SELF_MANAGED']
const internal = (v: Values) => text(v.loadBalancingScheme).startsWith('INTERNAL')

const HC_TYPES = ['HTTP', 'HTTPS', 'HTTP2', 'TCP'] as const
const HC_BLOCK: Record<string, string> = {
  HTTP: 'httpHealthCheck',
  HTTPS: 'httpsHealthCheck',
  HTTP2: 'http2HealthCheck',
  TCP: 'tcpHealthCheck',
  SSL: 'sslHealthCheck',
  GRPC: 'grpcHealthCheck',
  GRPC_WITH_TLS: 'grpcTlsHealthCheck',
}
const hcBlock = (v: Values) => HC_BLOCK[text(v.type)] ?? 'httpHealthCheck'
const isHttp = (v: Values) => ['HTTP', 'HTTPS', 'HTTP2'].includes(text(v.type))

export const SPECS: Record<Coll, KindSpec> = {
  forwardingRules: {
    coll: 'forwardingRules',
    fields: [
      name,
      region('forwarding rule'),
      description,
      {
        key: 'loadBalancingScheme',
        path: 'loadBalancingScheme',
        label: 'Load balancing scheme',
        type: 'select',
        options: SCHEMES,
        createOnly: true,
        initial: 'EXTERNAL_MANAGED',
      },
      {
        key: 'IPAddress',
        path: 'IPAddress',
        label: 'IP address',
        type: 'text',
        createOnly: true,
        mono: true,
        hint: 'An address, or a reserved address resource such as global/addresses/web; empty for an ephemeral one.',
      },
      {
        key: 'portRange',
        path: 'portRange',
        label: 'Port',
        type: 'text',
        required: true,
        createOnly: true,
        mono: true,
        initial: '80',
        hint: 'One port: 80, 8080 or 443 for an external Application Load Balancer.',
        check: (v) =>
          /^\d+(-\d+)?$/.test(v as string) &&
          (!(v as string).includes('-') ||
            (v as string).split('-')[0] === (v as string).split('-')[1])
            ? undefined
            : invalid(
                'portRange',
                v,
                'Proxy load balancer forwarding rules must specify a single port.',
              ),
      },
      {
        key: 'target',
        path: 'target',
        label: 'Target proxy',
        type: 'ref',
        targets: ['targetHttpProxies', 'targetHttpsProxies'],
        required: true,
      },
      {
        key: 'network',
        path: 'network',
        label: 'Network',
        type: 'text',
        createOnly: true,
        mono: true,
        show: internal,
        hint: 'The VPC network of an internal load balancer, e.g. global/networks/default.',
      },
      {
        key: 'subnetwork',
        path: 'subnetwork',
        label: 'Subnetwork',
        type: 'text',
        createOnly: true,
        mono: true,
        show: internal,
      },
      {
        key: 'labels',
        path: 'labels',
        label: 'Labels',
        type: 'labels',
        hint: 'key=value, one per line',
        check: (v) => labelsError(v as string),
      },
    ],
  },
  targetHttpProxies: {
    coll: 'targetHttpProxies',
    fields: [
      name,
      region('proxy'),
      description,
      {
        key: 'urlMap',
        path: 'urlMap',
        label: 'URL map',
        type: 'ref',
        targets: ['urlMaps'],
        required: true,
      },
    ],
  },
  targetHttpsProxies: {
    coll: 'targetHttpsProxies',
    fields: [
      name,
      region('proxy'),
      description,
      {
        key: 'urlMap',
        path: 'urlMap',
        label: 'URL map',
        type: 'ref',
        targets: ['urlMaps'],
        required: true,
      },
      {
        key: 'sslCertificates',
        path: 'sslCertificates',
        label: 'SSL certificates',
        type: 'refs',
        targets: ['sslCertificates'],
        hint: 'The certificates the proxy chooses among by SNI (up to 15).',
        check: (v) =>
          (v as string[]).length > 15
            ? invalid(
                'sslCertificates',
                (v as string[]).length,
                'At most 15 SSL certificates can be specified.',
              )
            : undefined,
      },
      {
        key: 'certificateMap',
        path: 'certificateMap',
        label: 'Certificate map',
        type: 'text',
        mono: true,
        hint: 'Instead of SSL certificates: //certificatemanager.googleapis.com/projects/PROJECT/locations/global/certificateMaps/MAP',
      },
      {
        key: 'sslPolicy',
        path: 'sslPolicy',
        label: 'SSL policy',
        type: 'ref',
        targets: ['sslPolicies'],
        emptyOption: 'None (GCP’s default policy)',
      },
    ],
  },
  urlMaps: {
    coll: 'urlMaps',
    fields: [
      name,
      region('URL map'),
      description,
      {
        key: 'defaultService',
        path: 'defaultService',
        label: 'Default backend',
        type: 'ref',
        targets: ['backendServices', 'backendBuckets'],
        emptyOption: 'None',
        hint: 'Serves requests no host rule matches. Without it, set a default redirect in JSON.',
      },
      {
        key: 'routing',
        label: 'Host and path rules',
        type: 'routing',
        targets: ['backendServices', 'backendBuckets'],
        check: (v) => {
          const ms = v as Matcher[]
          for (const [i, m] of ms.entries()) {
            if (!NAME.test(m.name)) {
              return invalid(
                `pathMatchers[${i}].name`,
                m.name,
                "Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'",
              )
            }
            if (!m.hosts.trim()) {
              return `Path matcher ${m.name} has no hosts: every path matcher needs a host rule.`
            }
            const bad = lines(m.pathRules).find((l) => !parsePathRule(l))
            if (bad) {
              return `Invalid path rule "${bad}" in path matcher ${m.name}: write paths = backend, e.g. /api/* = global/backendServices/api.`
            }
            const path = lines(m.pathRules)
              .flatMap((l) => parsePathRule(l)?.paths ?? [])
              .find((p) => !p.startsWith('/'))
            if (path)
              return invalid(`pathMatchers[${i}].pathRules`, path, 'Path must start with /.')
          }
          return undefined
        },
      },
    ],
  },
  backendServices: {
    coll: 'backendServices',
    fields: [
      name,
      region('backend service'),
      description,
      {
        key: 'loadBalancingScheme',
        path: 'loadBalancingScheme',
        label: 'Load balancing scheme',
        type: 'select',
        options: SCHEMES,
        createOnly: true,
        initial: 'EXTERNAL_MANAGED',
      },
      {
        key: 'protocol',
        path: 'protocol',
        label: 'Protocol',
        type: 'select',
        options: ['HTTP', 'HTTPS', 'HTTP2'],
        initial: 'HTTP',
      },
      {
        key: 'backends',
        path: 'backends',
        label: 'Backends',
        type: 'refs',
        targets: ['networkEndpointGroups'],
        hint: 'Network endpoint groups to balance across. A new one balances by RATE; edit balancing in JSON.',
      },
      {
        key: 'healthChecks',
        path: 'healthChecks',
        label: 'Health check',
        type: 'ref',
        targets: ['healthChecks'],
        asList: true,
        emptyOption: 'None (every endpoint is healthy)',
      },
      {
        key: 'timeoutSec',
        path: 'timeoutSec',
        label: 'Timeout (seconds)',
        type: 'number',
        hint: 'Empty for 30.',
        check: (v) =>
          Number(v) < 1
            ? invalid('timeoutSec', v, 'Must be greater than or equal to 1.')
            : undefined,
      },
      {
        key: 'sessionAffinity',
        path: 'sessionAffinity',
        label: 'Session affinity',
        type: 'select',
        options: ['NONE', 'CLIENT_IP', 'GENERATED_COOKIE', 'HEADER_FIELD', 'HTTP_COOKIE'],
        emptyOption: 'Default (NONE)',
      },
      {
        key: 'localityLbPolicy',
        path: 'localityLbPolicy',
        label: 'Locality load balancing policy',
        type: 'select',
        options: ['ROUND_ROBIN', 'LEAST_REQUEST', 'RING_HASH', 'RANDOM', 'MAGLEV'],
        emptyOption: 'Default (ROUND_ROBIN)',
        show: (v) => v.loadBalancingScheme !== 'EXTERNAL',
      },
      { key: 'enableCdn', path: 'enableCdn', label: 'Cloud CDN', type: 'checkbox' },
      headers('customRequestHeaders', 'customRequestHeaders', 'Custom request headers'),
      headers('customResponseHeaders', 'customResponseHeaders', 'Custom response headers'),
      { key: 'logging', path: 'logConfig.enable', label: 'Access logs', type: 'checkbox' },
    ],
  },
  backendBuckets: {
    coll: 'backendBuckets',
    fields: [
      name,
      description,
      {
        key: 'bucketName',
        path: 'bucketName',
        label: 'Cloud Storage bucket',
        type: 'text',
        required: true,
        mono: true,
      },
      { key: 'enableCdn', path: 'enableCdn', label: 'Cloud CDN', type: 'checkbox' },
      headers('customResponseHeaders', 'customResponseHeaders', 'Custom response headers'),
    ],
  },
  healthChecks: {
    coll: 'healthChecks',
    fields: [
      name,
      region('health check'),
      description,
      {
        key: 'type',
        path: 'type',
        label: 'Protocol',
        type: 'select',
        options: HC_TYPES,
        initial: 'HTTP',
      },
      {
        key: 'portSpecification',
        path: (v) => `${hcBlock(v)}.portSpecification`,
        label: 'Port specification',
        type: 'select',
        options: ['USE_FIXED_PORT', 'USE_NAMED_PORT', 'USE_SERVING_PORT'],
        emptyOption: 'Default (the port below)',
      },
      {
        key: 'port',
        path: (v) => `${hcBlock(v)}.port`,
        label: 'Port',
        type: 'number',
        show: (v) => v.portSpecification !== 'USE_SERVING_PORT',
        initial: '80',
        check: between('port', 1, 65535),
      },
      {
        key: 'requestPath',
        path: (v) => `${hcBlock(v)}.requestPath`,
        label: 'Request path',
        type: 'text',
        mono: true,
        show: isHttp,
        hint: 'Empty for /.',
      },
      {
        key: 'host',
        path: (v) => `${hcBlock(v)}.host`,
        label: 'Host header',
        type: 'text',
        mono: true,
        show: isHttp,
      },
      {
        key: 'response',
        path: (v) => `${hcBlock(v)}.response`,
        label: 'Expected response',
        type: 'text',
        hint: 'Empty accepts any; otherwise the response body must start with it.',
      },
      {
        key: 'checkIntervalSec',
        path: 'checkIntervalSec',
        label: 'Check interval (seconds)',
        type: 'number',
        initial: '5',
        check: between('checkIntervalSec', 1, 300),
      },
      {
        key: 'timeoutSec',
        path: 'timeoutSec',
        label: 'Timeout (seconds)',
        type: 'number',
        initial: '5',
        check: (v, all) =>
          all.checkIntervalSec !== '' && Number(v) > Number(all.checkIntervalSec)
            ? invalid(
                'timeoutSec',
                v,
                'Timeout sec must be less than or equal to check interval sec.',
              )
            : undefined,
      },
      {
        key: 'healthyThreshold',
        path: 'healthyThreshold',
        label: 'Healthy threshold',
        type: 'number',
        initial: '2',
        check: between('healthyThreshold', 1, 10, 'Thresholds must be between 1 and 10.'),
      },
      {
        key: 'unhealthyThreshold',
        path: 'unhealthyThreshold',
        label: 'Unhealthy threshold',
        type: 'number',
        initial: '2',
        check: between('unhealthyThreshold', 1, 10, 'Thresholds must be between 1 and 10.'),
      },
    ],
    finish: (body, v) => {
      // Exactly one protocol block: the type's.
      const keep = hcBlock(v)
      for (const b of Object.values(HC_BLOCK)) if (b !== keep) delete body[b]
      const block = isObj(body[keep]) ? { ...body[keep] } : {}
      if (v.portSpecification === 'USE_SERVING_PORT') delete block.port
      body[keep] = block
    },
  },
  sslCertificates: {
    coll: 'sslCertificates',
    fields: [
      name,
      region('certificate'),
      description,
      {
        key: 'type',
        path: 'type',
        label: 'Type',
        type: 'select',
        options: ['SELF_MANAGED', 'MANAGED'],
        initial: 'SELF_MANAGED',
        createOnly: true,
      },
      {
        key: 'certificate',
        path: 'certificate',
        label: 'Certificate (PEM)',
        type: 'textarea',
        mono: true,
        required: true,
        createOnly: true,
        show: (v) => v.type !== 'MANAGED',
        hint: 'The leaf certificate first, then its chain.',
      },
      {
        key: 'privateKey',
        path: 'privateKey',
        label: 'Private key (PEM)',
        type: 'textarea',
        mono: true,
        required: true,
        createOnly: true,
        show: (v) => v.type !== 'MANAGED',
      },
      {
        key: 'domains',
        path: 'managed.domains',
        label: 'Domains',
        type: 'lines',
        mono: true,
        required: true,
        createOnly: true,
        show: (v) => v.type === 'MANAGED',
        hint: 'One per line. The emulator’s CA issues the certificate once DNS for every domain points at a forwarding rule that uses it.',
        check: (v) => {
          const bad = lines(v as string).find((d) => d.includes('*') || !d.includes('.'))
          return bad
            ? invalid(
                'managed.domains',
                bad,
                'Domain must be a fully qualified domain name without wildcards.',
              )
            : undefined
        },
      },
    ],
  },
  sslPolicies: {
    coll: 'sslPolicies',
    fields: [
      name,
      region('SSL policy'),
      description,
      {
        key: 'profile',
        path: 'profile',
        label: 'Profile',
        type: 'select',
        options: ['COMPATIBLE', 'MODERN', 'RESTRICTED', 'CUSTOM'],
        initial: 'COMPATIBLE',
      },
      {
        key: 'minTlsVersion',
        path: 'minTlsVersion',
        label: 'Minimum TLS version',
        type: 'select',
        options: ['TLS_1_0', 'TLS_1_1', 'TLS_1_2'],
        initial: 'TLS_1_0',
      },
      {
        key: 'customFeatures',
        path: 'customFeatures',
        label: 'Custom features',
        type: 'lines',
        mono: true,
        show: (v) => v.profile === 'CUSTOM',
        required: invalid(
          'customFeatures',
          '',
          'At least one custom feature is required with the CUSTOM profile.',
        ),
        hint: 'Cipher suites, one per line, e.g. TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256.',
      },
    ],
  },
}
