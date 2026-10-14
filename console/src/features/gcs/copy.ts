import {
  block,
  gcloud,
  keyValues,
  omit,
  resource,
  type FlagValue,
  type HclBody,
  type Recipe,
  type RestCall,
} from '@/lib/copyAs'

import {
  bucketPath,
  objectPath,
  shortTopic,
  type Bucket,
  type Notification,
  type StorageObject,
} from './api'
import { baseName } from './format'

// Copy as (FR-UI-015) for buckets, their notification configs and
// objects: gcloud storage, the storage/v1 insert (or upload) request and
// the google_storage_* resource.

/** hasFlags reports whether gcloud would write any of flags. */
const hasFlags = (flags: Record<string, FlagValue>) =>
  Object.values(flags).some(
    (v) =>
      v !== undefined && v !== null && v !== false && v !== '' && !(Array.isArray(v) && !v.length),
  )

/** seconds writes an API seconds count as a gcloud duration. */
const seconds = (s?: string) => (s === undefined ? undefined : `${s}s`)

/** num is an API int64 string as an HCL number. */
const num = (s?: string) => (s === undefined ? undefined : Number(s))

/** heredoc writes value as JSON to file with a shell here-document. */
const heredoc = (file: string, value: unknown) =>
  `cat > ${file} <<'EOF'\n${JSON.stringify(value, null, 2)}\nEOF`

interface Cors {
  origin?: string[]
  method?: string[]
  responseHeader?: string[]
  maxAgeSeconds?: number
}

interface LifecycleRule {
  action?: { type?: string; storageClass?: string }
  condition?: {
    age?: number
    createdBefore?: string
    customTimeBefore?: string
    daysSinceCustomTime?: number
    daysSinceNoncurrentTime?: number
    isLive?: boolean
    matchesPrefix?: string[]
    matchesStorageClass?: string[]
    matchesSuffix?: string[]
    noncurrentTimeBefore?: string
    numNewerVersions?: number
  }
}

/** lifecycleRule is one JSON API lifecycle rule as a lifecycle_rule block. */
function lifecycleRule(r: LifecycleRule) {
  const c = r.condition ?? {}
  return block({
    condition: block({
      age: c.age,
      created_before: c.createdBefore,
      custom_time_before: c.customTimeBefore,
      days_since_custom_time: c.daysSinceCustomTime,
      days_since_noncurrent_time: c.daysSinceNoncurrentTime,
      with_state: c.isLive === undefined ? undefined : c.isLive ? 'LIVE' : 'ARCHIVED',
      matches_prefix: c.matchesPrefix,
      matches_storage_class: c.matchesStorageClass,
      matches_suffix: c.matchesSuffix,
      noncurrent_time_before: c.noncurrentTimeBefore,
      num_newer_versions: c.numNewerVersions,
    }),
    action: block({ type: r.action?.type, storage_class: r.action?.storageClass }),
  })
}

/** BUCKET_OUTPUT are a bucket's output-only fields. */
const BUCKET_OUTPUT = [
  'kind',
  'id',
  'selfLink',
  'projectNumber',
  'timeCreated',
  'updated',
  'metageneration',
  'etag',
  'locationType',
  'generation',
  'softDeleteTime',
  'hardDeleteTime',
  'satisfiesPZS',
  'satisfiesPZI',
]

/** bucketBody is the bucket as an insert body: no output-only fields, nested ones included. */
function bucketBody(b: Bucket) {
  const out: Record<string, unknown> = omit(b, BUCKET_OUTPUT)
  if (b.retentionPolicy) {
    out.retentionPolicy = omit(b.retentionPolicy, ['effectiveTime', 'isLocked'])
  }
  if (b.softDeletePolicy) out.softDeletePolicy = omit(b.softDeletePolicy, ['effectiveTime'])
  const ubla = b.iamConfiguration?.uniformBucketLevelAccess
  if (ubla) {
    out.iamConfiguration = {
      ...b.iamConfiguration,
      uniformBucketLevelAccess: omit(ubla, ['lockedTime']),
    }
  }
  return out
}

/**
 * bucketRecipe recreates a bucket in project. gcloud storage buckets
 * create takes only some settings; the rest follow as an update, with
 * the CORS and lifecycle configurations written to files first.
 */
export function bucketRecipe(b: Bucket, project: string): Recipe {
  const url = `gs://${b.name}`
  const cors = (b.cors ?? []) as Cors[]
  const rules = (b.lifecycle?.rule ?? []) as LifecycleRule[]
  const retention = b.retentionPolicy
  const update = {
    versioning: b.versioning?.enabled,
    'default-event-based-hold': b.defaultEventBasedHold,
    'update-labels': keyValues(b.labels),
    'web-main-page-suffix': b.website?.mainPageSuffix,
    'web-error-page': b.website?.notFoundPage,
    'cors-file': cors.length ? 'cors.json' : undefined,
  }
  const commands = [
    ...(cors.length ? [heredoc('cors.json', cors)] : []),
    ...(rules.length ? [heredoc('lifecycle.json', { rule: rules })] : []),
    gcloud(['storage', 'buckets', 'create', url], {
      project,
      location: b.location,
      'default-storage-class': b.storageClass,
      'uniform-bucket-level-access': b.iamConfiguration?.uniformBucketLevelAccess?.enabled,
      'public-access-prevention': b.iamConfiguration?.publicAccessPrevention === 'enforced',
      'retention-period': seconds(retention?.retentionPeriod),
      'soft-delete-duration': seconds(b.softDeletePolicy?.retentionDurationSeconds),
      'recovery-point-objective': b.rpo && b.rpo !== 'DEFAULT' ? b.rpo : undefined,
      'lifecycle-file': rules.length ? 'lifecycle.json' : undefined,
    }),
    ...(hasFlags(update) ? [gcloud(['storage', 'buckets', 'update', url], update)] : []),
    ...(retention?.isLocked
      ? [gcloud(['storage', 'buckets', 'update', url], { 'lock-retention-period': true })]
      : []),
  ]
  const tofu: HclBody = {
    project,
    name: b.name,
    location: b.location,
    storage_class: b.storageClass,
    labels: b.labels,
    uniform_bucket_level_access: b.iamConfiguration?.uniformBucketLevelAccess?.enabled,
    public_access_prevention: b.iamConfiguration?.publicAccessPrevention,
    default_event_based_hold: b.defaultEventBasedHold,
    rpo: b.rpo,
    versioning: b.versioning ? block({ enabled: !!b.versioning.enabled }) : undefined,
    website:
      b.website?.mainPageSuffix || b.website?.notFoundPage
        ? block({
            main_page_suffix: b.website.mainPageSuffix,
            not_found_page: b.website.notFoundPage,
          })
        : undefined,
    cors: cors.map((c) =>
      block({
        origin: c.origin,
        method: c.method,
        response_header: c.responseHeader,
        max_age_seconds: c.maxAgeSeconds,
      }),
    ),
    lifecycle_rule: rules.map(lifecycleRule),
    retention_policy: retention?.retentionPeriod
      ? block({
          retention_period: num(retention.retentionPeriod),
          is_locked: retention.isLocked,
        })
      : undefined,
    soft_delete_policy: b.softDeletePolicy
      ? block({
          retention_duration_seconds: num(b.softDeletePolicy.retentionDurationSeconds),
        })
      : undefined,
  }
  return {
    gcloud: commands,
    rest: {
      method: 'POST',
      path: `/storage/v1/b?${new URLSearchParams({ project })}`,
      body: bucketBody(b),
    },
    tofu: resource('google_storage_bucket', b.name, tofu),
  }
}

const PAYLOAD_FORMAT: Record<string, string> = { JSON_API_V1: 'json', NONE: 'none' }

export function notificationRecipe(bucket: string, n: Notification): Recipe {
  const topic = shortTopic(n.topic)
  return {
    gcloud: gcloud(['storage', 'buckets', 'notifications', 'create', `gs://${bucket}`], {
      topic,
      'event-types': n.event_types,
      'payload-format': n.payload_format && PAYLOAD_FORMAT[n.payload_format],
      'object-prefix': n.object_name_prefix,
      'custom-attributes': keyValues(n.custom_attributes),
    }),
    rest: {
      method: 'POST',
      path: `${bucketPath(bucket)}/notificationConfigs`,
      body: omit(n, ['kind', 'id', 'etag', 'selfLink']),
    },
    tofu: resource('google_storage_notification', `${bucket}-${n.id}`, {
      bucket,
      topic,
      payload_format: n.payload_format,
      event_types: n.event_types,
      object_name_prefix: n.object_name_prefix,
      custom_attributes: n.custom_attributes,
    }),
  }
}

/**
 * objectRecipe uploads a local file named after the object's base name.
 * The media upload sets only the content type; the object's other
 * metadata follows as a patch.
 */
export function objectRecipe(o: StorageObject): Recipe {
  const file = `./${baseName(o.name)}`
  const patch = {
    contentEncoding: o.contentEncoding,
    contentDisposition: o.contentDisposition,
    contentLanguage: o.contentLanguage,
    cacheControl: o.cacheControl,
    metadata: o.metadata,
    temporaryHold: o.temporaryHold || undefined,
    eventBasedHold: o.eventBasedHold || undefined,
  }
  const patched = Object.values(patch).some((v) => v !== undefined)
  const rest: RestCall[] = [
    {
      method: 'POST',
      path: `/upload${bucketPath(o.bucket)}/o?${new URLSearchParams({ uploadType: 'media', name: o.name })}`,
      file: { path: file, contentType: o.contentType },
    },
    ...(patched
      ? [{ method: 'PATCH' as const, path: objectPath(o.bucket, o.name), body: patch }]
      : []),
  ]
  return {
    gcloud: gcloud(['storage', 'cp', file, `gs://${o.bucket}/${o.name}`], {
      'content-type': o.contentType,
      'content-encoding': o.contentEncoding,
      'content-disposition': o.contentDisposition,
      'content-language': o.contentLanguage,
      'cache-control': o.cacheControl,
      'custom-metadata': keyValues(o.metadata),
      'storage-class': o.storageClass,
    }),
    rest,
    tofu: resource('google_storage_bucket_object', `${o.bucket}-${o.name}`, {
      bucket: o.bucket,
      name: o.name,
      source: file,
      content_type: o.contentType,
      content_encoding: o.contentEncoding,
      content_disposition: o.contentDisposition,
      content_language: o.contentLanguage,
      cache_control: o.cacheControl,
      metadata: o.metadata,
      storage_class: o.storageClass,
      temporary_hold: o.temporaryHold || undefined,
      event_based_hold: o.eventBasedHold || undefined,
    }),
  }
}
