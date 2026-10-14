import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, expect, it, vi } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import {
  withMember,
  type Denial,
  type Policy,
  type Role,
  type ServiceAccount,
  type ServiceAccountKey,
} from './api'
import { conditionFlag, customRoleRecipe, policyRecipe, serviceAccountRecipe } from './copy'
import { patchBody as rolePatchBody, roleMask } from './RoleForm'
import { patchBody as accountPatchBody } from './ServiceAccountForm'

const P = 'alpha-project'
const SA = `ci-bot@${P}.iam.gserviceaccount.com`

// A small in-memory IAM: service accounts and keys, roles, policies on the
// Project, a service account and a bucket with etags, an OAuth2 token
// endpoint that issues a token for any Principal, testIamPermissions
// answered from the bindings, and the admin API's enforcement level.
let accounts: ServiceAccount[] = []
let deleted: ServiceAccount[] = []
let keys: Record<string, ServiceAccountKey[]> = {}
let custom: Role[] = []
let policies: Record<string, Policy> = {}
let tokens: Record<string, string> = {}
let denials: Denial[] = []
let etag = 0
let requests: {
  method: string
  path: string
  search: URLSearchParams
  body: unknown
  auth: string | null
}[] = []

const PREDEFINED: Role[] = [
  {
    name: 'roles/viewer',
    title: 'Viewer',
    stage: 'GA',
    includedPermissions: ['resourcemanager.projects.get', 'storage.buckets.get'],
  },
  {
    name: 'roles/storage.admin',
    title: 'Storage Admin',
    stage: 'GA',
    includedPermissions: ['storage.buckets.get', 'storage.buckets.list', 'storage.objects.get'],
  },
  {
    name: 'roles/iam.serviceAccountTokenCreator',
    title: 'Service Account Token Creator',
    stage: 'GA',
    includedPermissions: ['iam.serviceAccounts.getAccessToken'],
  },
]
const UNIVERSE = [
  'resourcemanager.projects.get',
  'resourcemanager.projects.getIamPolicy',
  'storage.buckets.get',
  'storage.buckets.list',
  'storage.objects.get',
  'iam.serviceAccounts.getAccessToken',
  'pubsub.topics.publish',
]

const LEVEL_AUDIT = 'Audit: calls IAM would deny are allowed and logged.'

const err = (code: number, status: string, message: string) =>
  HttpResponse.json({ error: { code, status, message } }, { status: code })

const account = (id: string, over: Partial<ServiceAccount> = {}): ServiceAccount => {
  const email = `${id}@${P}.iam.gserviceaccount.com`
  return {
    name: `projects/${P}/serviceAccounts/${email}`,
    projectId: P,
    uniqueId: `10${id.length}4242`,
    email,
    ...over,
  }
}

const denial = (over: Partial<Denial>): Denial => ({
  id: 'd',
  principal: 'user:someone@example.com',
  permission: 'storage.buckets.get',
  resource: '//storage.googleapis.com/projects/_/buckets/b',
  level: 'audit',
  count: 1,
  firstTime: '2026-10-13T10:00:00Z',
  lastTime: '2026-10-13T11:00:00Z',
  project: P,
  granted: false,
  ...over,
})

const policyOf = (key: string): Policy => policies[key] ?? { version: 1, etag: 'ACAB' }

/** setPolicy stores p under key, enforcing the etag as the API does. */
function setPolicy(key: string, p: Policy) {
  const cur = policyOf(key)
  if (p.etag && p.etag !== cur.etag) {
    return err(
      409,
      'ABORTED',
      'There were concurrent policy changes. Please retry the whole read-modify-write with exponential backoff.',
    )
  }
  for (const b of p.bindings ?? []) {
    for (const m of b.members ?? []) {
      if (!/^(user|serviceAccount|group):/.test(m))
        return err(400, 'INVALID_ARGUMENT', `Invalid member: ${m}`)
    }
  }
  const out = {
    ...p,
    bindings: (p.bindings ?? []).filter((b) => b.members?.length),
    etag: `e${++etag}`,
  }
  policies[key] = out
  return HttpResponse.json(out)
}

/** granted is what principal holds on the resource whose policy is key (plus the Project's). */
function granted(principal: string, key: string, perms: string[]) {
  const roles = [...(policyOf(key).bindings ?? []), ...(policyOf('project').bindings ?? [])]
    .filter((b) => b.members?.includes(principal))
    .map((b) => b.role)
  const all = [...PREDEFINED, ...custom]
  return perms.filter((p) =>
    roles.some((r) => all.find((x) => x.name === r)?.includedPermissions?.includes(p)),
  )
}

async function record(request: Request, path: string) {
  const url = new URL(request.url)
  const text = request.method === 'GET' ? '' : await request.text()
  let body: unknown = undefined
  try {
    body = text ? JSON.parse(text) : undefined
  } catch {
    body = text
  }
  requests.push({
    method: request.method,
    path,
    search: url.searchParams,
    body,
    auth: request.headers.get('authorization'),
  })
  return { url, body }
}

const sent = (method: string, path: RegExp) =>
  requests.filter((r) => r.method === method && path.test(r.path))

const principalOf = (auth: string | null) => tokens[auth?.replace(/^Bearer /, '') ?? ''] ?? ''

beforeEach(() => {
  requests = []
  etag = 0
  tokens = {}
  fake.info.services = ['iam', 'gcs', 'pubsub']
  fake.info.iamMode = 'audit'
  accounts = [
    account('ci-bot', { displayName: 'CI bot', description: 'Runs CI' }),
    account('web-app', { displayName: 'Web', disabled: true }),
  ]
  deleted = [account('old-one')]
  keys = {
    [SA]: [
      {
        name: `projects/${P}/serviceAccounts/${SA}/keys/aaa111`,
        keyType: 'USER_MANAGED',
        keyOrigin: 'GOOGLE_PROVIDED',
        validAfterTime: '2026-10-01T00:00:00Z',
      },
      {
        name: `projects/${P}/serviceAccounts/${SA}/keys/sys999`,
        keyType: 'SYSTEM_MANAGED',
      },
    ],
  }
  custom = [
    {
      name: `projects/${P}/roles/bucketReader`,
      title: 'Bucket reader',
      stage: 'GA',
      includedPermissions: ['storage.buckets.get'],
      etag: 'r1',
    },
    { name: `projects/${P}/roles/gone`, title: 'Gone', stage: 'ALPHA', deleted: true, etag: 'r2' },
  ]
  policies = {
    project: {
      version: 1,
      etag: 'p1',
      bindings: [
        { role: 'roles/viewer', members: [`serviceAccount:${SA}`, 'user:alice@example.com'] },
      ],
    },
  }

  denials = [
    denial({
      id: 'd1',
      principal: `serviceAccount:${SA}`,
      permission: 'pubsub.topics.publish',
      resource: `//pubsub.googleapis.com/projects/${P}/topics/orders`,
      service: 'pubsub',
      method: `POST /v1/projects/${P}/topics/orders:publish`,
      count: 2,
      role: 'roles/pubsub.publisher',
      roleTitle: 'Pub/Sub Publisher',
    }),
    denial({
      id: 'd2',
      principal: 'user:bob@example.com',
      permission: 'storage.buckets.get',
      resource: '//storage.googleapis.com/projects/_/buckets/assets',
      service: 'gcs',
      method: 'GET /storage/v1/b/assets',
      level: 'enforce',
      role: 'roles/viewer',
      roleTitle: 'Viewer',
    }),
    denial({
      id: 'd3',
      principal: 'user:carol@example.com',
      permission: 'storage.objects.get',
      resource: '//storage.googleapis.com/projects/_/buckets/elsewhere/objects/o',
      project: 'other-project',
      role: 'roles/storage.objectViewer',
    }),
  ]

  server.use(
    http.get('*/_emu/v1/iam/denials', ({ request }) => {
      const project = new URL(request.url).searchParams.get('project')
      requests.push({
        method: 'GET',
        path: '/_emu/v1/iam/denials',
        search: new URL(request.url).searchParams,
        body: undefined,
        auth: null,
      })
      return HttpResponse.json({
        denials: denials
          .filter((d) => !project || d.project === project)
          .map((d) => ({
            ...d,
            granted:
              d.project === P &&
              !!policyOf('project').bindings?.some(
                (b) => b.role === d.role && !b.condition && b.members?.includes(d.principal),
              ),
          })),
      })
    }),
    http.delete('*/_emu/v1/iam/denials', async ({ request }) => {
      await record(request, '/_emu/v1/iam/denials')
      denials = []
      return HttpResponse.json({ status: 'cleared' })
    }),
    http.put('*/_emu/v1/iam/enforcement', async ({ request }) => {
      const { body } = await record(request, '/_emu/v1/iam/enforcement')
      fake.info.iamMode = (body as { level: typeof fake.info.iamMode }).level
      return HttpResponse.json({ level: fake.info.iamMode })
    }),
    http.post('*/oauth2/token', async ({ request }) => {
      const { body } = await record(request, '/oauth2/token')
      const form = new URLSearchParams(body as string)
      const p = form.get('refresh_token') ?? ''
      if (accounts.some((a) => a.disabled && p === `serviceAccount:${a.email}`)) {
        return HttpResponse.json(
          {
            error: 'invalid_grant',
            error_description: `service account ${p.slice(15)} is disabled`,
          },
          { status: 400 },
        )
      }
      const tok = `tok-${Object.keys(tokens).length + 1}`
      tokens[tok] = p
      return HttpResponse.json({ access_token: tok, token_type: 'Bearer', expires_in: 3599 })
    }),
    http.post('*/oauth2/revoke', async ({ request }) => {
      await record(request, '/oauth2/revoke')
      return HttpResponse.json({})
    }),
    http.all(/\/cloudresourcemanager\/v1\/projects\/([^/:]+):(\w+)$/, async ({ request }) => {
      const verb = /:(\w+)$/.exec(new URL(request.url).pathname)![1]
      const { body } = await record(request, `crm:${verb}`)
      if (verb === 'getIamPolicy') return HttpResponse.json(policyOf('project'))
      if (verb === 'setIamPolicy') return setPolicy('project', (body as { policy: Policy }).policy)
      const perms = (body as { permissions: string[] }).permissions
      return HttpResponse.json({
        permissions: granted(principalOf(request.headers.get('authorization')), 'project', perms),
      })
    }),
    http.all(/\/storage\/v1\/b\/([^/]+)\/iam(\/testPermissions)?$/, async ({ request }) => {
      const url = new URL(request.url)
      const bucket = decodeURIComponent(/\/b\/([^/]+)\/iam/.exec(url.pathname)![1]!)
      const { body } = await record(
        request,
        `gcs:${bucket}${url.pathname.endsWith('testPermissions') ? ':test' : ''}`,
      )
      if (bucket === 'missing') return err(404, 'NOT_FOUND', 'The specified bucket does not exist.')
      if (url.pathname.endsWith('testPermissions')) {
        return HttpResponse.json({
          permissions: granted(
            principalOf(request.headers.get('authorization')),
            `b/${bucket}`,
            url.searchParams.getAll('permissions'),
          ),
        })
      }
      if (request.method === 'PUT') return setPolicy(`b/${bucket}`, body as Policy)
      return HttpResponse.json({
        ...policyOf(`b/${bucket}`),
        kind: 'storage#policy',
        resourceId: `projects/_/buckets/${bucket}`,
      })
    }),
    http.get('*/storage/v1/b', () => HttpResponse.json({ items: [{ name: 'assets' }] })),
    http.all(/\/iam\/v1\/(.*)$/, async ({ request }) => {
      const url = new URL(request.url)
      const path = decodeURIComponent(url.pathname.replace(/^.*\/iam\/v1\//, ''))
      const { body } = await record(request, path)
      const m = request.method
      const b = body as Record<string, unknown>
      if (path === 'roles') return HttpResponse.json({ roles: PREDEFINED })
      if (path.startsWith('roles/')) {
        const r = PREDEFINED.find((x) => x.name === path)
        return r
          ? HttpResponse.json(r)
          : err(404, 'NOT_FOUND', `The role named ${path} was not found.`)
      }
      if (path === 'permissions:queryTestablePermissions') {
        return HttpResponse.json({ permissions: UNIVERSE.map((name) => ({ name })) })
      }
      let mm = /^projects\/([^/]+)\/roles(?:\/([^/:]+))?(?::(\w+))?$/.exec(path)
      if (mm) {
        const [, , id, verb] = mm
        if (!id && m === 'GET') return HttpResponse.json({ roles: custom })
        if (!id) {
          const r = {
            ...(b.role as Role),
            name: `projects/${P}/roles/${String(b.roleId)}`,
            etag: 'new',
          }
          custom.push(r)
          return HttpResponse.json(r)
        }
        const r = custom.find((x) => x.name.endsWith(`/roles/${id}`))
        if (!r)
          return err(404, 'NOT_FOUND', `The role named projects/${P}/roles/${id} was not found.`)
        if (verb === 'undelete') r.deleted = false
        else if (m === 'DELETE') r.deleted = true
        else if (m === 'PATCH') Object.assign(r, b)
        return HttpResponse.json(r)
      }
      mm = /^projects\/-\/serviceAccounts\/([^/:]+):undelete$/.exec(path)
      if (mm) {
        const sa = deleted.find((a) => a.uniqueId === mm![1])
        if (!sa) return err(404, 'NOT_FOUND', `Account deleted: ${mm[1]}`)
        accounts.push(sa)
        return HttpResponse.json({ restoredAccount: sa })
      }
      mm =
        /^projects\/([^/]+)\/serviceAccounts(?:\/([^/:]+))?(?::(\w+))?(?:\/keys(?::(upload))?(?:\/([^/:]+))?(?::(\w+))?)?$/.exec(
          path,
        )
      if (!mm) return err(400, 'INVALID_ARGUMENT', `bad path ${path}`)
      const [, , email, verb, upload, keyId, keyVerb] = mm
      if (!email && m === 'GET') return HttpResponse.json({ accounts })
      if (!email) {
        const sa = account(String(b.accountId), b.serviceAccount as Partial<ServiceAccount>)
        if (accounts.some((a) => a.email === sa.email)) {
          return err(
            409,
            'ALREADY_EXISTS',
            `Service account ${String(b.accountId)} already exists within project projects/${P}.`,
          )
        }
        accounts.push(sa)
        return HttpResponse.json(sa)
      }
      const sa = accounts.find((a) => a.email === email)
      if (!sa)
        return err(
          404,
          'NOT_FOUND',
          `Service account projects/${P}/serviceAccounts/${email} not found.`,
        )
      if (path.includes('/keys')) {
        const list = (keys[email] ??= [])
        if (upload) {
          const k = {
            name: `${sa.name}/keys/up1`,
            keyType: 'USER_MANAGED' as const,
            keyOrigin: 'USER_PROVIDED',
          }
          list.push(k)
          return HttpResponse.json(k)
        }
        if (!keyId && m === 'GET') return HttpResponse.json({ keys: list })
        if (!keyId) {
          const k = { name: `${sa.name}/keys/new123456789abc`, keyType: 'USER_MANAGED' as const }
          list.push(k)
          return HttpResponse.json({
            ...k,
            privateKeyData: btoa(JSON.stringify({ type: 'service_account', client_email: email })),
          })
        }
        const k = list.find((x) => x.name.endsWith(`/keys/${keyId}`))!
        if (keyVerb) k.disabled = keyVerb === 'disable'
        if (m === 'DELETE') keys[email] = list.filter((x) => x !== k)
        return HttpResponse.json({})
      }
      if (verb === 'disable' || verb === 'enable') {
        sa.disabled = verb === 'disable'
        return HttpResponse.json({})
      }
      if (verb === 'getIamPolicy') return HttpResponse.json(policyOf(`sa/${email}`))
      if (verb === 'setIamPolicy') return setPolicy(`sa/${email}`, (b as { policy: Policy }).policy)
      if (m === 'DELETE') {
        accounts = accounts.filter((a) => a !== sa)
        deleted.push(sa)
        return HttpResponse.json({})
      }
      if (m === 'PATCH') {
        Object.assign(sa, b.serviceAccount as object)
        return HttpResponse.json(sa)
      }
      return HttpResponse.json(sa)
    }),
  )
})

it('says how to enable IAM when it is off', async () => {
  fake.info.services = ['gcs']
  renderApp(`/iam?project=${P}`)
  expect(
    await screen.findByRole('heading', { name: /Service iam is not enabled/ }),
  ).toBeInTheDocument()
})

it('asks for a Project before showing policies', async () => {
  renderApp('/iam')
  expect(await screen.findByText(/Choose a Project/)).toBeInTheDocument()
})

it('switches the IAM enforcement level through the admin API', async () => {
  const user = userEvent.setup()
  renderApp(`/iam?project=${P}`)
  const level = await screen.findByLabelText('IAM enforcement level')
  expect(level).toHaveValue('audit')
  await user.selectOptions(level, 'enforce')
  expect(
    await screen.findByText(
      /IAM enforcement level is now enforce\. Enforce: calls IAM would deny fail/,
    ),
  ).toBeInTheDocument()
  expect(sent('PUT', /enforcement$/)[0]!.body).toEqual({ level: 'enforce' })
  expect(level).toHaveValue('enforce')
})

it('shows the Project policy and grants access with the API’s validation messages', async () => {
  const user = userEvent.setup()
  renderApp(`/iam?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Bindings' })
  const [row] = within(table).getAllByTestId('binding')
  expect(row).toHaveTextContent('roles/viewer')
  expect(row).toHaveTextContent('Viewer')
  expect(row).toHaveTextContent('user:alice@example.com')

  await user.click(screen.getByRole('button', { name: 'Grant access' }))
  const editor = screen.getByRole('region', { name: 'Grant access' })
  await user.click(within(editor).getByRole('button', { name: 'Save' }))
  expect(
    await within(editor).findByText('Role must be specified in a binding.'),
  ).toBeInTheDocument()
  expect(within(editor).getByText('Add at least one member.')).toBeInTheDocument()

  await user.type(within(editor).getByLabelText('Role'), 'roles/storage.admin')
  await user.type(within(editor).getByLabelText('Members'), 'bob@example.com')
  await user.click(within(editor).getByRole('button', { name: 'Save' }))
  expect(await within(editor).findByText('Invalid member: bob@example.com')).toBeInTheDocument()

  await user.clear(within(editor).getByLabelText('Members'))
  await user.type(within(editor).getByLabelText('Members'), 'user:bob@example.com')
  await user.type(
    within(editor).getByLabelText('Condition expression'),
    'request.time < timestamp("2030-01-01T00:00:00Z")',
  )
  await user.click(within(editor).getByRole('button', { name: 'Save' }))
  expect(await within(editor).findByText('Condition title must be specified.')).toBeInTheDocument()
  await user.type(within(editor).getByLabelText('Condition title'), 'until 2030')
  await user.click(within(editor).getByRole('button', { name: 'Save' }))

  await waitFor(() => expect(within(table).getAllByTestId('binding')).toHaveLength(2))
  const [set] = sent('POST', /^crm:setIamPolicy$/)
  expect(set!.body).toEqual({
    policy: {
      version: 1,
      etag: 'p1',
      bindings: [
        policies.project!.bindings![0],
        {
          role: 'roles/storage.admin',
          members: ['user:bob@example.com'],
          condition: {
            title: 'until 2030',
            expression: 'request.time < timestamp("2030-01-01T00:00:00Z")',
          },
        },
      ],
    },
  })
  expect(within(table).getAllByTestId('binding')[1]).toHaveTextContent('until 2030')
})

it('edits and removes bindings, and reports a concurrent change', async () => {
  const user = userEvent.setup()
  renderApp(`/iam?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Edit binding roles/viewer' }))
  const editor = screen.getByRole('region', { name: 'Edit binding roles/viewer' })
  const members = within(editor).getByLabelText('Members')
  expect(members).toHaveValue(`serviceAccount:${SA}\nuser:alice@example.com`)

  // Someone else changes the policy meanwhile: the etag no longer matches.
  policies.project = { ...policies.project!, etag: 'p2' }
  await user.clear(members)
  await user.type(members, 'user:alice@example.com')
  await user.click(within(editor).getByRole('button', { name: 'Save' }))
  expect(
    await within(editor).findByText(/There were concurrent policy changes/),
  ).toBeInTheDocument()

  // The policy is read again, so a retry carries the new etag.
  await waitFor(() => expect(sent('POST', /^crm:getIamPolicy$/).length).toBeGreaterThan(1))
  await user.click(within(editor).getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(screen.queryByRole('region', { name: /Edit binding/ })).toBeNull())
  expect(sent('POST', /^crm:setIamPolicy$/).at(-1)!.body).toMatchObject({
    policy: {
      etag: 'p2',
      bindings: [{ role: 'roles/viewer', members: ['user:alice@example.com'] }],
    },
  })

  await user.click(screen.getByRole('button', { name: 'Remove binding roles/viewer' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Remove binding' }),
  )
  expect(await screen.findByText(/No bindings on this project/)).toBeInTheDocument()
})

it('edits the whole policy as JSON', async () => {
  const user = userEvent.setup()
  renderApp(`/iam?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Edit policy JSON' }))
  const text = screen.getByLabelText('Policy')
  await user.clear(text)
  await user.click(text)
  await user.paste(
    '{"etag":"p1","bindings":[{"role":"roles/viewer","members":["group:devs@example.com"]}]}',
  )
  await user.click(screen.getByRole('button', { name: 'Save policy' }))
  expect(await screen.findByText('group:devs@example.com')).toBeInTheDocument()
  expect(sent('POST', /^crm:setIamPolicy$/)[0]!.body).toEqual({
    policy: {
      etag: 'p1',
      bindings: [{ role: 'roles/viewer', members: ['group:devs@example.com'] }],
    },
  })
})

it('shows and edits the policy of a bucket chosen by kind and name', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/iam?project=${P}`)
  await user.selectOptions(await screen.findByLabelText('Resource type'), 'bucket')
  await user.click(screen.getByRole('button', { name: 'Show policy' }))
  expect(await screen.findByText('Enter the bucket.')).toBeInTheDocument()
  await user.type(screen.getByLabelText('Bucket'), 'assets')
  await user.click(screen.getByRole('button', { name: 'Show policy' }))
  expect(router.state.location.search).toContain('kind=bucket')
  expect(router.state.location.search).toContain('name=assets')
  expect(await screen.findByText(/No bindings on this cloud storage bucket/)).toBeInTheDocument()

  await user.click(screen.getByRole('button', { name: 'Grant access' }))
  const editor = screen.getByRole('region', { name: 'Grant access' })
  await user.type(within(editor).getByLabelText('Role'), 'roles/storage.admin')
  await user.type(within(editor).getByLabelText('Members'), 'user:bob@example.com')
  await user.click(within(editor).getByRole('button', { name: 'Save' }))
  expect(await screen.findByRole('table', { name: 'Bindings' })).toHaveTextContent(
    'user:bob@example.com',
  )
  const [put] = sent('PUT', /^gcs:assets$/)
  expect(put!.body).toMatchObject({
    etag: 'ACAB',
    kind: 'storage#policy',
    bindings: [{ role: 'roles/storage.admin', members: ['user:bob@example.com'] }],
  })
})

it('shows the API’s error for a resource that does not exist', async () => {
  renderApp(`/iam?project=${P}&kind=bucket&name=missing`)
  expect(await screen.findByText('The specified bucket does not exist.')).toBeInTheDocument()
})

it('lists service accounts, filters them and undeletes one by unique ID', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/iam/serviceAccounts?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Service accounts' })
  const rows = within(table).getAllByTestId('service-account')
  expect(rows).toHaveLength(2)
  expect(rows[1]).toHaveTextContent('Disabled')

  await user.type(screen.getByLabelText('Filter'), 'web')
  expect(router.state.location.search).toContain('q=web')
  expect(within(table).getAllByTestId('service-account')).toHaveLength(1)

  await user.click(screen.getByRole('button', { name: 'Undelete' }))
  const dialog = await screen.findByRole('dialog')
  await user.type(within(dialog).getByLabelText('Unique ID'), 'nope')
  await user.click(within(dialog).getByRole('button', { name: 'Undelete' }))
  expect(await within(dialog).findByText('Account deleted: nope')).toBeInTheDocument()
  await user.clear(within(dialog).getByLabelText('Unique ID'))
  await user.type(within(dialog).getByLabelText('Unique ID'), deleted[0]!.uniqueId!)
  await user.click(within(dialog).getByRole('button', { name: 'Undelete' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe(
      `/iam/serviceAccounts/${encodeURIComponent(`old-one@${P}.iam.gserviceaccount.com`)}`,
    ),
  )
})

it('creates a service account with the API’s validation messages', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/iam/create-service-account?project=${P}`)
  await user.type(await screen.findByLabelText('Service account ID'), 'Bad')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(
    await screen.findByText(/Invalid account ID: "Bad"\. The account ID must be between 6 and 30/),
  ).toBeInTheDocument()

  await user.clear(screen.getByLabelText('Service account ID'))
  await user.type(screen.getByLabelText('Service account ID'), 'ci-bot')
  await user.type(screen.getByLabelText('Display name'), 'Second')
  await user.click(screen.getByRole('tab', { name: 'JSON' }))
  expect(JSON.parse(screen.getByLabelText<HTMLTextAreaElement>('Request body').value)).toEqual({
    accountId: 'ci-bot',
    serviceAccount: { displayName: 'Second' },
  })
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(await screen.findByText(/Service account ci-bot already exists/)).toBeInTheDocument()

  await user.click(screen.getByRole('tab', { name: 'Form' }))
  await user.clear(screen.getByLabelText('Service account ID'))
  await user.type(screen.getByLabelText('Service account ID'), 'deployer')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() =>
    expect(router.state.location.pathname).toBe(
      `/iam/serviceAccounts/${encodeURIComponent(`deployer@${P}.iam.gserviceaccount.com`)}`,
    ),
  )
  expect(await screen.findByLabelText('Service account details')).toHaveTextContent('Second')
})

it('edits, disables, enables and deletes a service account', async () => {
  const user = userEvent.setup()
  const base = `/iam/serviceAccounts/${encodeURIComponent(SA)}`
  const { router } = renderApp(`${base}?project=${P}`)
  expect(await screen.findByLabelText('Service account details')).toHaveTextContent('Runs CI')

  await user.click(screen.getByRole('link', { name: 'Edit service account' }))
  await user.clear(await screen.findByLabelText('Description'))
  await user.type(screen.getByLabelText('Description'), 'Deploys')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(router.state.location.pathname).toBe(base))
  expect(sent('PATCH', /ci-bot@/)[0]!.body).toEqual({
    serviceAccount: { description: 'Deploys' },
    updateMask: 'description',
  })

  await user.click(await screen.findByRole('button', { name: 'Disable' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Disable' }),
  )
  expect(await screen.findByRole('button', { name: 'Enable' })).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Enable' }))
  expect(await screen.findByRole('button', { name: 'Disable' })).toBeInTheDocument()
  expect(sent('POST', /:disable$/)).toHaveLength(1)
  expect(sent('POST', /:enable$/)).toHaveLength(1)

  await user.click(screen.getByRole('button', { name: 'Delete service account' }))
  const dialog = await screen.findByRole('dialog')
  expect(dialog).toHaveTextContent('unique ID 1064242')
  await user.click(within(dialog).getByRole('button', { name: 'Delete service account' }))
  await waitFor(() => expect(router.state.location.pathname).toBe('/iam/serviceAccounts'))
  expect(await screen.findByText(/Undelete it with its unique ID 1064242/)).toBeInTheDocument()
})

it('creates and downloads a JSON key, uploads one, and disables and deletes keys', async () => {
  const user = userEvent.setup()
  const createObjectURL = vi.fn((_: Blob) => 'blob:key')
  URL.createObjectURL = createObjectURL
  URL.revokeObjectURL = vi.fn()
  const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
  renderApp(`/iam/serviceAccounts/${encodeURIComponent(SA)}/keys?project=${P}`)
  const table = await screen.findByRole('table', { name: 'Keys' })
  expect(within(table).getAllByTestId('key')[1]).toHaveTextContent('Google-managed')
  expect(screen.queryByRole('button', { name: 'Delete key sys999' })).toBeNull()

  await user.click(screen.getByRole('button', { name: 'Create JSON key' }))
  await waitFor(() => expect(click).toHaveBeenCalled())
  const file = JSON.parse(await createObjectURL.mock.calls[0]![0].text()) as Record<string, string>
  expect(file).toEqual({ type: 'service_account', client_email: SA })
  expect(
    await screen.findByText(/Created key new123456789abc and downloaded its key file/),
  ).toBeInTheDocument()
  expect(sent('POST', /keys$/)[0]!.body).toEqual({
    privateKeyType: 'TYPE_GOOGLE_CREDENTIALS_FILE',
    keyAlgorithm: 'KEY_ALG_RSA_2048',
  })
  click.mockRestore()

  await user.click(screen.getByRole('button', { name: 'Upload key' }))
  const dialog = await screen.findByRole('dialog')
  await user.type(within(dialog).getByLabelText('Certificate (PEM)'), 'PEM')
  await user.click(within(dialog).getByRole('button', { name: 'Upload' }))
  expect(await screen.findByText('Uploaded key up1.')).toBeInTheDocument()
  expect(sent('POST', /keys:upload$/)[0]!.body).toEqual({ publicKeyData: btoa('PEM') })

  await user.click(screen.getByRole('button', { name: 'Disable key aaa111' }))
  expect(await screen.findByRole('button', { name: 'Enable key aaa111' })).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Delete key aaa111' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete key' }),
  )
  await waitFor(() =>
    expect(screen.queryByRole('button', { name: 'Delete key aaa111' })).toBeNull(),
  )
})

it('edits the policy on a service account from its Permissions tab', async () => {
  const user = userEvent.setup()
  renderApp(`/iam/serviceAccounts/${encodeURIComponent(SA)}/permissions?project=${P}`)
  expect(await screen.findByText(/No bindings on this service account/)).toBeInTheDocument()
  expect(screen.getByRole('link', { name: 'Test what it may do' })).toHaveAttribute(
    'href',
    `/iam/simulator?principal=${encodeURIComponent(`serviceAccount:${SA}`)}&project=${P}`,
  )
  await user.click(screen.getByRole('button', { name: 'Grant access' }))
  const editor = screen.getByRole('region', { name: 'Grant access' })
  await user.type(within(editor).getByLabelText('Role'), 'roles/iam.serviceAccountTokenCreator')
  await user.type(within(editor).getByLabelText('Members'), 'user:alice@example.com')
  await user.click(within(editor).getByRole('button', { name: 'Save' }))
  expect(await screen.findByRole('table', { name: 'Bindings' })).toHaveTextContent(
    'Service Account Token Creator',
  )
  expect(sent('POST', /:setIamPolicy$/)[0]!.path).toBe(
    `projects/${P}/serviceAccounts/${SA}:setIamPolicy`,
  )
})

it('lists custom and predefined roles and shows a role’s permissions', async () => {
  const user = userEvent.setup()
  renderApp(`/iam/roles?project=${P}`)
  const customs = await screen.findByRole('table', { name: 'Custom roles' })
  expect(within(customs).getAllByTestId('role')[1]).toHaveTextContent('Deleted')
  const predefined = await screen.findByRole('table', { name: 'Predefined roles' })
  expect(within(predefined).getAllByTestId('role')).toHaveLength(3)

  await user.type(screen.getByLabelText('Filter'), 'storage')
  expect(within(predefined).getAllByTestId('role')).toHaveLength(1)
  expect(screen.getByText('No custom roles match the filter.')).toBeInTheDocument()

  await user.click(within(predefined).getByRole('link', { name: 'Storage Admin' }))
  const perms = await screen.findByRole('list', { name: 'Permissions' })
  expect(within(perms).getAllByRole('listitem')).toHaveLength(3)
  await user.type(screen.getByLabelText('Filter'), 'objects')
  expect(within(perms).getAllByRole('listitem')).toHaveLength(1)
  expect(screen.queryByRole('link', { name: 'Edit role' })).toBeNull()
})

it('creates a custom role with permissions copied from a predefined one', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/iam/create-role?project=${P}`)
  await user.type(await screen.findByLabelText('Role ID'), 'x')
  await user.type(screen.getByLabelText('Permissions'), 'storage.get')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(await screen.findByText(/The role ID "x" is invalid/)).toBeInTheDocument()
  expect(screen.getByText('Permission storage.get is not valid.')).toBeInTheDocument()

  await user.clear(screen.getByLabelText('Role ID'))
  await user.type(screen.getByLabelText('Role ID'), 'deployer')
  await user.type(screen.getByLabelText('Title'), 'Deployer')
  await user.clear(screen.getByLabelText('Permissions'))
  await user.selectOptions(await screen.findByLabelText('Copy a predefined role'), 'roles/viewer')
  expect(screen.getByLabelText('Permissions')).toHaveValue(
    'resourcemanager.projects.get\nstorage.buckets.get',
  )
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() => expect(router.state.location.pathname).toBe('/iam/customRoles/deployer'))
  expect(sent('POST', /roles$/)[0]!.body).toEqual({
    roleId: 'deployer',
    role: {
      title: 'Deployer',
      stage: 'GA',
      includedPermissions: ['resourcemanager.projects.get', 'storage.buckets.get'],
    },
  })
})

it('edits, deletes and undeletes a custom role', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/iam/customRoles/bucketReader?project=${P}`)
  expect(await screen.findByLabelText('Role details')).toHaveTextContent('Bucket reader')
  await user.click(screen.getByRole('link', { name: 'Edit role' }))
  await user.type(await screen.findByLabelText('Permissions'), '\nstorage.objects.get')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(router.state.location.pathname).toBe('/iam/customRoles/bucketReader'))
  const [patch] = sent('PATCH', /roles\/bucketReader$/)
  expect(patch!.search.get('updateMask')).toBe('includedPermissions')
  expect(patch!.body).toEqual({
    etag: 'r1',
    includedPermissions: ['storage.buckets.get', 'storage.objects.get'],
  })

  await user.click(await screen.findByRole('button', { name: 'Delete role' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete role' }),
  )
  await user.click(await screen.findByRole('button', { name: 'Undelete role' }))
  expect(await screen.findByRole('button', { name: 'Delete role' })).toBeInTheDocument()
  expect(sent('POST', /bucketReader:undelete$/)).toHaveLength(1)
})

it('simulates a Principal’s permissions on the Project and on a bucket', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/iam/simulator?project=${P}`)
  const principal = await screen.findByLabelText('Principal')
  await user.type(principal, 'alice@example.com')
  await user.click(screen.getByRole('button', { name: 'Test permissions' }))
  expect(
    await screen.findByText('Enter a Principal as user:EMAIL or serviceAccount:EMAIL.'),
  ).toBeInTheDocument()

  await user.clear(principal)
  await user.type(principal, 'user:alice@example.com')
  await waitFor(() =>
    expect(screen.getByLabelText('Permissions')).toHaveValue(
      'resourcemanager.projects.get\nresourcemanager.projects.getIamPolicy',
    ),
  )
  await user.click(screen.getByRole('button', { name: 'Test permissions' }))
  const results = await screen.findByRole('table', { name: 'Results' })
  const rows = within(results).getAllByTestId('result')
  expect(rows[0]).toHaveTextContent('Granted')
  expect(rows[1]).toHaveTextContent('Denied')
  expect(screen.getByText(/holds 1 of 2 permissions/)).toBeInTheDocument()
  expect(router.state.location.search).toContain('principal=user%3Aalice%40example.com')

  // The token is minted for the Principal, used for testIamPermissions
  // alone, and revoked.
  const [mint] = sent('POST', /^\/oauth2\/token$/)
  expect(new URLSearchParams(mint!.body as string).get('refresh_token')).toBe(
    'user:alice@example.com',
  )
  const [test] = sent('POST', /^crm:testIamPermissions$/)
  expect(test!.auth).toBe('Bearer tok-1')
  await waitFor(() => expect(sent('POST', /^\/oauth2\/revoke$/)).toHaveLength(1))
  expect(requests.filter((r) => r.auth).map((r) => r.path)).toEqual(['crm:testIamPermissions'])

  await user.selectOptions(screen.getByLabelText('Resource type'), 'bucket')
  await user.type(screen.getByLabelText('Bucket'), 'assets')
  await waitFor(() =>
    expect(screen.getByLabelText('Permissions')).toHaveValue(
      'storage.buckets.get\nstorage.buckets.list\nstorage.objects.get',
    ),
  )
  await user.click(screen.getByRole('button', { name: 'Test permissions' }))
  await waitFor(() => expect(screen.getByText(/holds 1 of 3 permissions/)).toBeInTheDocument())
  const [gcs] = sent('GET', /^gcs:assets:test$/)
  expect(gcs!.search.getAll('permissions')).toEqual([
    'storage.buckets.get',
    'storage.buckets.list',
    'storage.objects.get',
  ])
})

it('explains why a disabled service account cannot be simulated', async () => {
  const user = userEvent.setup()
  renderApp(
    `/iam/simulator?project=${P}&principal=${encodeURIComponent(`serviceAccount:web-app@${P}.iam.gserviceaccount.com`)}`,
  )
  await waitFor(() => expect(screen.getByLabelText('Permissions')).not.toHaveValue(''))
  await user.click(screen.getByRole('button', { name: 'Test permissions' }))
  expect(await screen.findByText(/service account web-app@.* is disabled/)).toBeInTheDocument()
  expect(sent('POST', /testIamPermissions$/)).toHaveLength(0)
})

it('writes only the changed fields of a service account and a role', () => {
  const sa = account('x', { displayName: 'A', description: 'B' })
  expect(accountPatchBody(sa, { displayName: 'A', description: 'C' }, {})).toEqual({
    serviceAccount: { description: 'C' },
    updateMask: 'description',
  })
  const role: Role = { name: 'projects/p/roles/r', title: 'T', stage: 'GA', etag: 'e' }
  const body = rolePatchBody(
    role,
    { title: 'T2', description: '', stage: 'BETA', permissions: '' },
    {},
  )
  expect(body).toEqual({ etag: 'e', title: 'T2', stage: 'BETA' })
  expect(roleMask(body)).toEqual(['title', 'stage'])
})

it('lists the calls IAM would deny and grants the smallest role in one click', async () => {
  const user = userEvent.setup()
  renderApp('/iam/audit')
  expect(await screen.findByRole('heading', { name: 'IAM audit' })).toBeInTheDocument()
  expect(screen.getByRole('link', { name: 'Audit' })).toHaveAttribute('aria-current', 'page')
  expect(screen.getByRole('note')).toHaveTextContent(LEVEL_AUDIT)
  expect(await screen.findByText('3 denied calls; 3 still missing.')).toBeInTheDocument()
  const table = screen.getByRole('table', { name: 'Denied calls' })
  const [publish, bucket, other] = within(table).getAllByTestId('denial')
  expect(publish).toHaveTextContent(`serviceAccount:${SA}`)
  expect(publish).toHaveTextContent('pubsub.topics.publish')
  expect(publish).toHaveTextContent(`//pubsub.googleapis.com/projects/${P}/topics/orders`)
  expect(publish).toHaveTextContent(`POST /v1/projects/${P}/topics/orders:publish`)
  expect(publish).toHaveTextContent('Allowed by audit')
  expect(publish).toHaveTextContent('2 times')
  expect(within(publish!).getByRole('link', { name: /roles\/pubsub\.publisher/ })).toHaveAttribute(
    'href',
    '/iam/roles/pubsub.publisher',
  )
  expect(bucket).toHaveTextContent('Failed')
  expect(other).toHaveTextContent('Policy of other-project')

  await user.click(
    within(publish!).getByRole('button', {
      name: `Grant roles/pubsub.publisher to serviceAccount:${SA} on Project ${P}`,
    }),
  )
  expect(
    await screen.findByText(
      `Granted roles/pubsub.publisher to serviceAccount:${SA} on Project ${P}.`,
    ),
  ).toBeInTheDocument()
  await waitFor(() => expect(publish).toHaveTextContent('Granted'))
  expect(within(publish!).queryByRole('button', { name: /^Grant/ })).not.toBeInTheDocument()
  expect(sent('POST', /^crm:setIamPolicy$/)[0]!.body).toEqual({
    policy: {
      version: 1,
      etag: 'p1',
      bindings: [
        policies.project!.bindings![0],
        { role: 'roles/pubsub.publisher', members: [`serviceAccount:${SA}`] },
      ],
    },
  })

  // A role the Project already binds gains the member.
  await user.click(
    within(bucket!).getByRole('button', {
      name: `Grant roles/viewer to user:bob@example.com on Project ${P}`,
    }),
  )
  await waitFor(() => expect(bucket).toHaveTextContent('Granted'))
  expect(policies.project!.bindings![0]!.members).toEqual([
    `serviceAccount:${SA}`,
    'user:alice@example.com',
    'user:bob@example.com',
  ])
  expect(screen.getByText('3 denied calls; 1 still missing.')).toBeInTheDocument()
})

it('filters denied calls by text, status and Project, and clears them', async () => {
  const user = userEvent.setup()
  const { router, queryClient } = renderApp('/iam/audit')
  const rows = () =>
    within(screen.getByRole('table', { name: 'Denied calls' })).getAllByTestId('denial')
  await screen.findByRole('table', { name: 'Denied calls' })
  expect(rows()).toHaveLength(3)

  await user.type(screen.getByLabelText('Filter'), 'storage')
  expect(rows()).toHaveLength(2)
  await waitFor(() => expect(router.state.location.search).toContain('q=storage'))
  await user.clear(screen.getByLabelText('Filter'))
  await user.type(screen.getByLabelText('Filter'), 'nothing-like-this')
  expect(screen.getByText('No denied calls match the filter.')).toBeInTheDocument()
  await user.clear(screen.getByLabelText('Filter'))

  // A grant made elsewhere; the event bridge would mark the denials stale.
  policies.project!.bindings!.push({ role: 'roles/viewer', members: ['user:bob@example.com'] })
  await queryClient.invalidateQueries({ queryKey: ['iam', 'denials'] })
  await router.navigate('/iam/audit?status=granted')
  await waitFor(() => expect(rows()).toHaveLength(1))
  expect(screen.getByLabelText('Status')).toHaveValue('granted')
  await user.selectOptions(screen.getByLabelText('Status'), 'missing')
  await waitFor(() => expect(rows()).toHaveLength(2))
  expect(router.state.location.search).toContain('status=missing')

  await router.navigate(`/iam/audit?project=${P}`)
  expect(await screen.findByText(/on resources of Project/)).toBeInTheDocument()
  expect(await screen.findByText(/^2 denied calls/)).toBeInTheDocument()
  expect(
    sent('GET', /iam\/denials$/)
      .at(-1)!
      .search.get('project'),
  ).toBe(P)

  await user.click(screen.getByRole('button', { name: 'Clear' }))
  const dialog = await screen.findByRole('dialog')
  await user.click(within(dialog).getByRole('button', { name: 'Clear' }))
  expect(await screen.findByText('No denied calls yet.')).toBeInTheDocument()
  expect(sent('DELETE', /iam\/denials$/)).toHaveLength(1)
})

it('says that IAM off logs no denied calls', async () => {
  fake.info.iamMode = 'off'
  denials = []
  renderApp('/iam/audit')
  expect(await screen.findByRole('note')).toHaveTextContent(/IAM is off: calls are not checked/)
  expect(await screen.findByText('No denied calls yet.')).toBeInTheDocument()
})

it('adds a member to a role’s unconditional binding, or a new binding', () => {
  const policy: Policy = {
    etag: 'e',
    bindings: [
      { role: 'roles/viewer', members: ['user:a@x'], condition: { expression: 'true' } },
      { role: 'roles/editor', members: ['user:b@x'] },
    ],
  }
  expect(withMember(policy, 'roles/editor', 'user:c@x').bindings![1]).toEqual({
    role: 'roles/editor',
    members: ['user:b@x', 'user:c@x'],
  })
  expect(withMember(policy, 'roles/editor', 'user:b@x').bindings).toEqual(policy.bindings)
  expect(withMember(policy, 'roles/viewer', 'user:c@x').bindings![2]).toEqual({
    role: 'roles/viewer',
    members: ['user:c@x'],
  })
})

it('copies a service account as gcloud, curl and OpenTofu', async () => {
  const user = userEvent.setup()
  renderApp(`/iam/serviceAccounts/${encodeURIComponent(SA)}?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Copy service account as' }))
  const dialog = await screen.findByRole('dialog', { name: 'Copy service account as' })
  expect(within(dialog).getByLabelText('gcloud for the service account')).toHaveTextContent(
    [
      'gcloud iam service-accounts create ci-bot \\',
      `    --project=${P} \\`,
      "    --display-name='CI bot' \\",
      "    --description='Runs CI'",
    ].join('\n'),
    { normalizeWhitespace: false },
  )

  await user.click(within(dialog).getByRole('tab', { name: 'curl' }))
  const curl = within(dialog).getByLabelText('curl for the service account')
  expect(curl).toHaveTextContent(
    `curl ${location.origin}/iam/v1/projects/${P}/serviceAccounts \\ -X POST`,
  )
  expect(curl).toHaveTextContent('"accountId": "ci-bot"')

  await user.click(within(dialog).getByRole('tab', { name: 'OpenTofu' }))
  const tofu = within(dialog).getByLabelText('OpenTofu for the service account')
  expect(tofu).toHaveTextContent('resource "google_service_account" "ci-bot" {')
  expect(tofu).toHaveTextContent('account_id = "ci-bot"')
})

it('copies a disabled service account and a custom role', () => {
  const r = serviceAccountRecipe(account('web-app', { displayName: 'Web', disabled: true }))
  const email = `web-app@${P}.iam.gserviceaccount.com`
  expect(r.gcloud).toEqual([
    [
      'gcloud iam service-accounts create web-app \\',
      `    --project=${P} \\`,
      '    --display-name=Web',
    ].join('\n'),
    `gcloud iam service-accounts disable ${email} \\\n    --project=${P}`,
  ])
  expect(r.rest).toEqual([
    {
      method: 'POST',
      path: `/iam/v1/projects/${P}/serviceAccounts`,
      body: { accountId: 'web-app', serviceAccount: { displayName: 'Web' } },
    },
    { method: 'POST', path: `/iam/v1/projects/${P}/serviceAccounts/${email}:disable`, body: {} },
  ])
  expect(r.tofu).toContain('  disabled     = true')

  const role = customRoleRecipe({
    name: `projects/${P}/roles/bucketReader`,
    title: 'Bucket reader',
    stage: 'GA',
    includedPermissions: ['storage.buckets.get', 'storage.objects.list'],
    etag: 'r1',
  })
  expect(role.gcloud).toBe(
    [
      'gcloud iam roles create bucketReader \\',
      `    --project=${P} \\`,
      "    --title='Bucket reader' \\",
      '    --permissions=storage.buckets.get,storage.objects.list \\',
      '    --stage=GA',
    ].join('\n'),
  )
  expect(role.rest).toEqual({
    method: 'POST',
    path: `/iam/v1/projects/${P}/roles`,
    body: {
      roleId: 'bucketReader',
      role: {
        title: 'Bucket reader',
        stage: 'GA',
        includedPermissions: ['storage.buckets.get', 'storage.objects.list'],
      },
    },
  })
  expect(role.tofu).toContain('resource "google_project_iam_custom_role" "bucketReader" {')
})

it('copies an IAM policy one binding at a time, with conditions', () => {
  const expression = 'request.time < timestamp("2027-01-01T00:00:00Z")'
  const policy: Policy = {
    version: 3,
    etag: 'e1',
    bindings: [
      { role: 'roles/viewer', members: ['user:a@x', 'group:g@x'] },
      {
        role: 'roles/viewer',
        members: ['user:c@x'],
        condition: { title: 'Until 2027', expression },
      },
    ],
  }
  const add = (member: string, condition: string) =>
    [
      `gcloud projects add-iam-policy-binding ${P} \\`,
      `    --member=${member} \\`,
      '    --role=roles/viewer \\',
      `    --condition=${condition}`,
    ].join('\n')
  const r = policyRecipe({ project: P, kind: 'project', name: '' }, policy)
  expect(r.gcloud).toEqual([
    add('user:a@x', 'None'),
    add('group:g@x', 'None'),
    add('user:c@x', `'expression=${expression},title=Until 2027'`),
  ])
  expect(r.rest).toEqual({
    method: 'POST',
    path: `/cloudresourcemanager/v1/projects/${P}:setIamPolicy`,
    body: { policy: { version: 3, bindings: policy.bindings } },
  })
  expect(r.tofu).toContain('resource "google_project_iam_binding" "viewer" {')
  expect(r.tofu).toContain(
    [
      'resource "google_project_iam_binding" "viewer_2" {',
      `  project = "${P}"`,
      '  role    = "roles/viewer"',
      '  members = ["user:c@x"]',
      '  condition {',
      '    title      = "Until 2027"',
      '    expression = "request.time < timestamp(\\"2027-01-01T00:00:00Z\\")"',
      '  }',
      '}',
    ].join('\n'),
  )

  // A bucket's policy is PUT to its /iam resource, without the API's own fields.
  const b = policyRecipe(
    { project: P, kind: 'bucket', name: 'assets' },
    {
      kind: 'storage#policy',
      resourceId: 'projects/_/buckets/assets',
      etag: 'e',
      bindings: [{ role: 'roles/storage.admin', members: ['user:a@x'] }],
    },
  )
  expect(b.gcloud).toEqual([
    [
      'gcloud storage buckets add-iam-policy-binding gs://assets \\',
      '    --member=user:a@x \\',
      '    --role=roles/storage.admin',
    ].join('\n'),
  ])
  expect(b.rest).toEqual({
    method: 'PUT',
    path: '/storage/v1/b/assets/iam',
    body: { bindings: [{ role: 'roles/storage.admin', members: ['user:a@x'] }] },
  })
  expect(b.tofu).toContain('resource "google_storage_bucket_iam_binding" "storage_admin" {')

  // A regional secret's binding names its location.
  const s = policyRecipe(
    { project: P, kind: 'secret', name: 'us-central1/db' },
    { bindings: [{ role: 'roles/secretmanager.secretAccessor', members: ['user:a@x'] }] },
  )
  expect(s.gcloud).toEqual([
    [
      'gcloud secrets add-iam-policy-binding db \\',
      `    --project=${P} \\`,
      '    --location=us-central1 \\',
      '    --member=user:a@x \\',
      '    --role=roles/secretmanager.secretAccessor',
    ].join('\n'),
  ])
  expect(s.tofu).toContain('resource "google_secret_manager_regional_secret_iam_binding"')

  // TLS policies have no per-binding command or resource; the request still copies.
  const tls = policyRecipe({ project: P, kind: 'serverTlsPolicy', name: 'global/p' }, policy)
  expect(tls.gcloud).toBeUndefined()
  expect(tls.tofu).toBeUndefined()
  expect(tls.rest).toMatchObject({ method: 'POST' })

  expect(conditionFlag({ title: 't', expression: 'a in ["x", "y"]' })).toBe(
    '^;^expression=a in ["x", "y"];title=t',
  )
})
