import { useMutation, useQuery } from '@tanstack/react-query'
import { ArrowRight, Route } from 'lucide-react'
import { useState, type FormEvent, type ReactNode } from 'react'

import { errorMessage } from '@/api/errors'
import { Field, fieldAria } from '@/components/resource/Field'
import { Mono } from '@/components/resource/DetailList'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import { cn } from '@/lib/utils'

import {
  healthQuery,
  parseRef,
  resourcePath,
  resourceQuery,
  route,
  type LbRoute,
  type Ref,
  type Resource,
  text,
} from './api'
import { HealthBadge } from './Health'
import { RefLink } from './LbLayout'

// A URL map rendered as the route tree the data plane walks (FR-LB-003):
// host rules → path matcher → route rules (by priority) or path rules
// (longest path first) → backend, redirect or route action; and "test a
// URL", which asks the emulator which route and backend would serve a
// request.

type Obj = Record<string, unknown>
const arr = (v: unknown) => (Array.isArray(v) ? (v as Obj[]) : [])
const strs = (v: unknown) => (Array.isArray(v) ? (v as string[]) : [])

/** Action renders what a rule does: its backend, redirect or route action. */
function Action({ rule }: { rule: Obj }) {
  const redirect = rule.urlRedirect as Obj | undefined
  const action = rule.routeAction as Obj | undefined
  const weighted = arr(action?.weightedBackendServices)
  const rewrite = action?.urlRewrite as Obj | undefined
  const parts: ReactNode[] = []
  if (typeof rule.service === 'string') parts.push(<RefLink key="s" link={rule.service} />)
  if (weighted.length) {
    parts.push(
      <span key="w" className="flex flex-wrap gap-x-2">
        {weighted.map((w, i) => (
          <span key={i}>
            <RefLink link={w.backendService as string} />{' '}
            <span className="text-muted-foreground">{text(w.weight ?? 0)}</span>
          </span>
        ))}
      </span>,
    )
  }
  if (redirect) parts.push(<span key="r">redirect {describeRedirect(redirect)}</span>)
  if (rewrite) {
    const to = [rewrite.hostRewrite, rewrite.pathPrefixRewrite, rewrite.pathTemplateRewrite]
      .filter(Boolean)
      .join(' ')
    if (to) parts.push(<span key="u">rewrite to {to}</span>)
  }
  if (!parts.length) return <span className="text-muted-foreground">no action</span>
  return <span className="flex flex-wrap items-center gap-x-3">{parts}</span>
}

function describeRedirect(r: Obj) {
  const to = [
    r.httpsRedirect ? 'https://' : '',
    text(r.hostRedirect),
    text(r.pathRedirect ?? r.prefixRedirect),
  ].join('')
  return `${to || 'same URL'} (${text(r.redirectResponseCode ?? 'MOVED_PERMANENTLY_DEFAULT')})`
}

/** describeMatch renders a route rule's match rules. */
function describeMatch(rr: Obj) {
  return arr(rr.matchRules)
    .map((m) => {
      const p = m.prefixMatch
        ? `prefix ${text(m.prefixMatch)}`
        : m.fullPathMatch
          ? `path ${text(m.fullPathMatch)}`
          : m.regexMatch
            ? `regex ${text(m.regexMatch)}`
            : m.pathTemplateMatch
              ? `template ${text(m.pathTemplateMatch)}`
              : 'any path'
      const h = arr(m.headerMatches).map((x) => {
        const v = x.exactMatch ?? x.prefixMatch ?? x.regexMatch ?? x.suffixMatch
        return x.presentMatch
          ? `${text(x.headerName)} present`
          : `${text(x.headerName)}=${text(v ?? '')}`
      })
      const q = arr(m.queryParameterMatches).map((x) =>
        x.presentMatch
          ? `?${text(x.name)}`
          : `?${text(x.name)}=${text(x.exactMatch ?? x.regexMatch ?? '')}`,
      )
      return [p, ...h, ...q].join(', ')
    })
    .join(' or ')
}

/** Node is one line of the tree, highlighted when the tested request took it. */
function Node({
  label,
  children,
  hit,
  testId,
}: {
  label: ReactNode
  children?: ReactNode
  hit?: boolean
  testId?: string
}) {
  return (
    <li className="flex flex-col gap-1" data-testid={testId} data-hit={hit || undefined}>
      <div
        className={cn(
          'flex flex-wrap items-center gap-2 rounded-md px-2 py-1 text-sm',
          hit && 'bg-primary/10 ring-1 ring-primary',
        )}
      >
        {label}
        {hit && <Badge variant="default">Matched</Badge>}
      </div>
      {children && <ul className="ml-4 flex flex-col gap-1 border-l pl-3">{children}</ul>}
    </li>
  )
}

const samePaths = (a: unknown, b: unknown) => JSON.stringify(strs(a)) === JSON.stringify(strs(b))

/**
 * RouteTree renders a URL map as the tree a request walks. hit marks the
 * route the last tested request took.
 */
export function RouteTree({ map, hit }: { map: Resource; hit?: LbRoute }) {
  const pms = arr(map.pathMatchers)
  const hostRules = arr(map.hostRules)
  const ruleHit = (pm: string) => hit?.pathMatcher === pm
  return (
    <ul aria-label="Route tree" className="flex flex-col gap-1">
      {hostRules.map((hr, i) => {
        const pm = pms.find((p) => p.name === hr.pathMatcher)
        const name = text(hr.pathMatcher ?? '')
        const routeRules = [...arr(pm?.routeRules)].sort(
          (a, b) => Number(a.priority ?? 0) - Number(b.priority ?? 0),
        )
        const pathRules = arr(pm?.pathRules)
        return (
          <Node
            key={i}
            testId="host-rule"
            hit={ruleHit(name)}
            label={
              <>
                <span className="text-muted-foreground">Hosts</span>
                <Mono>{strs(hr.hosts).join(', ')}</Mono>
                <ArrowRight className="size-3.5 text-muted-foreground" aria-hidden />
                <span className="text-muted-foreground">path matcher</span>
                <Mono>{name}</Mono>
              </>
            }
          >
            {routeRules.map((rr, j) => (
              <Node
                key={`r${j}`}
                testId="route-rule"
                hit={
                  ruleHit(name) &&
                  hit?.match === 'routeRule' &&
                  hit.routeRule?.priority === rr.priority
                }
                label={
                  <>
                    <span className="text-muted-foreground">Priority {text(rr.priority ?? 0)}</span>
                    <Mono>{describeMatch(rr)}</Mono>
                    <ArrowRight className="size-3.5 text-muted-foreground" aria-hidden />
                    <Action rule={rr} />
                  </>
                }
              />
            ))}
            {pathRules.map((pr, j) => (
              <Node
                key={`p${j}`}
                testId="path-rule"
                hit={
                  ruleHit(name) &&
                  hit?.match === 'pathRule' &&
                  samePaths(hit.pathRule?.paths, pr.paths)
                }
                label={
                  <>
                    <Mono>{strs(pr.paths).join(', ')}</Mono>
                    <ArrowRight className="size-3.5 text-muted-foreground" aria-hidden />
                    <Action rule={pr} />
                  </>
                }
              />
            ))}
            <Node
              testId="matcher-default"
              hit={ruleHit(name) && hit?.match === 'pathMatcherDefault'}
              label={
                <>
                  <span className="text-muted-foreground">Anything else</span>
                  <ArrowRight className="size-3.5 text-muted-foreground" aria-hidden />
                  {pm ? (
                    <Action
                      rule={{
                        service: pm.defaultService,
                        urlRedirect: pm.defaultUrlRedirect,
                        routeAction: pm.defaultRouteAction,
                      }}
                    />
                  ) : (
                    <span className="text-destructive">no path matcher {name}</span>
                  )}
                </>
              }
            />
          </Node>
        )
      })}
      <Node
        testId="map-default"
        hit={hit?.match === 'urlMapDefault'}
        label={
          <>
            <span className="text-muted-foreground">
              {hostRules.length ? 'Any other host' : 'Every request'}
            </span>
            <ArrowRight className="size-3.5 text-muted-foreground" aria-hidden />
            <Action
              rule={{
                service: map.defaultService,
                urlRedirect: map.defaultUrlRedirect,
                routeAction: map.defaultRouteAction,
              }}
            />
          </>
        }
      />
    </ul>
  )
}

/** parseHeaders reads "Name: value" lines; undefined names a bad line. */
export function parseHeaders(text: string): { headers: Record<string, string>; bad?: string } {
  const headers: Record<string, string> = {}
  for (const line of text
    .split('\n')
    .map((l) => l.trim())
    .filter(Boolean)) {
    const at = line.indexOf(':')
    if (at <= 0) return { headers, bad: line }
    headers[line.slice(0, at).trim()] = line.slice(at + 1).trim()
  }
  return { headers }
}

/** splitUrl reads a full URL, or a host and path, into host and path. */
export function splitUrl(input: string): { scheme?: string; host: string; path: string } {
  const s = input.trim()
  const m = /^(https?):\/\/([^/?#]+)([^#]*)/i.exec(s)
  if (m) return { scheme: m[1]!.toLowerCase(), host: m[2]!, path: m[3] || '/' }
  const slash = s.search(/[/?]/)
  return slash < 0 ? { host: s, path: '/' } : { host: s.slice(0, slash), path: s.slice(slash) }
}

/** TestUrl asks which route and backend would serve a host, path and headers. */
export function TestUrl({
  map,
  mapRef,
  onResult,
}: {
  map: Resource
  mapRef: Ref
  onResult: (r?: LbRoute) => void
}) {
  const firstHost = strs(arr(map.hostRules)[0]?.hosts).find((h) => !h.includes('*')) ?? ''
  const [url, setUrl] = useState(firstHost ? `http://${firstHost}/` : '')
  const [method, setMethod] = useState('GET')
  const [headers, setHeaders] = useState('')
  const [error, setError] = useState<string>()
  const test = useMutation({
    meta: { toast: false },
    mutationFn: route,
    onSuccess: (r) => onResult(r),
    onError: (e) => {
      setError(errorMessage(e))
      onResult(undefined)
    },
  })
  const submit = (e: FormEvent) => {
    e.preventDefault()
    setError(undefined)
    const { scheme, host, path } = splitUrl(url)
    if (!host) {
      setError('Enter a URL such as http://www.example.com/path, or a host and path.')
      return
    }
    const h = parseHeaders(headers)
    if (h.bad) {
      setError(`Write headers as Name: value, one per line (not "${h.bad}").`)
      return
    }
    test.mutate({
      urlMap: resourcePath(mapRef),
      scheme: (scheme ?? 'http') as 'http' | 'https',
      method,
      host,
      path,
      headers: h.headers,
    })
  }
  const r = test.data
  return (
    <Card aria-labelledby="test-url-title">
      <CardTitle id="test-url-title">Test a URL</CardTitle>
      <p className="text-sm text-muted-foreground">
        Which route and backend would serve a request, as the load balancer routes it. Nothing is
        sent to a backend.
      </p>
      <form onSubmit={submit} noValidate className="flex flex-col gap-3">
        <div className="grid gap-3 sm:grid-cols-[1fr_auto]">
          <Field
            id="test-url"
            label="URL"
            hint="A full URL, or a host and path; the query string counts."
          >
            <Input
              className="font-mono"
              spellCheck={false}
              {...fieldAria('test-url', undefined, 'hint')}
              value={url}
              onChange={(e) => setUrl(e.target.value)}
            />
          </Field>
          <Field id="test-method" label="Method">
            <NativeSelect
              id="test-method"
              value={method}
              onChange={(e) => setMethod(e.target.value)}
            >
              {['GET', 'HEAD', 'POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS'].map((m) => (
                <option key={m}>{m}</option>
              ))}
            </NativeSelect>
          </Field>
        </div>
        <Field id="test-headers" label="Headers" hint="Name: value, one per line">
          <Textarea
            rows={2}
            spellCheck={false}
            className="font-mono"
            {...fieldAria('test-headers', undefined, 'hint')}
            value={headers}
            onChange={(e) => setHeaders(e.target.value)}
          />
        </Field>
        <Button type="submit" className="w-fit" disabled={test.isPending}>
          <Route aria-hidden />
          Test
        </Button>
      </form>
      {error && (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      )}
      {r && !error && <RouteResult r={r} />}
    </Card>
  )
}

const MATCHES: Record<LbRoute['match'], string> = {
  routeRule: 'Route rule',
  pathRule: 'Path rule',
  pathMatcherDefault: 'The path matcher’s default',
  urlMapDefault: 'The URL map’s default',
}

/** RouteResult shows a routing decision and the health of its backend. */
function RouteResult({ r }: { r: LbRoute }) {
  const rule = r.routeRule
  const pathRule = r.pathRule
  const weighted = (r.weightedBackendServices ?? []) as Obj[]
  return (
    <div
      role="status"
      aria-label="Route"
      className="flex flex-col gap-2 rounded-md border p-3 text-sm"
    >
      <p>
        <span className="font-medium">{MATCHES[r.match]}</span>
        {r.pathMatcher && (
          <>
            {' '}
            of path matcher <Mono>{r.pathMatcher}</Mono>
          </>
        )}
        {rule && (
          <>
            {' '}
            (priority {text(rule.priority ?? 0)}: {describeMatch(rule)})
          </>
        )}
        {pathRule && <> ({strs(pathRule.paths).join(', ')})</>}
      </p>
      {r.redirect && (
        <p>
          Redirects with <Mono>{r.redirect.code}</Mono> to <Mono>{r.redirect.location}</Mono>
        </p>
      )}
      {r.service && weighted.length === 0 && (
        <p className="flex flex-wrap items-center gap-2">
          Served by <RefLink link={r.service} />
        </p>
      )}
      {weighted.length > 0 && (
        <p className="flex flex-wrap items-center gap-2">
          Split across
          {weighted.map((w, i) => (
            <span key={i}>
              <RefLink link={w.backendService as string} /> ({text(w.weight ?? 0)})
            </span>
          ))}
        </p>
      )}
      <p>
        Reaches the backend as <Mono>{`${r.host}${r.path}`}</Mono>
      </p>
      {weighted.length === 0 && r.service && <BackendHealthSummary link={r.service} />}
    </div>
  )
}

/** BackendHealthSummary shows the endpoints of the backend service that would serve. */
function BackendHealthSummary({ link }: { link: string }) {
  const ref = parseRef(link)
  const bs = useQuery({ ...resourceQuery(ref!), enabled: ref?.coll === 'backendServices' })
  if (ref?.coll !== 'backendServices') return null
  const groups = ((bs.data?.backends ?? []) as Obj[]).map((b) => text(b.group))
  if (bs.data && groups.length === 0) {
    return <p className="text-muted-foreground">The backend service has no backends.</p>
  }
  return (
    <ul aria-label="Backend endpoints" className="flex flex-col gap-1">
      {groups.map((g) => (
        <GroupEndpoints key={g} r={ref} group={g} />
      ))}
    </ul>
  )
}

function GroupEndpoints({ r, group }: { r: Ref; group: string }) {
  const h = useQuery(healthQuery(r, group))
  const eps = h.data ?? []
  return (
    <>
      {eps.length === 0 && h.data && (
        <li className="text-muted-foreground">No endpoints in {group.split('/').pop()}.</li>
      )}
      {eps.map((e) => (
        <li key={`${e.ipAddress}:${e.port}`} className="flex items-center gap-2">
          <Mono>{`${e.ipAddress}:${e.port}`}</Mono>
          <HealthBadge state={e.healthState} />
        </li>
      ))}
    </>
  )
}
