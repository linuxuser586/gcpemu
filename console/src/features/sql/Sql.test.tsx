import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, expect, it } from 'vitest'

import { fake } from '@/test/handlers'
import { renderApp } from '@/test/render'
import { server } from '@/test/setup'

import type { BackupRun, Database, DatabaseInstance, ExecuteSqlResponse, User } from './api'

const P = 'alpha-project'
const API = `*/sqladmin/v1/projects/${P}`
const base = '/sql/instances/main'

function instance(over: Partial<DatabaseInstance> = {}): DatabaseInstance {
  return {
    name: 'main',
    project: P,
    region: 'us-central1',
    gceZone: 'us-central1-a',
    databaseVersion: 'POSTGRES_17',
    state: 'RUNNABLE',
    connectionName: `${P}:us-central1:main`,
    ipAddresses: [{ type: 'PRIMARY', ipAddress: '172.30.0.4' }],
    settings: {
      tier: 'db-custom-1-3840',
      edition: 'ENTERPRISE',
      activationPolicy: 'ALWAYS',
      availabilityType: 'ZONAL',
      dataDiskSizeGb: '10',
      dataApiAccess: 'ALLOW_DATA_API',
      databaseFlags: [{ name: 'work_mem', value: '8192' }],
      userLabels: { team: 'db' },
      ipConfiguration: { ipv4Enabled: true, sslMode: 'ALLOW_UNENCRYPTED_AND_ENCRYPTED' },
    },
    ...over,
  }
}

/** state is the fake sqladmin API's resources. */
let state: {
  instances: DatabaseInstance[]
  databases: Database[]
  users: User[]
  backups: BackupRun[]
  sql?: ExecuteSqlResponse
}

/** requests records the writes the fake sqladmin API received. */
let requests: { method: string; path: string; body: unknown }[] = []

async function record(request: Request) {
  const text = await request.text()
  const url = new URL(request.url)
  requests.push({
    method: request.method,
    path: url.pathname + url.search,
    body: text ? (JSON.parse(text) as unknown) : undefined,
  })
  return requests.at(-1)!.body as Record<string, unknown>
}

const op = (type: string) => ({ name: `op-${type}`, operationType: type, status: 'DONE' })

beforeEach(() => {
  requests = []
  state = {
    instances: [
      instance(),
      instance({
        name: 'old',
        region: 'europe-west1',
        connectionName: `${P}:europe-west1:old`,
        settings: { ...instance().settings, activationPolicy: 'NEVER' },
      }),
    ],
    databases: [{ name: 'postgres', charset: 'UTF8', collation: 'en_US.UTF8' }],
    users: [
      { name: 'postgres' },
      { name: 'sa@alpha-project.iam', type: 'CLOUD_IAM_SERVICE_ACCOUNT' },
    ],
    backups: [],
  }
  fake.info.services = ['iam', 'sql', 'secrets']
  fake.env = { GCPEMU_SQL_MAIN: '127.0.0.1:54321' }
  const write = (type: string, fn?: (b: Record<string, unknown>) => void) =>
    (async ({ request }: { request: Request }) => {
      const body = await record(request)
      fn?.(body)
      return HttpResponse.json(op(type))
    }) as never
  server.use(
    http.get(`${API}/instances`, () => HttpResponse.json({ items: state.instances })),
    http.get(`${API}/instances/:name`, ({ params }) => {
      const i = state.instances.find((x) => x.name === params.name)
      return i
        ? HttpResponse.json(i)
        : HttpResponse.json(
            { error: { code: 404, message: 'The Cloud SQL instance does not exist.' } },
            { status: 404 },
          )
    }),
    http.get('*/sqladmin/v1/flags', () =>
      HttpResponse.json({
        items: [
          {
            name: 'work_mem',
            type: 'INTEGER',
            minValue: '64',
            maxValue: '2147483647',
            appliesTo: ['POSTGRES_17', 'POSTGRES_18'],
          },
          {
            name: 'max_connections',
            type: 'INTEGER',
            minValue: '14',
            maxValue: '262143',
            requiresRestart: true,
            appliesTo: ['POSTGRES_17', 'POSTGRES_18'],
          },
          { name: 'log_connections', type: 'BOOLEAN', appliesTo: ['POSTGRES_17'] },
        ],
      }),
    ),
    http.get(`${API}/instances/main/databases`, () =>
      HttpResponse.json({ items: state.databases }),
    ),
    http.get(`${API}/instances/main/users`, () => HttpResponse.json({ items: state.users })),
    http.get(`${API}/instances/main/backupRuns`, () => HttpResponse.json({ items: state.backups })),
    http.get(`${API}/operations/:op`, ({ params }) =>
      HttpResponse.json({ ...op('X'), name: params.op }),
    ),
    http.post(`${API}/instances`, async ({ request }) => {
      const b = await record(request)
      if (b.name === 'taken') {
        return HttpResponse.json(
          {
            error: {
              code: 409,
              status: 'ALREADY_EXISTS',
              message: 'The Cloud SQL instance already exists.',
            },
          },
          { status: 409 },
        )
      }
      return HttpResponse.json(op('CREATE'))
    }),
    http.patch(`${API}/instances/main`, write('UPDATE')),
    http.delete(`${API}/instances/main`, write('DELETE')),
    http.post(`${API}/instances/main/restart`, write('RESTART')),
    http.post(
      `${API}/instances/main/databases`,
      write('CREATE_DATABASE', (b) => state.databases.push(b as unknown as Database)),
    ),
    http.delete(`${API}/instances/main/databases/:db`, write('DELETE_DATABASE')),
    http.post(`${API}/instances/main/users`, write('CREATE_USER')),
    http.put(`${API}/instances/main/users`, write('UPDATE_USER')),
    http.post(`${API}/instances/main/backupRuns`, write('BACKUP_VOLUME')),
    http.post(`${API}/instances/main/restoreBackup`, write('RESTORE_VOLUME')),
    http.post(`${API}/instances/main/executeSql`, async ({ request }) => {
      await record(request)
      return HttpResponse.json(state.sql ?? {})
    }),
  )
})

it('says how to enable Cloud SQL when it is off', async () => {
  fake.info.services = ['iam']
  renderApp(`/sql?project=${P}`)
  expect(await screen.findByRole('heading', { name: /Service sql is not enabled/ })).toBeVisible()
})

it('asks for a Project first', async () => {
  renderApp('/sql')
  expect(await screen.findByText(/Choose a Project/)).toBeInTheDocument()
})

it('lists instances, filtering by region and name in the URL', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/sql?project=${P}`)
  await screen.findByRole('table', { name: 'Instances' })
  const rows = () => screen.getAllByTestId('instance')
  expect(rows()).toHaveLength(2)
  expect(within(rows()[1]!).getByText('STOPPED')).toBeVisible()
  expect(within(rows()[0]!).getByText(`${P}:us-central1:main`)).toBeVisible()
  await user.type(screen.getByLabelText('Region'), 'europe-west1')
  expect(rows()).toHaveLength(1)
  expect(router.state.location.search).toContain('location=europe-west1')
  await user.clear(screen.getByLabelText('Region'))
  await user.type(screen.getByLabelText('Filter'), 'mai')
  expect(rows()).toHaveLength(1)
  expect(screen.getByRole('link', { name: 'main' })).toBeVisible()
})

it('creates an instance, showing the API’s validation messages', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`/sql/create?project=${P}`)
  const name = await screen.findByLabelText('Instance ID')
  await user.type(name, 'Bad_Name')
  await user.type(screen.getByLabelText('Database flags'), 'work_mem=1\nnope=on')
  await user.type(screen.getByLabelText('Authorized networks'), '10.0.0.0/33')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(await screen.findByText(/Invalid instance name \(Bad_Name\)/)).toBeVisible()
  expect(screen.getByText('Invalid request: Invalid value for flag work_mem: "1".')).toBeVisible()
  expect(
    screen.getByText('Invalid request: Invalid authorized network (10.0.0.0/33).'),
  ).toBeVisible()
  expect(requests).toHaveLength(0)

  // A flag the chosen version does not have.
  await user.selectOptions(screen.getByLabelText('Database version'), 'POSTGRES_18')
  await user.clear(screen.getByLabelText('Database flags'))
  await user.type(screen.getByLabelText('Database flags'), 'log_connections=on')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(
    await screen.findByText('Invalid request: Invalid flag name: log_connections.'),
  ).toBeVisible()

  await user.clear(name)
  await user.type(name, 'taken')
  await user.clear(screen.getByLabelText('Database flags'))
  await user.clear(screen.getByLabelText('Authorized networks'))
  await user.click(screen.getByRole('button', { name: 'Create' }))
  expect(await screen.findByText('The Cloud SQL instance already exists.')).toBeVisible()

  await user.clear(name)
  await user.type(name, 'api')
  await user.type(screen.getByLabelText('Password of the postgres user'), 'pw')
  await user.type(screen.getByLabelText('Database flags'), 'max_connections=50')
  await user.type(screen.getByLabelText('Authorized networks'), 'office=203.0.113.0/24')
  await user.click(screen.getByLabelText(/Data API access/))
  await user.type(screen.getByLabelText('Labels'), 'env=dev')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() => expect(router.state.location.pathname).toBe('/sql/instances/api'))
  expect(requests.at(-1)).toEqual({
    method: 'POST',
    path: `/sqladmin/v1/projects/${P}/instances`,
    body: {
      name: 'api',
      databaseVersion: 'POSTGRES_18',
      region: 'us-central1',
      rootPassword: 'pw',
      settings: {
        availabilityType: 'ZONAL',
        dataDiskSizeGb: '10',
        ipConfiguration: {
          ipv4Enabled: true,
          authorizedNetworks: [{ name: 'office', value: '203.0.113.0/24' }],
          sslMode: 'ALLOW_UNENCRYPTED_AND_ENCRYPTED',
        },
        dataApiAccess: 'ALLOW_DATA_API',
        deletionProtectionEnabled: false,
        databaseFlags: [{ name: 'max_connections', value: '50' }],
        userLabels: { env: 'dev' },
      },
    },
  })
})

it('shows an instance’s connection name and strings', async () => {
  renderApp(`${base}?project=${P}`)
  expect(await screen.findByText(`${P}:us-central1:main`)).toBeVisible()
  expect(
    await screen.findByText('psql "host=127.0.0.1 port=54321 user=postgres dbname=postgres"'),
  ).toBeVisible()
  expect(
    screen.getByText('psql "host=172.30.0.4 port=5432 user=postgres dbname=postgres"'),
  ).toBeVisible()
  expect(
    screen.getByText(
      `cloud-sql-proxy --sqladmin-api-endpoint ${location.origin}/ ${P}:us-central1:main`,
    ),
  ).toBeVisible()
  const details = screen.getByLabelText('Instance details')
  expect(details).toHaveTextContent('work_mem=8192')
  expect(details).toHaveTextContent('team=db')
})

it('stops, starts and restarts an instance', async () => {
  const user = userEvent.setup()
  renderApp(`${base}?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Stop' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Stop instance' }),
  )
  await waitFor(() => expect(requests).toHaveLength(1))
  await user.click(screen.getByRole('button', { name: 'Restart' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Restart instance' }),
  )
  await waitFor(() => expect(requests).toHaveLength(2))
  expect(requests).toEqual([
    {
      method: 'PATCH',
      path: `/sqladmin/v1/projects/${P}/instances/main`,
      body: { settings: { activationPolicy: 'NEVER' } },
    },
    { method: 'POST', path: `/sqladmin/v1/projects/${P}/instances/main/restart`, body: undefined },
  ])

  state.instances[0] = instance({ settings: { ...instance().settings, activationPolicy: 'NEVER' } })
  requests = []
  renderApp(`${base}?project=${P}`)
  await user.click((await screen.findAllByRole('button', { name: 'Start' })).at(-1)!)
  await waitFor(() => expect(requests).toHaveLength(1))
  expect(requests[0]!.body).toEqual({ settings: { activationPolicy: 'ALWAYS' } })
})

it('edits an instance with a patch of what changed', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`${base}/edit?project=${P}`)
  const labels = await screen.findByLabelText('Labels')
  expect(labels).toHaveValue('team=db')
  await user.clear(labels)
  await user.type(labels, 'env=prod')
  const flags = screen.getByLabelText('Database flags')
  expect(flags).toHaveValue('work_mem=8192')
  await user.type(flags, '\nmax_connections=40')
  await user.click(screen.getByLabelText('Deletion protection'))
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(router.state.location.pathname).toBe(base))
  expect(requests).toEqual([
    {
      method: 'PATCH',
      path: `/sqladmin/v1/projects/${P}/instances/main`,
      body: {
        settings: {
          deletionProtectionEnabled: true,
          databaseFlags: [
            { name: 'work_mem', value: '8192' },
            { name: 'max_connections', value: '40' },
          ],
          userLabels: { env: 'prod', team: null },
        },
      },
    },
  ])
})

it('creates and deletes databases', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`${base}/databases?project=${P}`)
  await user.click(await screen.findByRole('link', { name: 'Create database' }))
  await user.type(await screen.findByLabelText('Name'), 'app')
  await user.click(screen.getByRole('button', { name: 'Create' }))
  await waitFor(() => expect(router.state.location.pathname).toBe(`${base}/databases`))
  expect(requests[0]).toEqual({
    method: 'POST',
    path: `/sqladmin/v1/projects/${P}/instances/main/databases`,
    body: { name: 'app' },
  })
  await user.click(await screen.findByRole('button', { name: 'Delete database app' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete database' }),
  )
  await waitFor(() => expect(requests).toHaveLength(2))
  expect(requests[1]).toMatchObject({
    method: 'DELETE',
    path: `/sqladmin/v1/projects/${P}/instances/main/databases/app`,
  })
})

it('adds users with the API’s messages and changes a password', async () => {
  const user = userEvent.setup()
  const { router } = renderApp(`${base}/users?project=${P}`)
  expect(await screen.findByText('IAM service account')).toBeVisible()
  // IAM users have no password to edit.
  expect(screen.queryByRole('link', { name: 'Edit user sa@alpha-project.iam' })).toBeNull()

  await user.click(screen.getByRole('link', { name: 'Add user' }))
  const name = await screen.findByLabelText('Name')
  await user.type(name, 'cloudsqlx')
  await user.click(screen.getByRole('button', { name: 'Add' }))
  expect(
    await screen.findByText('Invalid request: User name (cloudsqlx) is reserved.'),
  ).toBeVisible()
  await user.selectOptions(screen.getByLabelText('Type'), 'CLOUD_IAM_USER')
  await user.click(screen.getByRole('button', { name: 'Add' }))
  expect(
    await screen.findByText('Invalid request: IAM user name (cloudsqlx) must be an email address.'),
  ).toBeVisible()
  expect(screen.queryByLabelText('Password')).toBeNull()
  await user.selectOptions(screen.getByLabelText('Type'), 'BUILT_IN')
  await user.clear(name)
  await user.type(name, 'app')
  await user.type(screen.getByLabelText('Password'), 'secret')
  await user.click(screen.getByRole('button', { name: 'Add' }))
  await waitFor(() => expect(router.state.location.pathname).toBe(`${base}/users`))
  expect(requests[0]!.body).toEqual({ name: 'app', type: 'BUILT_IN', password: 'secret' })

  await user.click(await screen.findByRole('link', { name: 'Edit user postgres' }))
  await user.type(await screen.findByLabelText('New password'), 'n3w')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(requests).toHaveLength(2))
  expect(requests[1]).toEqual({
    method: 'PUT',
    path: `/sqladmin/v1/projects/${P}/instances/main/users?name=postgres`,
    body: { name: 'postgres', password: 'n3w' },
  })
})

it('offers to allow the Data API before running queries', async () => {
  const user = userEvent.setup()
  state.instances[0] = instance({ settings: { ...instance().settings, dataApiAccess: undefined } })
  renderApp(`${base}/query?project=${P}`)
  await user.click(await screen.findByRole('button', { name: 'Allow Data API access' }))
  await waitFor(() => expect(requests).toHaveLength(1))
  expect(requests[0]!.body).toEqual({ settings: { dataApiAccess: 'ALLOW_DATA_API' } })
})

it('runs queries read-only by default and shows each statement’s rows', async () => {
  const user = userEvent.setup()
  state.sql = {
    results: [
      { message: 'BEGIN' },
      {
        message: 'SELECT 2',
        columns: [
          { name: 'id', type: 'INT4' },
          { name: 'name', type: 'TEXT' },
        ],
        rows: [
          { values: [{ value: '1' }, { value: 'one' }] },
          { values: [{ value: '2' }, { nullValue: true }] },
        ],
      },
      { message: 'COMMIT' },
    ],
    metadata: { sqlStatementExecutionTime: '0.012s' },
  }
  renderApp(`${base}/query?project=${P}`)
  const sql = await screen.findByLabelText('SQL')
  await user.clear(sql)
  await user.type(sql, 'SELECT * FROM items')
  await user.selectOptions(screen.getByLabelText('User'), 'sa@alpha-project.iam')
  await user.click(screen.getByRole('button', { name: 'Run' }))
  const table = await screen.findByRole('table', { name: 'Result 1' })
  expect(screen.getAllByTestId('result')).toHaveLength(1)
  expect(table).toHaveTextContent('INT4')
  expect(within(table).getAllByRole('row')[2]).toHaveTextContent('NULL')
  expect(screen.getByText('Ran in 0.012s.')).toBeVisible()
  expect(requests[0]!.body).toEqual({
    user: 'sa@alpha-project.iam',
    database: 'postgres',
    sqlStatement: 'BEGIN READ ONLY;\nSELECT * FROM items\n;\nCOMMIT;',
    rowLimit: '1000',
    partialResultMode: 'ALLOW_PARTIAL_RESULT',
    application: 'gcpemu-console',
  })

  // Unticked, the statements run as written; an error stops them.
  state.sql = {
    status: { code: 3, message: 'ERROR: relation "nope" does not exist (SQLSTATE 42P01)' },
  }
  await user.click(screen.getByLabelText('Read-only'))
  expect(screen.getByText('Statements can change data.')).toBeVisible()
  await user.clear(sql)
  await user.type(sql, 'DELETE FROM nope{Control>}{Enter}{/Control}')
  expect(await screen.findByText(/relation "nope" does not exist/)).toBeVisible()
  expect(requests[1]!.body).toMatchObject({ sqlStatement: 'DELETE FROM nope' })
})

it('takes, restores and lists backups', async () => {
  const user = userEvent.setup()
  state.backups = [
    {
      id: '1700000000000',
      status: 'SUCCESSFUL',
      type: 'ON_DEMAND',
      description: 'nightly',
      endTime: '2026-10-08T12:00:00Z',
    },
  ]
  renderApp(`${base}/backups?project=${P}`)
  const row = await screen.findByTestId('backup')
  expect(row).toHaveTextContent('SUCCESSFUL')
  await user.click(screen.getByRole('button', { name: 'Create backup' }))
  const dialog = await screen.findByRole('dialog')
  await user.type(within(dialog).getByLabelText('Description'), 'manual')
  await user.click(within(dialog).getByRole('button', { name: 'Create' }))
  await waitFor(() => expect(requests).toHaveLength(1))
  expect(requests[0]!.body).toEqual({ description: 'manual' })

  await user.click(screen.getByRole('button', { name: 'Restore backup 1700000000000' }))
  await user.click(
    within(await screen.findByRole('dialog')).getByRole('button', { name: 'Restore' }),
  )
  await waitFor(() => expect(requests).toHaveLength(2))
  expect(requests[1]).toEqual({
    method: 'POST',
    path: `/sqladmin/v1/projects/${P}/instances/main/restoreBackup`,
    body: {
      restoreBackupContext: { backupRunId: '1700000000000', instanceId: 'main', project: P },
    },
  })
})
