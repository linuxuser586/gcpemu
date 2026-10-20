import {
  block,
  expr,
  gcloud,
  keyValues,
  omit,
  resource,
  shQuote,
  type FlagValue,
  type HclBody,
  type HclValue,
  type Recipe,
} from '@/lib/copyAs'

import {
  isObj,
  lastSeg,
  port,
  relPath,
  scopePath,
  text,
  type Coll,
  type Ref,
  type Resource,
} from './api'

// Copy as (FR-UI-015) for the load-balancing resources: gcloud compute,
// the compute v1 insert request and the google_compute_* resource, global
// or regional. Cloud CDN origins are backend services and buckets, so the
// CDN view uses these too.

type Obj = Record<string, unknown>

/** OUTPUT are the fields the API sets, which an insert request leaves out. */
const OUTPUT = [
  'id',
  'kind',
  'selfLink',
  'selfLinkWithId',
  'creationTimestamp',
  'fingerprint',
  'labelFingerprint',
  'region',
  'status',
  'usedBy',
]

/** OUTPUT_OF are a collection's own output-only fields. */
const OUTPUT_OF: Partial<Record<Coll, readonly string[]>> = {
  forwardingRules: ['pscConnectionId', 'pscConnectionStatus', 'baseForwardingRule'],
  // Security policies are set with setSecurityPolicy, not on insert.
  backendServices: ['securityPolicy', 'edgeSecurityPolicy'],
  backendBuckets: ['edgeSecurityPolicy'],
  sslCertificates: ['expireTime', 'subjectAlternativeNames', 'selfManaged'],
  sslPolicies: ['enabledFeatures', 'warnings'],
}

/** PRIVATE_KEY stands in for a certificate's private key, which the API never returns. */
const PRIVATE_KEY = 'PRIVATE_KEY_PEM (the API does not return the private key)'

const LINK = /^https?:\/\/\S*\/projects\//

/** rel writes the API's resource URLs in v as the relative paths gcloud and OpenTofu take. */
function rel(v: unknown): unknown {
  if (typeof v === 'string') return LINK.test(v) ? relPath(v) : v
  if (Array.isArray(v)) return v.map(rel)
  if (isObj(v)) return Object.fromEntries(Object.entries(v).map(([k, x]) => [k, rel(x)]))
  return v
}

const obj = (v: unknown): Obj => (isObj(v) ? v : {})
const objs = (v: unknown): Obj[] => (Array.isArray(v) ? v.filter(isObj) : [])
const strs = (v: unknown): string[] =>
  Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : []

/** regionOf is the region of a regional resource path; undefined for a global one. */
const regionOf = (link: unknown) => /\/regions\/([^/]+)\//.exec(text(link))?.[1]

/** insertBody is a resource as its insert request sends it: no output-only fields, relative links. */
export function insertBody(coll: Coll, r: Obj): Obj {
  const b = rel(omit(r, [...OUTPUT, ...(OUTPUT_OF[coll] ?? [])])) as Obj
  if (isObj(b.cdnPolicy)) b.cdnPolicy = omit(b.cdnPolicy, ['signedUrlKeyNames'])
  if (coll === 'sslCertificates') {
    if (isObj(b.managed)) b.managed = omit(b.managed, ['status', 'domainStatus'])
    if (b.certificate) b.privateKey = PRIVATE_KEY
  }
  return b
}

// ---- gcloud ----

const scope = (region: string): Record<string, FlagValue> =>
  region ? { region } : { global: true }

/** repeat adds a flag per value, for flags gcloud takes once per value. */
const repeat = (cmd: string, flag: string, values: readonly string[]) =>
  [cmd, ...values.map((v) => `--${flag}=${shQuote(v)}`)].join(' \\\n    ')

/** onOff writes a boolean flag as --flag or --no-flag; unset leaves it out. */
const onOff = (flag: string, v: unknown): Record<string, FlagValue> =>
  typeof v === 'boolean' ? { [v ? flag : `no-${flag}`]: true } : {}

/** cdnFlags are a backend's Cloud CDN flags, which buckets and services share. */
function cdnFlags(b: Obj, service: boolean): Record<string, FlagValue> {
  const p = obj(b.cdnPolicy)
  const key = isObj(p.cacheKeyPolicy) ? p.cacheKeyPolicy : undefined
  return {
    'enable-cdn': b.enableCdn === true,
    'cache-mode': text(p.cacheMode),
    'default-ttl': text(p.defaultTtl),
    'max-ttl': text(p.maxTtl),
    'client-ttl': text(p.clientTtl),
    'serve-while-stale': text(p.serveWhileStale),
    'negative-caching': p.negativeCaching === true,
    'negative-caching-policy': objs(p.negativeCachingPolicy).map(
      (n) => `${text(n.code)}=${text(n.ttl)}`,
    ),
    'bypass-cache-on-request-headers': objs(p.bypassCacheOnRequestHeaders).map((h) =>
      text(h.headerName),
    ),
    'signed-url-cache-max-age': text(p.signedUrlCacheMaxAgeSec),
    ...(service && key
      ? {
          ...onOff('cache-key-include-protocol', key.includeProtocol),
          ...onOff('cache-key-include-host', key.includeHost),
          ...onOff('cache-key-include-query-string', key.includeQueryString),
        }
      : {}),
    'cache-key-query-string-whitelist': strs(key?.queryStringWhitelist),
    'cache-key-query-string-blacklist': service ? strs(key?.queryStringBlacklist) : undefined,
    'cache-key-include-http-header': strs(key?.includeHttpHeaders),
    'cache-key-include-named-cookie': service ? strs(key?.includeNamedCookies) : undefined,
  }
}

/** TARGET_FLAG is the forwarding rule flag of each kind of target proxy. */
const TARGET_FLAG: Record<string, string> = {
  targetHttpProxies: 'target-http-proxy',
  targetHttpsProxies: 'target-https-proxy',
  targetTcpProxies: 'target-tcp-proxy',
  targetSslProxies: 'target-ssl-proxy',
  targetGrpcProxies: 'target-grpc-proxy',
}

function forwardingRuleCmds(p: string, region: string, b: Obj, portRange: string): string[] {
  const target = text(b.target)
  const flag = TARGET_FLAG[/\/(target\w+)\/[^/]+$/.exec(target)?.[1] ?? '']
  const ip = text(b.IPAddress)
  const labels = keyValues(b.labels as Record<string, string> | undefined)
  const cmds = [
    gcloud(['compute', 'forwarding-rules', 'create', text(b.name)], {
      project: p,
      ...scope(region),
      description: text(b.description),
      'load-balancing-scheme': text(b.loadBalancingScheme),
      // A reserved address is named; an ephemeral one is the address itself.
      address: ip.includes('/') ? lastSeg(ip) : ip,
      'ip-protocol': text(b.IPProtocol),
      ports: portRange,
      'network-tier': text(b.networkTier),
      network: lastSeg(text(b.network)),
      subnet: lastSeg(text(b.subnetwork)),
      ...(flag ? { [flag]: lastSeg(target), [`${flag}-region`]: regionOf(target) } : {}),
    }),
  ]
  // gcloud sets a forwarding rule's labels after it is created.
  if (labels.length) {
    cmds.push(
      gcloud(['compute', 'forwarding-rules', 'update', text(b.name)], {
        project: p,
        ...scope(region),
        'update-labels': labels,
      }),
    )
  }
  return cmds
}

const proxyFlags = (b: Obj): Record<string, FlagValue> => ({
  description: text(b.description),
  'url-map': lastSeg(text(b.urlMap)),
  'url-map-region': regionOf(b.urlMap),
})

/** CERT_MAP_URL is the API prefix of a certificate map, which gcloud leaves out. */
const CERT_MAP_URL = /^\/\/certificatemanager\.googleapis\.com\//

/** tlsFlags are the certificate and SSL policy flags of a target HTTPS or SSL proxy. */
function tlsFlags(b: Obj): Record<string, FlagValue> {
  const certs = strs(b.sslCertificates)
  return {
    'ssl-certificates': certs.map(lastSeg),
    'ssl-certificates-region': regionOf(certs[0]),
    'certificate-map': text(b.certificateMap).replace(CERT_MAP_URL, ''),
    'ssl-policy': lastSeg(text(b.sslPolicy)),
    'ssl-policy-region': regionOf(b.sslPolicy),
  }
}

function httpsProxyFlags(b: Obj): Record<string, FlagValue> {
  return {
    ...proxyFlags(b),
    ...tlsFlags(b),
    'server-tls-policy': text(b.serverTlsPolicy).replace(
      /^\/\/networksecurity\.googleapis\.com\//,
      '',
    ),
    'quic-override': text(b.quicOverride),
  }
}

/** sslProxyCmd creates a target SSL proxy, which is global only. */
const sslProxyCmd = (p: string, b: Obj) =>
  gcloud(['compute', 'target-ssl-proxies', 'create', text(b.name)], {
    project: p,
    'backend-service': lastSeg(text(b.service)),
    ...tlsFlags(b),
    // NONE is the default.
    'proxy-header': b.proxyHeader === 'NONE' ? undefined : text(b.proxyHeader),
    description: text(b.description),
  })

/** URL_MAP_FLAGS are what url-maps create sets; a map with more is imported instead. */
const URL_MAP_FLAGS = new Set(['name', 'description', 'defaultService'])

function urlMapCmd(p: string, region: string, b: Obj): string {
  const name = text(b.name)
  if (Object.keys(b).every((k) => URL_MAP_FLAGS.has(k))) {
    const def = text(b.defaultService)
    const bucket = def.includes('/backendBuckets/')
    return gcloud(['compute', 'url-maps', 'create', name], {
      project: p,
      ...scope(region),
      description: text(b.description),
      'default-service': bucket ? undefined : lastSeg(def),
      'default-backend-bucket': bucket ? lastSeg(def) : undefined,
    })
  }
  // Host rules, path matchers and route rules have no flags: import the
  // map, which reads YAML (and so JSON) from stdin.
  const cmd = gcloud(['compute', 'url-maps', 'import', name], { project: p, ...scope(region) })
  return `${cmd} <<'EOF'\n${JSON.stringify(b, null, 2)}\nEOF`
}

function backendServiceCmds(p: string, region: string, b: Obj): string[] {
  const name = text(b.name)
  const hcs = strs(b.healthChecks)
  const log = obj(b.logConfig)
  let create = gcloud(['compute', 'backend-services', 'create', name], {
    project: p,
    ...scope(region),
    description: text(b.description),
    'load-balancing-scheme': text(b.loadBalancingScheme),
    protocol: text(b.protocol),
    'port-name': text(b.portName),
    timeout: text(b.timeoutSec),
    'health-checks': hcs.map(lastSeg),
    'health-checks-region': regionOf(hcs[0]),
    'session-affinity': text(b.sessionAffinity),
    'affinity-cookie-ttl': text(b.affinityCookieTtlSec),
    'locality-lb-policy': text(b.localityLbPolicy),
    'connection-draining-timeout': text(obj(b.connectionDraining).drainingTimeoutSec),
    ...cdnFlags(b, true),
    'enable-logging': log.enable === true,
    'logging-sample-rate': log.enable === true ? text(log.sampleRate) : undefined,
  })
  create = repeat(create, 'custom-request-header', strs(b.customRequestHeaders))
  create = repeat(create, 'custom-response-header', strs(b.customResponseHeaders))
  const cmds = [create]
  // Backends are added one by one after the service exists.
  for (const be of objs(b.backends)) {
    const g = text(be.group)
    const kind = g.includes('/networkEndpointGroups/') ? 'network-endpoint-group' : 'instance-group'
    const loc = /\/(zones|regions)\/([^/]+)\//.exec(g)
    cmds.push(
      gcloud(['compute', 'backend-services', 'add-backend', name], {
        project: p,
        ...scope(region),
        [kind]: lastSeg(g),
        [`${kind}-zone`]: loc?.[1] === 'zones' ? loc[2] : undefined,
        [`${kind}-region`]: loc?.[1] === 'regions' ? loc[2] : undefined,
        'global-network-endpoint-group': kind === 'network-endpoint-group' && !loc,
        description: text(be.description),
        'balancing-mode': text(be.balancingMode),
        'max-rate': text(be.maxRate),
        'max-rate-per-instance': text(be.maxRatePerInstance),
        'max-rate-per-endpoint': text(be.maxRatePerEndpoint),
        'max-connections': text(be.maxConnections),
        'max-connections-per-instance': text(be.maxConnectionsPerInstance),
        'max-connections-per-endpoint': text(be.maxConnectionsPerEndpoint),
        'max-utilization': text(be.maxUtilization),
        'capacity-scaler': text(be.capacityScaler),
      }),
    )
  }
  return cmds
}

/** securityCmd attaches a backend's Cloud Armor policies, which insert does not set. */
function securityCmd(p: string, region: string, coll: Coll, r: Obj): string[] {
  const sec = text(rel(r.securityPolicy))
  const edge = text(rel(r.edgeSecurityPolicy))
  if (!sec && !edge) return []
  const group = coll === 'backendBuckets' ? 'backend-buckets' : 'backend-services'
  return [
    gcloud(['compute', group, 'update', text(r.name)], {
      project: p,
      ...(coll === 'backendBuckets' ? {} : scope(region)),
      'security-policy': lastSeg(sec),
      'edge-security-policy': lastSeg(edge),
    }),
  ]
}

function backendBucketCmd(p: string, b: Obj): string {
  const create = gcloud(['compute', 'backend-buckets', 'create', text(b.name)], {
    project: p,
    description: text(b.description),
    'gcs-bucket-name': text(b.bucketName),
    'compression-mode': text(b.compressionMode),
    ...cdnFlags(b, false),
  })
  return repeat(create, 'custom-response-header', strs(b.customResponseHeaders))
}

/** HC_BLOCK is the protocol block of each health check type. */
const HC_BLOCK: Record<string, string> = {
  HTTP: 'httpHealthCheck',
  HTTPS: 'httpsHealthCheck',
  HTTP2: 'http2HealthCheck',
  TCP: 'tcpHealthCheck',
  SSL: 'sslHealthCheck',
  GRPC: 'grpcHealthCheck',
  GRPC_WITH_TLS: 'grpcTlsHealthCheck',
}

function healthCheckCmd(p: string, region: string, b: Obj): string {
  const type = text(b.type) || 'HTTP'
  const hb = obj(b[HC_BLOCK[type] ?? 'httpHealthCheck'])
  const serving = hb.portSpecification === 'USE_SERVING_PORT'
  return gcloud(
    ['compute', 'health-checks', 'create', type.toLowerCase().replaceAll('_', '-'), text(b.name)],
    {
      project: p,
      ...scope(region),
      description: text(b.description),
      port: serving ? undefined : text(hb.port),
      'port-name': text(hb.portName),
      'use-serving-port': serving,
      'request-path': text(hb.requestPath),
      host: text(hb.host),
      request: text(hb.request),
      response: text(hb.response),
      'proxy-header': text(hb.proxyHeader),
      'grpc-service-name': text(hb.grpcServiceName),
      'check-interval': text(b.checkIntervalSec),
      timeout: text(b.timeoutSec),
      'healthy-threshold': text(b.healthyThreshold),
      'unhealthy-threshold': text(b.unhealthyThreshold),
    },
  )
}

function sslCertificateCmds(p: string, region: string, b: Obj): string[] {
  const managed = obj(b.managed)
  if (b.type === 'MANAGED' || !b.certificate) {
    return [
      gcloud(['compute', 'ssl-certificates', 'create', text(b.name)], {
        project: p,
        ...scope(region),
        description: text(b.description),
        domains: strs(managed.domains),
      }),
    ]
  }
  return [
    `cat > certificate.pem <<'EOF'\n${text(b.certificate).trimEnd()}\nEOF`,
    '# The API does not return the private key: save it as private-key.pem.\n' +
      gcloud(['compute', 'ssl-certificates', 'create', text(b.name)], {
        project: p,
        ...scope(region),
        description: text(b.description),
        certificate: 'certificate.pem',
        'private-key': 'private-key.pem',
      }),
  ]
}

const sslPolicyCmd = (p: string, region: string, b: Obj) =>
  gcloud(['compute', 'ssl-policies', 'create', text(b.name)], {
    project: p,
    ...scope(region),
    description: text(b.description),
    profile: text(b.profile),
    'min-tls-version': text(b.minTlsVersion).replace(/^TLS_(\d)_(\d)$/, '$1.$2'),
    'custom-features': strs(b.customFeatures),
  })

function gcloudCmds(coll: Coll, ref: Ref, r: Obj, b: Obj): string[] {
  const { project: p, region } = ref
  switch (coll) {
    case 'forwardingRules':
      return forwardingRuleCmds(p, region, b, port(r as Resource))
    case 'targetHttpProxies':
      return [
        gcloud(['compute', 'target-http-proxies', 'create', text(b.name)], {
          project: p,
          ...scope(region),
          ...proxyFlags(b),
        }),
      ]
    case 'targetHttpsProxies':
      return [
        gcloud(['compute', 'target-https-proxies', 'create', text(b.name)], {
          project: p,
          ...scope(region),
          ...httpsProxyFlags(b),
        }),
      ]
    case 'targetSslProxies':
      return [sslProxyCmd(p, b)]
    case 'urlMaps':
      return [urlMapCmd(p, region, b)]
    case 'backendServices':
      return [...backendServiceCmds(p, region, b), ...securityCmd(p, region, coll, r)]
    case 'backendBuckets':
      return [backendBucketCmd(p, b), ...securityCmd(p, region, coll, r)]
    case 'healthChecks':
      return [healthCheckCmd(p, region, b)]
    case 'sslCertificates':
      return sslCertificateCmds(p, region, b)
    case 'sslPolicies':
      return [sslPolicyCmd(p, region, b)]
  }
}

// ---- OpenTofu ----

/** TOFU is each collection's resource type, global and regional. */
const TOFU: Record<Coll, readonly [string, string?]> = {
  forwardingRules: ['google_compute_global_forwarding_rule', 'google_compute_forwarding_rule'],
  targetHttpProxies: [
    'google_compute_target_http_proxy',
    'google_compute_region_target_http_proxy',
  ],
  targetHttpsProxies: [
    'google_compute_target_https_proxy',
    'google_compute_region_target_https_proxy',
  ],
  targetSslProxies: ['google_compute_target_ssl_proxy'],
  urlMaps: ['google_compute_url_map', 'google_compute_region_url_map'],
  backendServices: ['google_compute_backend_service', 'google_compute_region_backend_service'],
  backendBuckets: ['google_compute_backend_bucket'],
  healthChecks: ['google_compute_health_check', 'google_compute_region_health_check'],
  sslCertificates: ['google_compute_ssl_certificate', 'google_compute_region_ssl_certificate'],
  sslPolicies: ['google_compute_ssl_policy', 'google_compute_region_ssl_policy'],
}

/**
 * ATTR names the provider's attribute or block of an API field where it
 * is not the field in snake case: the IP fields, and lists of objects
 * the provider writes as a repeated singular block.
 */
const ATTR: Record<string, string> = {
  IPAddress: 'ip_address',
  IPProtocol: 'ip_protocol',
  hostRules: 'host_rule',
  pathMatchers: 'path_matcher',
  pathRules: 'path_rule',
  tests: 'test',
  backends: 'backend',
}

/** MAPS are fields that are map attributes, not nested blocks. */
const MAPS = new Set(['labels'])

const snake = (k: string) => ATTR[k] ?? k.replaceAll(/[A-Z]/g, (c) => `_${c.toLowerCase()}`)

/** hcl writes an API value as the provider's: objects as blocks, lists of objects as repeated blocks. */
function hcl(v: unknown): HclValue {
  if (Array.isArray(v)) {
    return v.length && v.every(isObj) ? v.map((o) => block(hclBody(o))) : v.map(hcl)
  }
  if (isObj(v)) return Object.keys(v).length ? block(hclBody(v)) : undefined
  return v as HclValue
}

function hclBody(o: Obj): HclBody {
  return Object.fromEntries(
    Object.entries(o).map(([k, v]) => [snake(k), MAPS.has(k) ? (v as HclValue) : hcl(v)]),
  )
}

function tofuBlock(coll: Coll, ref: Ref, r: Obj, b: Obj): string | undefined {
  const [global, regional] = TOFU[coll]
  const type = ref.region ? regional : global
  if (!type) return undefined
  const head: HclBody = {
    project: ref.project,
    name: text(b.name),
    region: ref.region || undefined,
  }
  const rest = omit(b, ['name'])
  switch (coll) {
    case 'forwardingRules':
      return resource(type, text(b.name), {
        ...head,
        ...hclBody(rest),
        port_range: port(r as Resource) || undefined,
      })
    case 'targetSslProxies':
      // The provider calls the backend service backend_service.
      return resource(type, text(b.name), {
        ...head,
        ...hclBody(omit(rest, ['service'])),
        backend_service: text(b.service) || undefined,
      })
    case 'healthChecks':
      // The provider tells the protocol by its block.
      return resource(type, text(b.name), { ...head, ...hclBody(omit(rest, ['type'])) })
    case 'backendServices':
    case 'backendBuckets': {
      const drain = obj(rest.connectionDraining).drainingTimeoutSec
      return resource(type, text(b.name), {
        ...head,
        ...hclBody(omit(rest, ['connectionDraining'])),
        connection_draining_timeout_sec: drain as HclValue,
        security_policy: text(rel(r.securityPolicy)) || undefined,
        edge_security_policy: text(rel(r.edgeSecurityPolicy)) || undefined,
      })
    }
    case 'sslCertificates': {
      if (b.type === 'MANAGED' || !b.certificate) {
        if (ref.region) return undefined
        return resource('google_compute_managed_ssl_certificate', text(b.name), {
          ...head,
          description: text(b.description) || undefined,
          managed: block({ domains: strs(obj(b.managed).domains) }),
        })
      }
      return (
        '# The API does not return the private key: save it as private-key.pem.\n' +
        resource(type, text(b.name), {
          ...head,
          description: text(b.description) || undefined,
          certificate: text(b.certificate),
          private_key: expr('file("private-key.pem")'),
        })
      )
    }
    default:
      return resource(type, text(b.name), { ...head, ...hclBody(rest) })
  }
}

/**
 * recipeFor is a load-balancing resource as the gcloud commands, the
 * insert request and the OpenTofu block that create it as it is.
 */
export function recipeFor(ref: Ref, r: Resource): Recipe {
  const coll = ref.coll
  const b = insertBody(coll, r)
  // The request sends the stand-in key; gcloud and OpenTofu read a file.
  const own = coll === 'sslCertificates' ? omit(b, ['privateKey']) : b
  return {
    gcloud: gcloudCmds(coll, ref, r, own),
    rest: {
      method: 'POST',
      path: `/compute/v1/${scopePath(ref.project, ref.region)}/${coll}`,
      body: b,
    },
    tofu: tofuBlock(coll, ref, r, own),
  }
}
