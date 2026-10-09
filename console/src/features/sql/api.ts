import { queryOptions } from '@tanstack/react-query'

import { ApiError } from '@/api/errors'
import { gcpFetch } from '@/api/fetch'

// The Cloud SQL view's reads and writes, all through sqladmin v1:
// instances, databases, users, flags, backup runs, Operations and
// instances.executeSql (the Data API) for the query runner. Types cover
// the fields the view reads; the raw JSON editor covers the rest.

export type InstanceState =
  | 'SQL_INSTANCE_STATE_UNSPECIFIED'
  | 'RUNNABLE'
  | 'SUSPENDED'
  | 'PENDING_DELETE'
  | 'PENDING_CREATE'
  | 'MAINTENANCE'
  | 'FAILED'
  | 'STOPPED'

export interface DatabaseFlag {
  name: string
  value: string
}

export interface AclEntry {
  name?: string
  value: string
  expirationTime?: string
}

export interface Settings {
  tier?: string
  edition?: string
  /** ALWAYS, or NEVER for a stopped instance. */
  activationPolicy?: string
  availabilityType?: string
  dataDiskSizeGb?: string
  dataDiskType?: string
  dataApiAccess?: 'DATA_API_ACCESS_UNSPECIFIED' | 'DISALLOW_DATA_API' | 'ALLOW_DATA_API'
  deletionProtectionEnabled?: boolean
  databaseFlags?: DatabaseFlag[]
  userLabels?: Record<string, string>
  ipConfiguration?: {
    ipv4Enabled?: boolean
    privateNetwork?: string
    sslMode?: string
    authorizedNetworks?: AclEntry[]
  }
  backupConfiguration?: { enabled?: boolean; startTime?: string }
  settingsVersion?: string
}

export interface IpMapping {
  /** PRIMARY (public), PRIVATE or OUTGOING. */
  type: string
  ipAddress: string
}

export interface DatabaseInstance {
  name: string
  project?: string
  region?: string
  gceZone?: string
  databaseVersion?: string
  databaseInstalledVersion?: string
  state?: InstanceState
  connectionName?: string
  ipAddresses?: IpMapping[]
  serviceAccountEmailAddress?: string
  createTime?: string
  settings?: Settings
  selfLink?: string
  etag?: string
}

export interface Database {
  name: string
  charset?: string
  collation?: string
  instance?: string
  project?: string
  selfLink?: string
  etag?: string
}

export interface User {
  name: string
  host?: string
  /** Empty for BUILT_IN. */
  type?: string
  instance?: string
  project?: string
  etag?: string
}

export interface BackupRun {
  id: string
  description?: string
  status?: string
  type?: string
  backupKind?: string
  databaseVersion?: string
  enqueuedTime?: string
  startTime?: string
  endTime?: string
  location?: string
  error?: { message?: string }
}

export interface Flag {
  name: string
  type: string
  requiresRestart?: boolean
  appliesTo?: string[]
  minValue?: string
  maxValue?: string
  allowedStringValues?: string[]
}

/** SqlOperation is sqladmin's own Operation resource. */
export interface SqlOperation {
  name: string
  operationType?: string
  status?: 'SQL_OPERATION_STATUS_UNSPECIFIED' | 'PENDING' | 'RUNNING' | 'DONE'
  targetId?: string
  error?: { errors?: { code?: string; message?: string }[] }
}

export interface InstanceRef {
  project: string
  instance: string
}

// ---- instances.executeSql ----

export interface ExecuteSqlPayload {
  user?: string
  sqlStatement: string
  database?: string
  autoIamAuthn?: boolean
  rowLimit?: string
  partialResultMode?: 'FAIL_PARTIAL_RESULT' | 'ALLOW_PARTIAL_RESULT'
  application?: string
}

export interface QueryResult {
  columns?: { name: string; type: string }[]
  rows?: { values: { value?: string; nullValue?: boolean }[] }[]
  message?: string
  partialResult?: boolean
}

export interface ExecuteSqlResponse {
  messages?: { message: string; severity: string }[]
  metadata?: { sqlStatementExecutionTime?: string }
  results?: QueryResult[]
  status?: { code: number; message: string }
}

export const DATABASE_VERSIONS = [
  'POSTGRES_18',
  'POSTGRES_17',
  'POSTGRES_16',
  'POSTGRES_15',
  'POSTGRES_14',
] as const

/** NAME is Cloud SQL's instance name rule, with the API's message. */
export const NAME = /^[a-z]([-a-z0-9]*[a-z0-9])?$/
export const nameMessage = (name: string) =>
  `Invalid request: Invalid instance name (${name}): it must start with a letter and contain only lowercase letters, numbers and hyphens.`

const API = '/sqladmin/v1/'

export const instanceName = (r: InstanceRef) =>
  `projects/${r.project}/instances/${encodeURIComponent(r.instance)}`

/** isRunning reports whether an instance's PostgreSQL server is up. */
export const isRunning = (i?: DatabaseInstance) =>
  i?.state === 'RUNNABLE' && i.settings?.activationPolicy !== 'NEVER'

/** isStopped reports whether an instance was stopped (activationPolicy NEVER). */
export const isStopped = (i?: DatabaseInstance) => i?.settings?.activationPolicy === 'NEVER'

/** displayState is RUNNABLE, or STOPPED for an instance that is not activated. */
export function displayState(i: DatabaseInstance): string {
  if (isStopped(i) && i.state === 'RUNNABLE') return 'STOPPED'
  return i.state ?? 'SQL_INSTANCE_STATE_UNSPECIFIED'
}

export const ipOf = (i: DatabaseInstance | undefined, type: string) =>
  i?.ipAddresses?.find((a) => a.type === type)?.ipAddress

const send = <T>(path: string, method: string, body?: unknown) =>
  gcpFetch<T>(API + path, {
    method,
    body: body === undefined ? undefined : JSON.stringify(body),
  })

export const listInstances = async (project: string) =>
  (await gcpFetch<{ items?: DatabaseInstance[] }>(`${API}projects/${project}/instances`)).items ??
  []

export const getInstance = (r: InstanceRef) => gcpFetch<DatabaseInstance>(API + instanceName(r))

export const insertInstance = (project: string, body: unknown) =>
  send<SqlOperation>(`projects/${project}/instances`, 'POST', body)

export const patchInstance = (r: InstanceRef, body: unknown) =>
  send<SqlOperation>(instanceName(r), 'PATCH', body)

export const deleteInstance = (r: InstanceRef) => send<SqlOperation>(instanceName(r), 'DELETE')

export const restartInstance = (r: InstanceRef) =>
  send<SqlOperation>(`${instanceName(r)}/restart`, 'POST')

/** setActivation starts (ALWAYS) or stops (NEVER) an instance. */
export const setActivation = (r: InstanceRef, activationPolicy: 'ALWAYS' | 'NEVER') =>
  patchInstance(r, { settings: { activationPolicy } })

export const listDatabases = async (r: InstanceRef) =>
  (await gcpFetch<{ items?: Database[] }>(`${API}${instanceName(r)}/databases`)).items ?? []

export const insertDatabase = (r: InstanceRef, body: unknown) =>
  send<SqlOperation>(`${instanceName(r)}/databases`, 'POST', body)

export const patchDatabase = (r: InstanceRef, db: string, body: unknown) =>
  send<SqlOperation>(`${instanceName(r)}/databases/${encodeURIComponent(db)}`, 'PATCH', body)

export const deleteDatabase = (r: InstanceRef, db: string) =>
  send<SqlOperation>(`${instanceName(r)}/databases/${encodeURIComponent(db)}`, 'DELETE')

export const listUsers = async (r: InstanceRef) =>
  (await gcpFetch<{ items?: User[] }>(`${API}${instanceName(r)}/users`)).items ?? []

export const insertUser = (r: InstanceRef, body: unknown) =>
  send<SqlOperation>(`${instanceName(r)}/users`, 'POST', body)

const userQuery = (name: string) => `?${new URLSearchParams({ name })}`

export const updateUser = (r: InstanceRef, name: string, body: unknown) =>
  send<SqlOperation>(`${instanceName(r)}/users${userQuery(name)}`, 'PUT', body)

export const deleteUser = (r: InstanceRef, name: string) =>
  send<SqlOperation>(`${instanceName(r)}/users${userQuery(name)}`, 'DELETE')

export const listBackupRuns = async (r: InstanceRef) =>
  (await gcpFetch<{ items?: BackupRun[] }>(`${API}${instanceName(r)}/backupRuns`)).items ?? []

export const insertBackupRun = (r: InstanceRef, description: string) =>
  send<SqlOperation>(`${instanceName(r)}/backupRuns`, 'POST', description ? { description } : {})

export const deleteBackupRun = (r: InstanceRef, id: string) =>
  send<SqlOperation>(`${instanceName(r)}/backupRuns/${id}`, 'DELETE')

/** restoreBackup restores a backup run of instance from onto r. */
export const restoreBackup = (r: InstanceRef, backupRunId: string, instance: string) =>
  send<SqlOperation>(`${instanceName(r)}/restoreBackup`, 'POST', {
    restoreBackupContext: { backupRunId, instanceId: instance, project: r.project },
  })

export const executeSql = (r: InstanceRef, body: ExecuteSqlPayload) =>
  send<ExecuteSqlResponse>(`${instanceName(r)}/executeSql`, 'POST', body)

/** OP_POLL_MS is how often waitOperation checks an Operation. */
export const OP_POLL_MS = 500

/** opError is an Operation's first error message, if it failed. */
export const opError = (op: SqlOperation) => op.error?.errors?.find((e) => e.message)?.message

/**
 * waitOperation resolves once op is DONE, or rejects with its error. Forms
 * whose result the next page shows (a new database or user) wait for it.
 */
export async function waitOperation(project: string, op: SqlOperation) {
  let cur = op
  while (cur.status !== 'DONE') {
    await new Promise((r) => setTimeout(r, OP_POLL_MS))
    cur = await gcpFetch<SqlOperation>(`${API}projects/${project}/operations/${op.name}`)
  }
  const message = opError(cur)
  if (message) throw new ApiError(400, 'FAILED_PRECONDITION', message)
  return cur
}

// ---- queries ----

export const instancesQuery = (project: string) =>
  queryOptions({
    queryKey: ['sql', 'instances', project],
    queryFn: () => listInstances(project),
  })

export const instanceQuery = (r: InstanceRef) =>
  queryOptions({
    queryKey: ['sql', 'instance', r],
    queryFn: () => getInstance(r),
  })

export const databasesQuery = (r: InstanceRef, enabled = true) =>
  queryOptions({
    queryKey: ['sql', 'databases', r],
    queryFn: () => listDatabases(r),
    enabled,
  })

export const usersQuery = (r: InstanceRef, enabled = true) =>
  queryOptions({
    queryKey: ['sql', 'users', r],
    queryFn: () => listUsers(r),
    enabled,
  })

export const backupRunsQuery = (r: InstanceRef) =>
  queryOptions({
    queryKey: ['sql', 'backupRuns', r],
    queryFn: () => listBackupRuns(r),
  })

/** flagsQuery lists the database flags of every version (flags.list). */
export const flagsQuery = () =>
  queryOptions({
    queryKey: ['sql', 'flags'],
    queryFn: async () => (await gcpFetch<{ items?: Flag[] }>(`${API}flags`)).items ?? [],
    staleTime: Infinity,
  })
