import { describe, expect, it } from 'vitest'

import { applyMerge, mergeDiff, parseRef, port } from './api'
import {
  createBody,
  initialValues,
  patchBody,
  patchValues,
  readValues,
  runSetCalls,
  setBody,
  setCalls,
  SPECS,
  validate,
  type Matcher,
  type Values,
} from './spec'
import { parseHeaders, splitUrl } from './UrlMap'

const link = (p: string) => `https://www.googleapis.com/compute/v1/projects/p/${p}`

describe('merge patches', () => {
  it('diffs objects recursively, nulling removed fields', () => {
    const cur = { a: 1, b: { c: 1, d: 2 }, e: [1], f: 'x' }
    const next = { a: 1, b: { c: 3 }, e: [1, 2], g: true }
    const d = mergeDiff(cur, next)
    expect(d).toEqual({ b: { c: 3, d: null }, e: [1, 2], f: null, g: true })
    expect(applyMerge(cur, d)).toEqual(next)
    expect(mergeDiff(cur, cur)).toEqual({})
  })
})

describe('references', () => {
  it('parses resource paths and selfLinks', () => {
    expect(parseRef(link('regions/us-east1/urlMaps/m'))).toEqual({
      project: 'p',
      region: 'us-east1',
      coll: 'urlMaps',
      name: 'm',
    })
    expect(parseRef('projects/p/global/backendBuckets/b')?.coll).toBe('backendBuckets')
    expect(parseRef('projects/p/zones/z/networkEndpointGroups/n')).toBeUndefined()
  })

  it('shows one port, not a range', () => {
    expect(port({ name: 'f', portRange: '8080-8080' })).toBe('8080')
    expect(port({ name: 'f', portRange: '80' })).toBe('80')
  })
})

describe('health checks', () => {
  const spec = SPECS.healthChecks

  it('writes the protocol block of the type only', () => {
    const v = { ...initialValues(spec), name: 'hc', type: 'TCP', port: '5432' }
    expect(createBody(spec)(v, { httpHealthCheck: { requestPath: '/' } })).toEqual({
      name: 'hc',
      type: 'TCP',
      tcpHealthCheck: { port: 5432 },
      checkIntervalSec: 5,
      timeoutSec: 5,
      healthyThreshold: 2,
      unhealthyThreshold: 2,
    })
  })

  it('drops the port with USE_SERVING_PORT', () => {
    const cur = {
      name: 'hc',
      type: 'HTTP',
      httpHealthCheck: { port: 80, requestPath: '/healthz' },
      fingerprint: 'fp',
    }
    const v = readValues(spec, cur)
    expect(v.requestPath).toBe('/healthz')
    expect(patchBody(spec, cur)({ ...v, portSpecification: 'USE_SERVING_PORT' }, {})).toEqual({
      httpHealthCheck: { port: null, portSpecification: 'USE_SERVING_PORT' },
      fingerprint: 'fp',
    })
  })

  it('validates with the API’s messages', () => {
    const v = { ...initialValues(spec), name: '', checkIntervalSec: '5', timeoutSec: '10' }
    expect(validate(spec, v)).toEqual({
      name: "Invalid value for field 'resource.name': ''. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'",
      timeoutSec:
        "Invalid value for field 'resource.timeoutSec': '10'. Timeout sec must be less than or equal to check interval sec.",
    })
  })
})

describe('backend services', () => {
  const spec = SPECS.backendServices
  const cur = {
    name: 'api',
    selfLink: link('global/backendServices/api'),
    healthChecks: [link('global/healthChecks/hc')],
    backends: [{ group: link('zones/z/networkEndpointGroups/a'), balancingMode: 'UTILIZATION' }],
    protocol: 'HTTP',
    timeoutSec: 30,
  }

  it('patches nothing when nothing changed', () => {
    expect(patchBody(spec, cur)(readValues(spec, cur), {})).toEqual({})
  })

  it('keeps a backend’s settings and adds a NEG by rate', () => {
    const v = readValues(spec, cur)
    expect(v.healthChecks).toBe('projects/p/global/healthChecks/hc')
    const body = patchBody(spec, cur)(
      {
        ...v,
        backends: [...(v.backends as string[]), 'projects/p/zones/z/networkEndpointGroups/b'],
        healthChecks: '',
      },
      {},
    )
    expect(body).toEqual({
      backends: [
        cur.backends[0],
        {
          group: 'projects/p/zones/z/networkEndpointGroups/b',
          balancingMode: 'RATE',
          maxRatePerEndpoint: 100,
        },
      ],
      healthChecks: null,
    })
  })
})

describe('URL map routing', () => {
  const spec = SPECS.urlMaps
  const cur = {
    name: 'm',
    fingerprint: 'fp',
    defaultService: link('global/backendBuckets/static'),
    hostRules: [{ hosts: ['a.test'], pathMatcher: 'app', description: 'kept' }],
    pathMatchers: [
      {
        name: 'app',
        defaultService: link('global/backendBuckets/static'),
        pathRules: [
          { paths: ['/api/*'], service: link('global/backendServices/api') },
          { paths: ['/old'], urlRedirect: { pathRedirect: '/new' } },
        ],
        routeRules: [{ priority: 1, service: link('global/backendServices/api') }],
      },
    ],
  }

  it('reads matchers with their hosts and service path rules', () => {
    expect(readValues(spec, cur).routing).toEqual([
      {
        name: 'app',
        hosts: 'a.test',
        defaultService: 'projects/p/global/backendBuckets/static',
        pathRules: '/api/* = global/backendServices/api',
      },
    ])
  })

  it('patches a path rule, keeping what the form does not edit', () => {
    const v = readValues(spec, cur)
    const routing = v.routing as Matcher[]
    const body = patchBody(spec, cur)(
      {
        ...v,
        routing: [
          {
            ...routing[0]!,
            pathRules: `${routing[0]!.pathRules}\n/img/*, /css/* = global/backendBuckets/static`,
          },
          {
            name: 'b',
            hosts: 'b.test, *.b.test',
            defaultService: 'projects/p/global/backendServices/api',
            pathRules: '',
          },
        ],
      },
      {},
    )
    expect(body.fingerprint).toBe('fp')
    expect(body.hostRules).toEqual([
      cur.hostRules[0],
      { hosts: ['b.test', '*.b.test'], pathMatcher: 'b' },
    ])
    expect(body.pathMatchers).toEqual([
      {
        ...cur.pathMatchers[0],
        pathRules: [
          cur.pathMatchers[0]!.pathRules[0],
          { paths: ['/img/*', '/css/*'], service: 'global/backendBuckets/static' },
          cur.pathMatchers[0]!.pathRules[1],
        ],
      },
      { name: 'b', defaultService: 'projects/p/global/backendServices/api' },
    ])
    // The JSON editor's body reads back into the same form.
    expect(patchValues(spec, cur)(body, v).routing).toHaveLength(2)
  })

  it('explains a malformed path rule', () => {
    const v = {
      ...readValues(spec, cur),
      routing: [{ name: 'app', hosts: 'a.test', defaultService: '', pathRules: '/api/*' }],
    }
    expect(validate(spec, v).routing).toMatch(/Invalid path rule "\/api\/\*" in path matcher app/)
  })
})

describe('target SSL proxies', () => {
  const spec = SPECS.targetSslProxies
  const CM = '//certificatemanager.googleapis.com/projects/p/locations/global/certificateMaps/main'
  const cur = {
    name: 'ssl',
    selfLink: link('global/targetSslProxies/ssl'),
    service: link('global/backendServices/tcp'),
    sslCertificates: [link('global/sslCertificates/a')],
    proxyHeader: 'NONE',
  }
  /** edit is the body an edit form sends after changing values. */
  const edit = (r: Record<string, unknown>, change: Values) =>
    setBody(spec, r)({ ...readValues(spec, r), ...change }, {})

  it('sends nothing for an unchanged form', () => {
    expect(setCalls(spec, cur, edit(cur, {}))).toEqual([])
    const mapped = {
      ...cur,
      sslCertificates: undefined,
      certificateMap: CM,
      sslPolicy: link('global/sslPolicies/modern'),
    }
    expect(setCalls(spec, mapped, edit(mapped, {}))).toEqual([])
  })

  it('sends one set call for each changed field', () => {
    expect(
      setCalls(
        spec,
        cur,
        edit(cur, {
          service: 'projects/p/global/backendServices/ssl',
          proxyHeader: 'PROXY_V1',
          sslPolicy: 'projects/p/global/sslPolicies/modern',
        }),
      ),
    ).toEqual([
      {
        method: 'setBackendService',
        body: { service: 'projects/p/global/backendServices/ssl' },
      },
      { method: 'setProxyHeader', body: { proxyHeader: 'PROXY_V1' } },
      { method: 'setSslPolicy', body: { sslPolicy: 'projects/p/global/sslPolicies/modern' } },
    ])
  })

  it('sets a certificate map before clearing the certificates', () => {
    expect(setCalls(spec, cur, edit(cur, { sslCertificates: [], certificateMap: CM }))).toEqual([
      { method: 'setCertificateMap', body: { certificateMap: CM } },
      { method: 'setSslCertificates', body: { sslCertificates: [] } },
    ])
  })

  it('sets certificates before clearing the certificate map', () => {
    const mapped = { ...cur, sslCertificates: undefined, certificateMap: CM }
    expect(
      setCalls(
        spec,
        mapped,
        edit(mapped, {
          sslCertificates: ['projects/p/global/sslCertificates/b'],
          certificateMap: '',
        }),
      ),
    ).toEqual([
      {
        method: 'setSslCertificates',
        body: { sslCertificates: ['projects/p/global/sslCertificates/b'] },
      },
      { method: 'setCertificateMap', body: {} },
    ])
  })

  it('stops at the first call that fails', async () => {
    const sent: string[] = []
    const calls = setCalls(
      spec,
      cur,
      edit(cur, { proxyHeader: 'PROXY_V1', sslCertificates: [], certificateMap: CM }),
    )
    await expect(
      runSetCalls(calls, (c) => {
        sent.push(c.method)
        return c.method === 'setCertificateMap'
          ? Promise.reject(new Error('no such map'))
          : Promise.resolve()
      }),
    ).rejects.toThrow('no such map')
    expect(sent).toEqual(['setProxyHeader', 'setCertificateMap'])
  })

  it('needs SSL certificates or a certificate map', () => {
    const v = {
      ...initialValues(spec),
      name: 'ssl',
      service: 'projects/p/global/backendServices/tcp',
    }
    expect(validate(spec, v)).toEqual({
      sslCertificates:
        "Invalid value for field 'resource.sslCertificates': ''. At least one SSL certificate or a certificate map must be specified.",
    })
    expect(validate(spec, { ...v, certificateMap: CM })).toEqual({})
    expect(createBody(spec)({ ...v, certificateMap: CM }, {})).toEqual({
      name: 'ssl',
      service: 'projects/p/global/backendServices/tcp',
      certificateMap: CM,
      proxyHeader: 'NONE',
    })
  })
})

describe('target TCP proxies', () => {
  const spec = SPECS.targetTcpProxies
  const cur = {
    name: 'tcp',
    selfLink: link('global/targetTcpProxies/tcp'),
    description: 'kept',
    service: link('global/backendServices/tcp'),
    proxyHeader: 'NONE',
  }
  const edit = (change: Values) => setBody(spec, cur)({ ...readValues(spec, cur), ...change }, {})

  it('sends no call for an unchanged field', () => {
    expect(setCalls(spec, cur, edit({}))).toEqual([])
  })

  it('sends setBackendService and setProxyHeader for the fields that changed', () => {
    expect(setCalls(spec, cur, edit({ service: 'projects/p/global/backendServices/tls' }))).toEqual(
      [{ method: 'setBackendService', body: { service: 'projects/p/global/backendServices/tls' } }],
    )
    expect(
      setCalls(
        spec,
        cur,
        edit({ service: 'projects/p/global/backendServices/tls', proxyHeader: 'PROXY_V1' }),
      ),
    ).toEqual([
      { method: 'setBackendService', body: { service: 'projects/p/global/backendServices/tls' } },
      { method: 'setProxyHeader', body: { proxyHeader: 'PROXY_V1' } },
    ])
  })

  it('creates with service, proxy header and description, and needs a backend service', () => {
    const v = { ...initialValues(spec, 'us-east1'), name: 'east', description: 'raw' }
    expect(validate(spec, v)).toEqual({
      service: "Required field 'resource.service' not specified",
    })
    const service = 'projects/p/regions/us-east1/backendServices/tcp'
    expect(createBody(spec)({ ...v, service }, {})).toEqual({
      name: 'east',
      description: 'raw',
      service,
      proxyHeader: 'NONE',
    })
  })
})

describe('test a URL', () => {
  it('splits URLs and headers', () => {
    expect(splitUrl('https://a.test:8443/x?y=1')).toEqual({
      scheme: 'https',
      host: 'a.test:8443',
      path: '/x?y=1',
    })
    expect(splitUrl('a.test/x')).toEqual({ host: 'a.test', path: '/x' })
    expect(splitUrl('a.test')).toEqual({ host: 'a.test', path: '/' })
    expect(parseHeaders('X-A: 1\nx-b:2')).toEqual({ headers: { 'X-A': '1', 'x-b': '2' } })
    expect(parseHeaders('nope').bad).toBe('nope')
  })
})
