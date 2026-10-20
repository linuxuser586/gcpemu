import { http, HttpResponse } from 'msw'

// A small in-memory Certificate Manager REST API for the view's tests of
// DNS authorizations, trust configs and certificate issuance configs. It
// lists every location ("-"), applies a patch's updateMask, checks a
// trust config's etag and refuses to delete what a certificate uses, as
// the Service does; operations are done at once.

export type Res = Record<string, unknown> & { name: string }

export interface Sent {
  method: string
  path: string
  search: URLSearchParams
  body: unknown
}

const ID_PARAMS: Record<string, string> = {
  dnsAuthorizations: 'dnsAuthorizationId',
  trustConfigs: 'trustConfigId',
  certificateIssuanceConfigs: 'certificateIssuanceConfigId',
}

const err = (code: number, status: string, message: string) =>
  HttpResponse.json({ error: { code, status, message } }, { status: code })

const done = (response: unknown) =>
  HttpResponse.json({ name: 'projects/p/locations/global/operations/op-1', done: true, response })

const camel = (s: string) => s.replace(/_([a-z])/g, (_, c: string) => c.toUpperCase())

/**
 * fakeCertManager serves resources, recording each request in requests;
 * inUse names the resources a certificate uses. computed adds a created
 * resource's output fields.
 */
export function fakeCertManager(
  resources: Res[],
  requests: Sent[],
  {
    inUse = [] as string[],
    computed = (r: Res): Res => r,
  }: { inUse?: string[]; computed?: (r: Res) => Res } = {},
) {
  let etag = 0
  return http.all(
    /\/certificatemanager\/v1\/projects\/[^/]+\/locations\/[^/]+\/(dnsAuthorizations|trustConfigs|certificateIssuanceConfigs)(\/[^/?]+)?(\?.*)?$/,
    async ({ request }) => {
      const url = new URL(request.url)
      const text = request.method === 'GET' ? '' : await request.text()
      const body = (text ? JSON.parse(text) : undefined) as Record<string, unknown> | undefined
      const path = decodeURIComponent(url.pathname.replace(/^.*\/certificatemanager\/v1\//, ''))
      requests.push({ method: request.method, path, search: url.searchParams, body })
      const [, project, location, coll, id] =
        /^projects\/([^/]+)\/locations\/([^/]+)\/([^/]+)(?:\/([^/]+))?$/.exec(path)!
      if (!id) {
        if (request.method === 'GET') {
          const list = resources.filter(
            (r) =>
              r.name.startsWith(`projects/${project}/`) &&
              r.name.includes(`/${coll}/`) &&
              (location === '-' || r.name.includes(`/locations/${location}/`)),
          )
          return HttpResponse.json(list.length ? { [coll!]: list } : {})
        }
        const name = `projects/${project}/locations/${location}/${coll}/${url.searchParams.get(ID_PARAMS[coll!]!)}`
        if (resources.some((r) => r.name === name)) {
          return err(409, 'ALREADY_EXISTS', `Resource ${name} already exists.`)
        }
        const r = computed({ ...body, name })
        if (coll === 'trustConfigs') r.etag = `e-${++etag}`
        resources.push(r)
        return done(r)
      }
      const r = resources.find((x) => x.name === path)
      if (!r) return err(404, 'NOT_FOUND', `Resource '${path}' was not found`)
      if (request.method === 'DELETE') {
        if (inUse.includes(path)) {
          return err(400, 'FAILED_PRECONDITION', `Resource '${path}' is in use by: a certificate`)
        }
        resources.splice(resources.indexOf(r), 1)
        return done({})
      }
      if (request.method === 'PATCH') {
        if (body?.etag && body.etag !== r.etag) {
          return err(
            409,
            'ABORTED',
            `The etag "${body.etag as string}" does not match the current etag of ${path}.`,
          )
        }
        for (const f of (url.searchParams.get('updateMask') ?? '').split(',').map(camel)) {
          if (body && f in body) r[f] = body[f]
          else delete r[f]
        }
        if (coll === 'trustConfigs') r.etag = `e-${++etag}`
        return done(r)
      }
      return HttpResponse.json(r)
    },
  )
}
