import { block, gcloud, keyValues, omit, resource, type Recipe } from '@/lib/copyAs'

import type { Database, DatabaseInstance, InstanceRef, User } from './api'

// Copy as (FR-UI-015) for instances, databases and users: gcloud sql, the
// sqladmin v1 insert request and the google_sql_* resource.

const API = '/sqladmin/v1'

/** lower writes an enum value as gcloud takes it: ENTERPRISE_PLUS is enterprise-plus. */
const lower = (v?: string) => v?.toLowerCase().replaceAll('_', '-')

/** INSTANCE_OUTPUT are an instance's output-only fields. */
const INSTANCE_OUTPUT = [
  'state',
  'connectionName',
  'ipAddresses',
  'serviceAccountEmailAddress',
  'createTime',
  'selfLink',
  'etag',
  'kind',
  'gceZone',
  'databaseInstalledVersion',
]

export function instanceRecipe(i: DatabaseInstance, project = i.project ?? ''): Recipe {
  const st = i.settings ?? {}
  const ip = st.ipConfiguration
  const backup = st.backupConfiguration
  const activation = st.activationPolicy !== 'ALWAYS' ? st.activationPolicy : undefined
  const dataApi = st.dataApiAccess === 'ALLOW_DATA_API' ? st.dataApiAccess : undefined
  return {
    gcloud: gcloud(['sql', 'instances', 'create', i.name], {
      project,
      'database-version': i.databaseVersion,
      tier: st.tier,
      edition: lower(st.edition),
      region: i.region,
      'availability-type': lower(st.availabilityType),
      // gcloud names the disk types without the PD_ prefix.
      'storage-type': st.dataDiskType?.replace(/^PD_/, ''),
      'storage-size': st.dataDiskSizeGb ? `${st.dataDiskSizeGb}GB` : undefined,
      'activation-policy': lower(activation),
      'database-flags': st.databaseFlags?.map((f) => `${f.name}=${f.value}`),
      labels: keyValues(st.userLabels),
      'no-assign-ip': ip?.ipv4Enabled === false,
      network: ip?.privateNetwork,
      'authorized-networks': ip?.authorizedNetworks?.map((a) => a.value),
      'ssl-mode': ip?.sslMode,
      // gcloud turns daily backups on unless told otherwise.
      'backup-start-time': backup?.enabled ? backup.startTime : undefined,
      'no-backup': !backup?.enabled,
      'deletion-protection': st.deletionProtectionEnabled,
      'data-api-access': dataApi,
    }),
    rest: {
      method: 'POST',
      path: `${API}/projects/${project}/instances`,
      body: {
        ...omit(i, INSTANCE_OUTPUT),
        settings: i.settings ? omit(i.settings, ['settingsVersion']) : undefined,
      },
    },
    tofu: resource('google_sql_database_instance', i.name, {
      project,
      name: i.name,
      database_version: i.databaseVersion,
      region: i.region,
      settings: block({
        tier: st.tier,
        edition: st.edition,
        availability_type: st.availabilityType,
        activation_policy: activation,
        disk_size: st.dataDiskSizeGb ? Number(st.dataDiskSizeGb) : undefined,
        disk_type: st.dataDiskType,
        user_labels: st.userLabels,
        deletion_protection_enabled: st.deletionProtectionEnabled || undefined,
        data_api_access: dataApi,
        ip_configuration: ip
          ? block({
              ipv4_enabled: ip.ipv4Enabled,
              private_network: ip.privateNetwork,
              ssl_mode: ip.sslMode,
              authorized_networks: (ip.authorizedNetworks ?? []).map((a) =>
                block({ name: a.name, value: a.value, expiration_time: a.expirationTime }),
              ),
            })
          : undefined,
        database_flags: (st.databaseFlags ?? []).map((f) =>
          block({ name: f.name, value: f.value }),
        ),
        backup_configuration: backup
          ? block({ enabled: backup.enabled ?? false, start_time: backup.startTime })
          : undefined,
      }),
    }),
  }
}

export function databaseRecipe(r: InstanceRef, d: Database): Recipe {
  return {
    gcloud: gcloud(['sql', 'databases', 'create', d.name], {
      project: r.project,
      instance: r.instance,
      charset: d.charset,
      collation: d.collation,
    }),
    rest: {
      method: 'POST',
      path: `${API}/projects/${r.project}/instances/${r.instance}/databases`,
      body: omit(d, ['selfLink', 'etag', 'kind']),
    },
    tofu: resource('google_sql_database', d.name, {
      project: r.project,
      name: d.name,
      instance: r.instance,
      charset: d.charset,
      collation: d.collation,
    }),
  }
}

/** NO_PASSWORD heads a built-in user's recipe: the API never returns a password. */
const NO_PASSWORD = 'The API does not return passwords: add one to log in with it.'

/**
 * userRecipe leaves a built-in user's password out, as the API never
 * returns it; a comment says to add one.
 */
export function userRecipe(r: InstanceRef, u: User): Recipe {
  const type = u.type && u.type !== 'BUILT_IN' ? u.type : undefined
  const note = (prefix: string) => (type ? '' : `${prefix} ${NO_PASSWORD}\n`)
  return {
    gcloud:
      note('#') +
      gcloud(['sql', 'users', 'create', u.name], {
        project: r.project,
        instance: r.instance,
        host: u.host,
        type: type?.toLowerCase(),
      }),
    rest: {
      method: 'POST',
      path: `${API}/projects/${r.project}/instances/${r.instance}/users`,
      body: omit(u, ['etag', 'kind']),
    },
    tofu:
      note('#') +
      resource('google_sql_user', u.name, {
        project: r.project,
        name: u.name,
        instance: r.instance,
        host: u.host,
        type,
      }),
  }
}
